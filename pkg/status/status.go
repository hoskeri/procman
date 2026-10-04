// Package status is the health vocabulary shared by procman processes,
// formations, and in-process sub-components.
//
// A component reports its state either by returning an error from a Status
// callback (pull) or by holding a Reporter (push).  Both produce the same
// thing: a *Error carrying a State, a human-readable reason, and structured
// extras.  A nil error means healthy.
//
// The State also answers the question a supervising system actually asks:
// Recoverable() reports whether the higher level system should keep waiting.
// Everything except StateFailed is recoverable, so a "flapping dependency"
// does not tear down a formation while a "permanent misconfiguration" does.
package status

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// State is the coarse health classification of a component.
type State uint8

const (
	// StateUnknown is the zero value: the component exists but has never
	// reported a status.  It carries a live pid (see Snapshot.PID).
	StateUnknown State = iota
	// StateUnknownProcess is "there is no live process here": no pid, or the
	// process exited and was reaped, and no status has been reported.  It is
	// the specialization of Unknown that makes a missing component
	// distinguishable from a silent one.
	StateUnknownProcess
	// StateStarting means "not ready yet, this is expected" (no cert, waiting
	// for a dependency).  Never fatal.
	StateStarting
	// StateHealthy means the component is serving.
	StateHealthy
	// StateDegraded means serving, but something is wrong (slow, retrying).
	// Never fatal.
	StateDegraded
	// StateUnhealthy means not serving right now, but a retry may fix it
	// (waiting for etcd to come back).  Recoverable.
	StateUnhealthy
	// StateFailed means the component is broken in a way procman should not
	// wait out (bad flags, missing PKI material, permanent config error).
	// Fatal.
	StateFailed
)

// String returns the lower-case wire/render name of the state: the value
// carried in the reserved "status" attribute.
func (s State) String() string {
	switch s {
	case StateUnknownProcess:
		return "unknown-process"
	case StateStarting:
		return "starting"
	case StateHealthy:
		return "healthy"
	case StateDegraded:
		return "degraded"
	case StateUnhealthy:
		return "unhealthy"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ParseState is the inverse of State.String; ok is false for unknown names.
func ParseState(name string) (State, bool) {
	switch name {
	case "unknown":
		return StateUnknown, true
	case "unknown-process":
		return StateUnknownProcess, true
	case "starting":
		return StateStarting, true
	case "healthy":
		return StateHealthy, true
	case "degraded":
		return StateDegraded, true
	case "unhealthy":
		return StateUnhealthy, true
	case "failed":
		return StateFailed, true
	}
	return StateUnknown, false
}

// Recoverable reports whether a supervising system should keep waiting when a
// component is in this state.  Everything except StateFailed is recoverable.
func (s State) Recoverable() bool { return s != StateFailed }

// Reported reports whether the state came from an actual report (as opposed
// to being inferred from process liveness).
func (s State) Reported() bool {
	switch s {
	case StateUnknown, StateUnknownProcess:
		return false
	}
	return true
}

// Level is the slog level a status record is logged at, so status output obeys
// the same --log-level policy as any other record.
func (s State) Level() slog.Level {
	switch s {
	case StateHealthy:
		return slog.LevelDebug
	case StateStarting:
		return slog.LevelInfo
	case StateDegraded:
		return slog.LevelWarn
	case StateUnhealthy, StateFailed:
		return slog.LevelError
	default:
		// Liveness-derived states have nothing to say; they live in the
		// registry, not in the log stream.
		return slog.LevelDebug
	}
}

// sentinel is a state-valued error usable with errors.Is.
type sentinel struct {
	name  string
	state State
}

func (s sentinel) Error() string { return "status: " + s.name }

// Sentinels for errors.Is comparisons against any *Error of a given state.
var (
	ErrUnknown        = sentinel{"unknown", StateUnknown}
	ErrUnknownProcess = sentinel{"unknown-process", StateUnknownProcess}
	ErrStarting       = sentinel{"starting", StateStarting}
	ErrHealthy        = sentinel{"healthy", StateHealthy}
	ErrDegraded       = sentinel{"degraded", StateDegraded}
	ErrUnhealthy      = sentinel{"unhealthy", StateUnhealthy}
	ErrFailed         = sentinel{"failed", StateFailed}
)

// Error is the canonical status error.  A nil *Error means healthy.
type Error struct {
	// State is the classification of the condition.
	State State
	// Reason is a short, operator-facing summary.
	Reason string
	// Detail is optional longer text.
	Detail string
	// Err is the wrapped cause, for errors.Is/As chains.
	Err error
	// Since is when the condition was first observed.  The zero value means
	// "now" at report time.
	Since time.Time
	// Attrs are structured extras carried alongside the status record.
	Attrs []slog.Attr
}

// Error renders the reason (plus detail) for the log line.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Reason
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if e.Detail != "" {
		if msg == "" {
			return e.Detail
		}
		return msg + ": " + e.Detail
	}
	return msg
}

// Unwrap exposes the wrapped cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is reports whether target is the sentinel for this error's state, so
// errors.Is(err, status.ErrFailed) matches any fatal status.
func (e *Error) Is(target error) bool {
	if e == nil {
		return target == nil
	}
	s, ok := target.(sentinel)
	return ok && s.state == e.State
}

// Recoverable reports whether a supervising system should keep waiting.
func (e *Error) Recoverable() bool {
	if e == nil {
		return true
	}
	return e.State.Recoverable()
}

// Fatal reports whether the status should bring the formation down.
func (e *Error) Fatal() bool { return e != nil && e.State == StateFailed }

// LogValue implements slog.LogValuer so a status can be attached as a single
// structured attribute.
func (e *Error) LogValue() slog.Value {
	if e == nil {
		return slog.Value{}
	}
	attrs := []slog.Attr{slog.String("state", e.State.String())}
	if e.Reason != "" {
		attrs = append(attrs, slog.String("reason", e.Reason))
	}
	if e.Detail != "" {
		attrs = append(attrs, slog.String("detail", e.Detail))
	}
	if !e.Since.IsZero() {
		attrs = append(attrs, slog.Duration("since", time.Since(e.Since).Round(time.Second)))
	}
	if e.Err != nil {
		attrs = append(attrs, slog.String("cause", e.Err.Error()))
	}
	attrs = append(attrs, e.Attrs...)
	return slog.GroupValue(attrs...)
}

// Healthy returns an *Error describing a healthy component (never nil, so it
// composes with FromError's return type).
func Healthy(format string, a ...any) *Error {
	return &Error{State: StateHealthy, Reason: fmt.Sprintf(format, a...)}
}

// Starting reports an expected not-ready-yet condition.
func Starting(format string, a ...any) *Error {
	return &Error{State: StateStarting, Reason: fmt.Sprintf(format, a...)}
}

// Degraded reports a component that still serves but is unhappy.
func Degraded(cause error, format string, a ...any) *Error {
	return &Error{State: StateDegraded, Reason: fmt.Sprintf(format, a...), Err: cause}
}

// Unhealthy reports a component that is not serving now but may recover (a
// retry may fix it).
func Unhealthy(cause error, format string, a ...any) *Error {
	return &Error{State: StateUnhealthy, Reason: fmt.Sprintf(format, a...), Err: cause}
}

// Failed reports a permanent condition: the supervising system should stop
// waiting.  Only this state may bring a formation down, and only when returned
// from a Formation's own Status callback.
func Failed(cause error, format string, a ...any) *Error {
	return &Error{State: StateFailed, Reason: fmt.Sprintf(format, a...), Err: cause}
}

// Wrap classifies err with the given state.  A nil err yields a nil *Error.
func Wrap(err error, st State, format string, a ...any) *Error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) && se.State == st {
		return se
	}
	return &Error{State: st, Reason: fmt.Sprintf(format, a...), Err: err}
}

// FromError classifies any error into the canonical form.  A nil err becomes a
// healthy status; a *Error passes through unchanged; anything else becomes a
// recoverable unhealthy status carrying the error text as the reason.
func FromError(err error) *Error {
	switch e := err.(type) {
	case nil:
		return &Error{State: StateHealthy}
	case *Error:
		if e == nil {
			return &Error{State: StateHealthy}
		}
		return e
	default:
		return &Error{State: StateUnhealthy, Reason: err.Error(), Err: err}
	}
}

// Snapshot is one component's latest known state, whether that state came from
// a status report or from the formation observing the unix process.
type Snapshot struct {
	// Identity is the full display path relative to the formation: a process
	// tag ("api", "api-2") or a component path ("webhook/validate").
	Identity string
	// State is the reported state, or Unknown / UnknownProcess.
	State State
	// Reason and Detail come from the report; they are empty for a
	// liveness-derived state.
	Reason string
	Detail string
	// Since is when this state began.  It is never zero for a snapshot the
	// registry produced: at worst it is the spawn time.
	Since time.Time
	// Recoverable is State.Recoverable().
	Recoverable bool

	// PID is the unix process id while the process is alive (0 otherwise).
	PID int
	// Exited reports whether the process is gone (reaped by the formation).
	Exited bool
	// ExitCode is the observed exit code, or -1 when unknown/signalled.
	ExitCode int
}

// Age is how long the current state has been held.
func (s Snapshot) Age() time.Duration {
	if s.Since.IsZero() {
		return 0
	}
	return time.Since(s.Since)
}

// String renders the snapshot the way the renderer does.
func (s Snapshot) String() string {
	b := &strings.Builder{}
	b.WriteString(strings.ToUpper(s.State.String()))
	if s.Reason != "" {
		b.WriteString(" ")
		b.WriteString(s.Reason)
		if s.Detail != "" {
			b.WriteString(": ")
			b.WriteString(s.Detail)
		}
	}
	if !s.Since.IsZero() {
		fmt.Fprintf(b, " (%s)", s.Age().Round(time.Second))
	}
	return b.String()
}

// Identity returns the display path for a process tag and replica index,
// matching the convention used for log identities ("api", "api-2").
func Identity(tag string, index int) string {
	if index > 0 {
		return fmt.Sprintf("%s-%d", tag, index)
	}
	return tag
}
