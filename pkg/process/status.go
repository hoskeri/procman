package process

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hoskeri/procman/pkg/status"
	"github.com/hoskeri/procman/pkg/writelog"
)

// Status polling defaults.  A Status callback is an in-process call, so these
// are deliberately modest; a component that needs a slower cadence can do its
// own caching inside the callback.
const (
	// DefaultStatusInterval is how often a Status callback is called while the
	// component is healthy.
	DefaultStatusInterval = 5 * time.Second
	// DefaultStatusBackoff is the first retry delay after a recoverable
	// unhealthy report.
	DefaultStatusBackoff = 1 * time.Second
	// DefaultStatusBackoffMax caps the retry delay.  A fatal (Failed) report
	// is re-checked at this rate rather than never, because its cause may be
	// external and fixable — but it is a condition worth waiting on.
	DefaultStatusBackoffMax = 30 * time.Second
)

// StatusSink is the optional half of a LogSink that also carries component
// status.  termhandler.TermHandler implements it: a formation pushes its
// Process.Status results (and process liveness) into the sink's status
// registry, which is the same registry fed by statuses relayed up from child
// processes and nested formations.  When the formation's Logs does not
// implement it, statuses are logged as records instead.
type StatusSink interface {
	// ReportStatus publishes a status for identity ("api", "webhook/validate").
	ReportStatus(identity string, e *status.Error)
	// ProcessStarted registers a live process with its unix pid, so an
	// unreported process is StateUnknown rather than missing.
	ProcessStarted(identity string, pid int)
	// ProcessExited records a reaped process and its exit code (-1 unknown).
	ProcessExited(identity string, code int)
}

// ComponentStatus is one process's contribution to the formation's StatusView:
// the state its own Status callback last returned, plus the unix process facts
// the formation observed.
type ComponentStatus struct {
	Tag    string
	Index  int
	State  status.State // reported state, or Unknown / UnknownProcess
	Reason string
	Detail string
	// Since is when the reported state began (zero when nothing was ever
	// reported).
	Since       time.Time
	Recoverable bool
	// Fatal reports State == Failed: the component will not recover on its
	// own.  A fatal component status is *not* terminal by itself — only the
	// formation's own Status callback can end Run (see Formation.Status).
	Fatal bool

	// PID is the live process id, 0 once exited.
	PID int
	// Exited reports the process is gone (reaped by this formation).
	Exited bool
	// ExitCode is the observed exit code, -1 when unknown.
	ExitCode int
}

// Identity is the display path of this component ("api", "api-2").
func (c ComponentStatus) Identity() string { return status.Identity(c.Tag, c.Index) }

// StatusView is the aggregate passed to Formation.Status on every tick.
type StatusView struct {
	Components []ComponentStatus
}

// Get returns the component status for a tag (first replica when index == 0).
func (v StatusView) Get(tag string) (ComponentStatus, bool) {
	for _, c := range v.Components {
		if c.Tag == tag {
			return c, true
		}
	}
	return ComponentStatus{}, false
}

// Unhealthy returns every component that is not healthy and not merely
// starting, i.e. the ones an embedder might want to act on.
func (v StatusView) Unhealthy() []ComponentStatus {
	var out []ComponentStatus
	for _, c := range v.Components {
		if c.State == status.StateDegraded || c.State == status.StateUnhealthy || c.State == status.StateFailed {
			out = append(out, c)
		}
	}
	return out
}

// Fatal returns every component in the terminal Failed state.
func (v StatusView) Fatal() []ComponentStatus {
	var out []ComponentStatus
	for _, c := range v.Components {
		if c.Fatal {
			out = append(out, c)
		}
	}
	return out
}

// statusTrack is the formation's mutable view of one process: the reported
// half (written by its poller) and the liveness half (written when it starts
// and when it is reaped).  It also doubles as the "is this process gone"
// signal for the poller, which is authoritative (the formation reaped it, so
// no pid recycling can confuse it).
type statusTrack struct {
	done     chan struct{} // closed when the process has been reaped
	doneOnce sync.Once
	mu       sync.Mutex
	state    status.State
	reason   string
	detail   string
	since    time.Time
	pid      int
	exited   bool
	code     int
}

func newStatusTrack() *statusTrack {
	return &statusTrack{done: make(chan struct{}), state: status.StateUnknown, code: -1}
}

func (t *statusTrack) started(pid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pid = pid
	t.exited = false
	t.code = -1
}

// exitedProcess records the reap: the pid is cleared, the exit code kept, and
// the done channel closed so the poller stops.  The formation reaping the child
// is authoritative about death, unlike a signal-0 probe on a pid the OS may
// have recycled.
func (t *statusTrack) exitedProcess(code int) {
	t.mu.Lock()
	t.pid = 0
	t.exited = true
	t.code = code
	t.mu.Unlock()
	t.doneOnce.Do(func() { close(t.done) })
}

// isDone reports whether the process is gone, so there is nothing left to
// poll.
func (t *statusTrack) isDone() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

func (t *statusTrack) component() ComponentStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	cs := ComponentStatus{
		State:       t.state,
		Reason:      t.reason,
		Detail:      t.detail,
		Since:       t.since,
		Recoverable: t.state.Recoverable(),
		Fatal:       t.state == status.StateFailed,
		PID:         t.pid,
		Exited:      t.exited,
		ExitCode:    t.code,
	}
	return cs
}

func (t *statusTrack) view(p *Process) ComponentStatus {
	cs := t.component()
	cs.Tag = p.Tag
	cs.Index = p.Index
	return cs
}

// report records a classified status.  A change of state or reason restarts
// the held duration.
func (t *statusTrack) report(e *status.Error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != e.State || t.reason != e.Reason {
		t.since = time.Now()
	}
	t.state = e.State
	t.reason = e.Reason
	t.detail = e.Detail
}

// statusRecorder is the internal view of the optional StatusSink plus the
// formation's own tagged logger, so a status is reported exactly once whether
// or not the sink tracks status.
type statusRecorder struct {
	sink StatusSink
	// log is the fallback logger for statuses that belong to no single
	// process (the formation's own verdict).  Per-process records get their own
	// tagged logger in startStatusPollers.
	log *slog.Logger
}

func (r statusRecorder) report(identity string, e *status.Error) {
	if e == nil {
		return
	}
	if r.sink != nil {
		r.sink.ReportStatus(identity, e)
	}
	if r.log != nil && e.State.Reported() {
		// Render a status as an ordinary record so it is visible without a
		// status-aware consumer: "<tag> | FAILED <reason>".  A healthy status
		// has no reason by construction, so it renders as just its state.
		msg := e.Error()
		if msg == "" {
			msg = "ok"
		}
		r.log.LogAttrs(context.Background(), e.State.Level(), msg,
			slog.String(writelog.AttrStatus, e.State.String()))
	}
}

// pollLoop calls p.Status on a schedule until the process is gone or the
// formation is canceled.  It never ends the formation: a component's status is
// reported, not obeyed.  A recoverable failure shrinks the poll rate (the
// reported state is unchanged, so the registry's held duration keeps
// counting); a fatal one is re-checked at the slow backoff rate so an
// externally-caused failure can still recover.
func (l *Formation) pollLoop(ctx context.Context, p *Process, t *statusTrack, rec statusRecorder) {
	interval := l.statusInterval()
	delay := interval
	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// Nothing left to poll if the process is gone.
		if t.isDone() {
			return
		}

		err := p.Status(ctx)
		e := status.FromError(err)
		if e.State == status.StateHealthy {
			e.Since = time.Time{}
		}
		t.report(e)
		rec.report(status.Identity(p.Tag, p.Index), e)

		switch {
		case e.State == status.StateHealthy || e.State == status.StateStarting:
			delay = interval
		case e.State == status.StateFailed:
			// Terminal-looking, but the cause may be external: keep
			// checking slowly rather than freezing the state.
			delay = l.statusBackoffMax()
		default:
			delay = min(backoffFor(l.statusBackoff(), l.statusBackoffMax()), interval)
		}
		timer.Reset(delay)
	}
}

// backoffFor doubles d up to max (a plain geometric schedule; the observed
// state is what matters, not the exact cadence).
func backoffFor(d, max time.Duration) time.Duration {
	if d >= max {
		return max
	}
	return d * 2
}

func (l *Formation) statusInterval() time.Duration {
	if l.StatusInterval > 0 {
		return l.StatusInterval
	}
	return DefaultStatusInterval
}

func (l *Formation) statusBackoff() time.Duration {
	if l.StatusBackoff > 0 {
		return l.StatusBackoff
	}
	return DefaultStatusBackoff
}

func (l *Formation) statusBackoffMax() time.Duration {
	if l.StatusBackoffMax > 0 {
		return l.StatusBackoffMax
	}
	return DefaultStatusBackoffMax
}

// hasStatus reports whether any status polling is needed at all, so a formation
// that does not use status starts no goroutine and no timers.
func (l *Formation) hasStatus() bool {
	if l.Status != nil {
		return true
	}
	for _, p := range l.Processes {
		if p != nil && p.Status != nil {
			return true
		}
	}
	return false
}

// startStatusLoaders registers one track per process and starts a poller per
// process with a Status callback.  The caller wires the tracks' liveness into
// the process goroutines.
//
// Each poller logs under the process's own identity ("<process>/status"), so a
// relayed status line reads "webhook/validate | UNHEALTHY ..." rather than an
// anonymous "status | UNHEALTHY ..." — the tag path is what tells an operator
// (and a parent formation) which component spoke.
func (l *Formation) startStatusPollers(ctx context.Context, tracks []*statusTrack, rec statusRecorder, base *slog.Logger) func() {
	var wg sync.WaitGroup
	for i, p := range l.Processes {
		if p == nil || p.Status == nil {
			continue
		}
		r := rec
		if base != nil {
			r.log = writelog.WithTag(base, status.Identity(p.Tag, p.Index)+"/status")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.pollLoop(ctx, p, tracks[i], r)
		}()
	}
	return wg.Wait
}

// view builds the aggregate passed to Formation.Status.
func (l *Formation) view(tracks []*statusTrack) StatusView {
	v := StatusView{Components: make([]ComponentStatus, 0, len(l.Processes))}
	for i, p := range l.Processes {
		if p == nil {
			continue
		}
		v.Components = append(v.Components, tracks[i].view(p))
	}
	return v
}

// statusViewKey is the context key carrying the StatusView handed to
// Formation.Status, so the callback signature stays a plain
// func(ctx) error.
type statusViewKey struct{}

// WithStatusView returns a context carrying v, for Formation.Run's internal
// use when invoking Formation.Status.
func WithStatusView(ctx context.Context, v StatusView) context.Context {
	return context.WithValue(ctx, statusViewKey{}, v)
}

// StatusViewFromContext returns the aggregate view of the formation's
// components as of the current status tick.  It is the only way a
// Formation.Status callback learns what its processes reported:
//
//	fm := &process.Formation{
//	    Status: func(ctx context.Context) error {
//	        v, _ := process.StatusViewFromContext(ctx)
//	        for _, c := range v.Fatal() { ... }
//	        return nil
//	    },
//	}
//
// ok is false when called outside Formation.Run, in which case an empty view is
// returned and the callback should treat every component as unreported.
func StatusViewFromContext(ctx context.Context) (StatusView, bool) {
	v, ok := ctx.Value(statusViewKey{}).(StatusView)
	return v, ok
}

// formationStatusLoop calls the formation's own Status callback on each tick
// and returns its fatal result, which ends the formation exactly like a
// process exit does.  Any other outcome (nil, or a recoverable status) only
// updates the reported state and the formation keeps waiting.
func (l *Formation) formationStatusLoop(ctx context.Context, tracks []*statusTrack, rec statusRecorder) error {
	interval := l.statusInterval()
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		// Each callback sees a fresh aggregate: the reported half from the
		// pollers, the liveness half from the process goroutines.
		callCtx := WithStatusView(ctx, l.view(tracks))
		e := status.FromError(l.Status(callCtx))
		if e.State != status.StateFailed {
			// Not a verdict on the formation: report it under the "procman"
			// identity so it stays visible, but keep going.
			if e.State.Reported() {
				rec.report("procman", e)
			}
			timer.Reset(interval)
			continue
		}
		rec.report("procman", e)
		if ctx.Err() != nil {
			// The formation was torn down by someone else first; this verdict
			// is not what ended it.
			return nil
		}
		return e
	}
}
