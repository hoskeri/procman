package termhandler

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hoskeri/procman/pkg/status"
	"github.com/hoskeri/procman/pkg/writelog"
)

// StatusStore exposes the current status of every component in the formation,
// whether the status was reported by the component (a Reporter or a Status
// callback relayed up from a child process) or derived from the unix process
// state.
//
// A row exists for every process the formation ran: it is created when the
// process starts (StateUnknown, with its pid) and updated when the process
// exits (StateUnknownProcess when nothing was ever reported).  So "alive but
// silent", "reported healthy", and "gone without a word" are all
// distinguishable, and every row is timestamped.
//
// Rows are never deleted, so a snapshot is a complete picture of the
// formation.  A component's Status is served even when its status *line* was
// filtered out by the level policy: the registry is not gated by log levels.
type StatusStore interface {
	// Statuses returns a snapshot of every known identity, sorted by
	// identity.
	Statuses() []status.Snapshot
	// Status returns the snapshot for one identity; ok is false when nothing
	// is known about it.
	Status(identity string) (status.Snapshot, bool)
}

// statusRegistry is the mutex-guarded store behind StatusStore.  No goroutine
// and no timer: rows are created and updated synchronously from the child
// channel setup, the relay, and the formation's exit path.
type statusRegistry struct {
	mu   sync.Mutex
	rows map[string]*status.Snapshot
}

func newStatusRegistry() *statusRegistry {
	return &statusRegistry{rows: map[string]*status.Snapshot{}}
}

// set applies fn to the row for identity, creating it when absent.  fn runs
// under the registry lock, so it must not block or call back in.
func (r *statusRegistry) set(identity string, fn func(row *status.Snapshot)) {
	if identity == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[identity]
	if !ok {
		row = &status.Snapshot{
			Identity: identity,
			State:    status.StateUnknown,
			Since:    now,
			ExitCode: -1,
		}
		r.rows[identity] = row
	}
	fn(row)
	if row.Since.IsZero() {
		row.Since = now
	}
	row.Recoverable = row.State.Recoverable()
}

func (r *statusRegistry) snapshot(identity string) (status.Snapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[identity]
	if !ok {
		return status.Snapshot{}, false
	}
	return *row, true
}

func (r *statusRegistry) all() []status.Snapshot {
	r.mu.Lock()
	out := make([]status.Snapshot, 0, len(r.rows))
	for _, row := range r.rows {
		out = append(out, *row)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out
}

// observe records a status relayed from a child channel (or, in nested mode,
// from a whole nested formation).  Since is timed from first receipt, so the
// held duration a reader sees is always real even for a component that never
// sent the wire "since" attribute.
func (r *statusRegistry) observe(identity string, rec writelog.StatusRecord) {
	st, ok := status.ParseState(rec.State)
	if !ok {
		return
	}
	r.set(identity, func(row *status.Snapshot) {
		// A sub-component's row inherits the liveness of its process: the
		// formation knows the pid of "webhook", not of "webhook/validate".
		if row.PID == 0 && !row.Exited {
			if proc := strings.LastIndex(identity, "/"); proc > 0 {
				if parent, ok := r.rows[identity[:proc]]; ok {
					row.PID = parent.PID
					row.Exited = parent.Exited
				}
			}
		}
		applyReport(row, st, rec.Reason, rec.Detail, time.Now().Add(-rec.Since))
	})
}

// report records a status produced in-process (a Process's Status callback or
// a formation-level Reporter).
func (r *statusRegistry) report(identity string, e *status.Error) {
	if e == nil {
		return
	}
	since := e.Since
	if since.IsZero() {
		since = time.Now()
	}
	r.set(identity, func(row *status.Snapshot) {
		applyReport(row, e.State, e.Reason, e.Detail, since)
	})
}

// applyReport writes the reported half of a row.  A new state (or a changed
// reason for the same state) restarts the held duration; an unchanged report
// only refreshes the detail.
func applyReport(row *status.Snapshot, st status.State, reason, detail string, since time.Time) {
	if row.State != st || row.Reason != reason {
		row.Since = since
	}
	row.State = st
	row.Reason = reason
	row.Detail = detail
}

// processStarted records a live process for an identity, before it has had any
// chance to report: the row starts in StateUnknown with its pid.
func (r *statusRegistry) processStarted(identity string, pid int) {
	r.set(identity, func(row *status.Snapshot) {
		row.PID = pid
		row.Exited = false
	})
}

// processExited records a reaped process.  The reported state is preserved (a
// process can die while healthy, which is itself a meaningful fact), but a
// component that never said anything becomes StateUnknownProcess so "gone" is
// distinguishable from "silent".  Only StateUnknown collapses that way.
func (r *statusRegistry) processExited(identity string, code int) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	markExited(r.rows[identity], code, now)
	// A process's sub-components go with it: they cannot outlive the process
	// that hosted them, so their rows are updated too (the reported state is
	// preserved; only liveness and the never-reported case change).
	prefix := identity + "/"
	for id, row := range r.rows {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		markExited(row, -1, now)
	}
}

// markExited clears liveness on a row, keeping any reported state and turning a
// row that never reported into StateUnknownProcess.  Rows that are already
// exited are left alone, so a repeated reap cannot restart their timestamp.
func markExited(row *status.Snapshot, code int, now time.Time) {
	if row == nil || (row.Exited && row.PID == 0) {
		return
	}
	row.PID = 0
	row.Exited = true
	if code >= 0 {
		row.ExitCode = code
	}
	if !row.State.Reported() {
		row.State = status.StateUnknownProcess
		row.Since = now
		row.Recoverable = row.State.Recoverable()
	}
}

// statusObserver adapts the registry to writelog.StatusObserver without
// exposing the domain types on the interface (pkg/writelog stays independent of
// pkg/status).
type statusObserver struct{ r *statusRegistry }

func (o statusObserver) ObserveStatus(identity string, rec writelog.StatusRecord) {
	if o.r == nil {
		return
	}
	o.r.observe(identity, rec)
}
