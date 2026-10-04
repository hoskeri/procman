package status

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hoskeri/procman/pkg/writelog"
)

// Reporter publishes a component's status to whatever formation is hosting the
// current process.  It is the push-side twin of the Status(ctx) error callback:
// a sub-component that learns about a condition out of band (a cert watcher, a
// retry loop, a readiness probe) holds a Reporter and updates it when things
// change.
//
// Every report is an ordinary framed log record carrying the reserved status
// attributes, so it crosses a process boundary and a nested formation on the
// same channel as logs, and is subject to the usual --log-level policy.  The
// parent's status registry records it regardless of whether the line rendered.
//
// A Reporter is safe for concurrent use, never blocks, and remembers the last
// report so Snapshot can be read back locally.
type Reporter struct {
	sink *slog.Logger
	now  func() time.Time

	mu    sync.Mutex
	last  *Error
	since time.Time
}

// NewReporter returns a Reporter that publishes records to sink, which should
// already carry the component's display tag (writelog.WithTag / WithTag).
// A nil sink degrades to slog.Default().
func NewReporter(sink *slog.Logger) *Reporter {
	if sink == nil {
		sink = slog.Default()
	}
	return &Reporter{sink: sink, now: time.Now}
}

// TaggedReporter returns a Reporter for a component hosted inside the current
// process, mirroring writelog.TaggedLogger: it probes the standard streams for
// a parent formation's log channel, reuses an already-installed FramerHandler
// default, and falls back to slog.Default() so the same code works
// stand-alone.  The parent renders the component as "<process>/<tag>".
func TaggedReporter(tag string) *Reporter {
	return NewReporter(writelog.ChildLogger(tag))
}

// OK reports the component healthy, clearing any previous condition.
func (r *Reporter) OK(format string, a ...any) {
	r.report(Healthy(format, a...))
}

// Starting reports an expected not-ready-yet condition.
func (r *Reporter) Starting(format string, a ...any) {
	r.report(Starting(format, a...))
}

// Degraded reports that the component still serves but is unhappy.
func (r *Reporter) Degraded(err error, format string, a ...any) {
	r.report(Degraded(err, format, a...))
}

// Unhealthy reports that the component is not serving now but may recover.
func (r *Reporter) Unhealthy(err error, format string, a ...any) {
	r.report(Unhealthy(err, format, a...))
}

// Failed reports a permanent condition.  Reporting a fatal status never
// terminates anything by itself: only a Formation's Status callback may bring
// the formation down.
func (r *Reporter) Failed(err error, format string, a ...any) {
	r.report(Failed(err, format, a...))
}

// Report publishes an arbitrary classified status (see FromError).
func (r *Reporter) Report(err error) {
	r.report(FromError(err))
}

// Snapshot returns the most recently reported status; State is StateUnknown if
// nothing has been reported yet.
func (r *Reporter) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		return Snapshot{State: StateUnknown}
	}
	snap := Snapshot{
		State:       r.last.State,
		Reason:      r.last.Reason,
		Detail:      r.last.Detail,
		Since:       r.since,
		Recoverable: r.last.Recoverable(),
	}
	if !snap.Since.IsZero() {
		snap.Since = snap.Since.Round(time.Second)
	}
	return snap
}

// Log emits a status through the Reporter as a record (mostly useful for
// embedding a Reporter-shaped component in a struct that also logs).
func (r *Reporter) Log(ctx context.Context, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.report(FromError(err))
}

// report updates the local snapshot and emits the record.  Repeating the same
// state with the same reason is suppressed, so a component that re-checks in a
// loop does not flood the log; the held duration keeps counting because
// snapshot state is only reset when the state or reason actually changes.
func (r *Reporter) report(e *Error) {
	if e == nil {
		e = Healthy("")
	}
	now := r.now()

	r.mu.Lock()
	if r.last != nil && r.last.State == e.State && r.last.Reason == e.Reason && r.last.Detail == e.Detail {
		r.mu.Unlock()
		return
	}
	r.last = e
	r.since = now
	r.mu.Unlock()

	attrs := make([]slog.Attr, 0, 4+len(e.Attrs))
	attrs = append(attrs, slog.String(writelog.AttrStatus, e.State.String()))
	if !e.Since.IsZero() {
		// A sender that already knows how long it has held the condition
		// (an adapter over an existing watcher) reports it; the parent
		// otherwise times it from first receipt.
		attrs = append(attrs, slog.Duration(writelog.AttrSince, now.Sub(e.Since).Round(time.Second)))
	}
	if e.Detail != "" {
		attrs = append(attrs, slog.String(writelog.AttrDetail, e.Detail))
	}
	if e.Err != nil {
		attrs = append(attrs, slog.String("cause", e.Err.Error()))
	}
	attrs = append(attrs, e.Attrs...)

	r.sink.LogAttrs(context.Background(), e.State.Level(), e.Error(), attrs...)
}
