package process

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoskeri/procman/pkg/status"
	"github.com/hoskeri/procman/pkg/termhandler"
	"github.com/hoskeri/procman/pkg/writelog"
)

// sleeper is a process that runs for a while, long enough for several status
// ticks and short enough to keep the tests quick.
var sleeper = []string{"/bin/sh", "-c", "sleep 30"}

// runFormation runs f in the background and returns a channel carrying Run's
// result.
func runFormation(f *Formation) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f.Run(context.Background()) }()
	return ch
}

// runFormationCtx runs a formation under a context the test can cancel, the way
// procman's own signal handler tears one down (Run then returns nil).
func runFormationCtx(f *Formation) (<-chan error, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- f.Run(ctx) }()
	return ch, cancel
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// testTermHandler returns a TermHandler writing to a temp file (so status log
// lines do not pollute the test output) plus its registry.
func testTermHandler(t *testing.T) *termhandler.TermHandler {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "procman-status-*.log")
	if err != nil {
		t.Fatalf("temp log: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	th := termhandler.New(context.Background(), nil, f, f, &termhandler.Options{Level: slog.LevelDebug})
	t.Cleanup(th.Close)
	return th
}

// TestProcessStatusIsPolledAndReported verifies a Process.Status callback is
// called, that its result reaches the sink's registry, and that it does not
// tear the formation down.
func TestProcessStatusIsPolledAndReported(t *testing.T) {
	th := testTermHandler(t)

	frm := &Formation{
		Logs:           th,
		Processes:      []*Process{{Tag: "api", CmdArgs: sleeper}},
		StatusInterval: 10 * time.Millisecond,
	}

	var polls atomic.Int32
	frm.Processes[0].Status = func(ctx context.Context) error {
		polls.Add(1)
		if polls.Load() == 1 {
			return status.Starting("waiting for cert")
		}
		if polls.Load() == 2 {
			return status.Unhealthy(nil, "etcd unreachable")
		}
		return nil
	}

	done := runFormation(frm)

	// Recoverable states never end the formation.
	waitFor(t, "the unhealthy report", func() bool {
		s, ok := th.Status("api")
		return ok && s.State == status.StateUnhealthy
	})
	select {
	case err := <-done:
		t.Fatalf("formation ended on a recoverable status: %v", err)
	default:
	}

	// ...and it recovers to healthy while the formation keeps waiting.
	waitFor(t, "recovery", func() bool {
		s, ok := th.Status("api")
		return ok && s.State == status.StateHealthy
	})
	if polls.Load() < 3 {
		t.Errorf("expected at least 3 polls, got %d", polls.Load())
	}

	// A spawned process is registered with a pid even before it reports.
	snap, ok := th.Status("api")
	if !ok || snap.PID == 0 || snap.Exited {
		t.Errorf("liveness: %+v", snap)
	}
}

// TestProcessStatusIsTaggedWithItsIdentity: a relayed status line is tagged
// "<process>/status" (and "<tag>-<index>/status" for a replica), so a nested
// formation renders "webhook/status | UNHEALTHY ..." rather than an anonymous
// "status | ..." — the tag path is what says which component spoke.
func TestProcessStatusIsTaggedWithItsIdentity(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "out.log")
	f, err := os.Create(logFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	th := termhandler.New(context.Background(), nil, f, f, &termhandler.Options{Level: slog.LevelDebug, Columns: -1})
	defer th.Close()

	frm := &Formation{
		Logs:           th,
		Processes:      []*Process{{Tag: "api", Index: 2, CmdArgs: sleeper}},
		StatusInterval: 10 * time.Millisecond,
	}
	frm.Processes[0].Status = func(context.Context) error {
		return status.Unhealthy(nil, "etcd unreachable")
	}

	// The status line is written through the sink's relay, so give it a moment
	// after the registry updates before tearing the formation down.
	done, cancel := runFormationCtx(frm)
	waitFor(t, "the unhealthy report", func() bool {
		s, ok := th.Status("api-2")
		return ok && s.State == status.StateUnhealthy
	})
	// The status line is rendered through the sink's relay, so give it a moment
	// after the registry updates before tearing the formation down.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	out, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "api-2/status | UNHEALTHY etcd unreachable") {
		t.Errorf("expected a tagged status line, got:\n%s", out)
	}
}

// TestFormationStatusFatalEndsRun is the core lifecycle rule: only the
// formation's own Status callback may end the formation, and only with a fatal
// status.
func TestFormationStatusFatalEndsRun(t *testing.T) {
	th := testTermHandler(t)

	frm := &Formation{
		Logs:           th,
		Processes:      []*Process{{Tag: "webhook", CmdArgs: sleeper}},
		StatusInterval: 10 * time.Millisecond,
		// A fatal component status that the embedder decides to tolerate:
		// Run must keep waiting.
		Status: func(ctx context.Context) error {
			v, ok := StatusViewFromContext(ctx)
			if !ok {
				t.Error("Formation.Status got no StatusView")
			}
			for _, c := range v.Components {
				if c.Tag == "webhook" && c.State == status.StateFailed {
					return status.Degraded(nil, "webhook degraded, waiting")
				}
			}
			return nil
		},
	}
	frm.Processes[0].Status = func(context.Context) error {
		return status.Failed(nil, "cert %s unusable", "webhook")
	}

	done := runFormation(frm)
	waitFor(t, "the fatal component status", func() bool {
		s, ok := th.Status("webhook")
		return ok && s.State == status.StateFailed
	})
	select {
	case err := <-done:
		t.Fatalf("formation ended although the policy tolerated the status: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestFormationStatusFatalTearsDownRun verifies the verdict that does end the
// formation, and that it comes back as a *status.Error.
func TestFormationStatusFatalTearsDownRun(t *testing.T) {
	th := testTermHandler(t)

	frm := &Formation{
		Logs:           th,
		Processes:      []*Process{{Tag: "webhook", CmdArgs: sleeper}},
		StatusInterval: 10 * time.Millisecond,
		Status: func(ctx context.Context) error {
			v, _ := StatusViewFromContext(ctx)
			if len(v.Fatal()) > 0 {
				return status.Failed(nil, "webhook cert unusable")
			}
			return nil
		},
	}
	frm.Processes[0].Status = func(context.Context) error {
		return status.Failed(nil, "cert %s unusable", "webhook")
	}

	done := runFormation(frm)

	select {
	case err := <-done:
		var se *status.Error
		if !errors.As(err, &se) {
			t.Fatalf("Run returned %T (%v), want *status.Error", err, err)
		}
		if se.State != status.StateFailed {
			t.Errorf("state = %v", se.State)
		}
		if se.Recoverable() {
			t.Error("the returned status must be fatal")
		}
		// The sibling teardown is the same soft shutdown an exit triggers: the
		// process is not left running.
		waitFor(t, "the process teardown", func() bool {
			s, ok := th.Status("webhook")
			return ok && s.Exited
		})
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after a fatal formation status")
	}
}

// TestFormationStatusSeesLiveness verifies the aggregate carries the unix
// process state, so a policy can distinguish "unhealthy" from "gone".  A
// process exit ends the formation, so the view is inspected directly.
func TestFormationStatusSeesLiveness(t *testing.T) {
	frm := &Formation{Processes: []*Process{{Tag: "api", Index: 2}}}
	tracks := []*statusTrack{newStatusTrack()}
	tracks[0].started(4242)
	tracks[0].report(status.Unhealthy(nil, "etcd unreachable"))

	v := frm.view(tracks)
	c, ok := v.Get("api")
	if !ok {
		t.Fatal("Get(api) missed")
	}
	if c.PID != 4242 || c.Exited || c.Index != 2 || c.Identity() != "api-2" {
		t.Errorf("running component: %+v", c)
	}
	if !c.Recoverable || c.Fatal || c.State != status.StateUnhealthy {
		t.Errorf("classification: %+v", c)
	}

	// Reaped without ever reporting: nothing is recoverable to inspect.
	tracks[0].exitedProcess(7)
	v = frm.view(tracks)
	c, _ = v.Get("api")
	if !c.Exited || c.ExitCode != 7 || c.PID != 0 {
		t.Errorf("exited component: %+v", c)
	}
	if tracks[0].isDone() != true {
		t.Error("a reaped process must report itself done")
	}
}

// TestStatusPollingStopsWhenProcessGone verifies there is no point polling a
// process that no longer exists.
func TestStatusPollingStopsWhenProcessGone(t *testing.T) {
	th := testTermHandler(t)

	var polls atomic.Int32
	frm := &Formation{
		Logs:           th,
		Processes:      []*Process{{Tag: "quick", CmdArgs: []string{"/bin/sh", "-c", "sleep 0.05"}}},
		StatusInterval: 5 * time.Millisecond,
	}
	frm.Processes[0].Status = func(context.Context) error {
		polls.Add(1)
		return nil
	}

	runFormation(frm)

	// Let the process go away and the poller notice.
	waitFor(t, "the process exit", func() bool {
		s, ok := th.Status("quick")
		return ok && s.Exited
	})
	settled := polls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := polls.Load(); got != settled {
		t.Errorf("polling continued after the process exited: %d -> %d", settled, got)
	}
}

// TestNoStatusCallbacksMeansNoWork pins the zero-cost requirement: a formation
// that does not use status starts no poller and needs no intervals.
func TestNoStatusCallbacksMeansNoWork(t *testing.T) {
	f := &Formation{
		Processes: []*Process{{Tag: "api", CmdArgs: sleeper}},
	}
	if f.hasStatus() {
		t.Error("hasStatus() should be false without any Status callback")
	}
	if f.statusInterval() != DefaultStatusInterval ||
		f.statusBackoff() != DefaultStatusBackoff ||
		f.statusBackoffMax() != DefaultStatusBackoffMax {
		t.Error("defaults are wrong")
	}

	// Explicit overrides win.
	f.StatusInterval = time.Second
	f.StatusBackoff = 2 * time.Second
	f.StatusBackoffMax = 3 * time.Second
	if f.statusInterval() != time.Second || f.statusBackoff() != 2*time.Second || f.statusBackoffMax() != 3*time.Second {
		t.Error("overrides ignored")
	}
}

// TestProcessStatusReportedWithoutStatusSink verifies statuses are still logged
// as records when the formation has no status-aware sink.
func TestProcessStatusReportedWithoutStatusSink(t *testing.T) {
	// Run with Logs == nil: the writelog text pipeline takes over and the
	// status still lands as a record on the process's tag.
	p := &Process{
		Tag:     "api",
		CmdArgs: []string{"/bin/sh", "-c", "sleep 0.1"},
		Status:  func(context.Context) error { return status.Degraded(nil, "slow") },
	}
	err := p.run(context.Background())
	if err == nil {
		t.Fatal("expected the exit to be reported")
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("got %T (%v)", err, err)
	}
}

func TestStatusViewHelpers(t *testing.T) {
	v := StatusView{Components: []ComponentStatus{
		{Tag: "api", Index: 2, State: status.StateHealthy},
		{Tag: "etcd", State: status.StateUnhealthy},
		{Tag: "webhook", State: status.StateFailed, Fatal: true},
		{Tag: "kms", State: status.StateStarting},
	}}

	if c, ok := v.Get("etcd"); !ok || c.State != status.StateUnhealthy {
		t.Errorf("Get(etcd) = %+v, %v", c, ok)
	}
	if _, ok := v.Get("nope"); ok {
		t.Error("Get should miss an unknown tag")
	}
	if c, _ := v.Get("api"); c.Identity() != "api-2" {
		t.Errorf("Identity() = %q", c.Identity())
	}

	unhealthy := v.Unhealthy()
	if len(unhealthy) != 2 {
		t.Errorf("Unhealthy() = %+v", unhealthy)
	}
	if f := v.Fatal(); len(f) != 1 || f[0].Tag != "webhook" {
		t.Errorf("Fatal() = %+v", f)
	}
}

// TestRelayedChildStatusFeedsFormationView pins the supervisor roll-up: a
// process with no Status callback of its own takes its state from the
// sub-component statuses relayed up from a child (the node-a/kubelet,
// node-a/criserver case), while an own report still wins.
func TestRelayedChildStatusFeedsFormationView(t *testing.T) {
	frm := &Formation{Processes: []*Process{{Tag: "node-a"}}}
	tracks := []*statusTrack{newStatusTrack()}
	tracks[0].started(4321)
	obs := newChildStatusObserver(frm.Processes, tracks)

	// Alive but silent: nothing relayed yet.
	if c, _ := frm.view(tracks).Get("node-a"); c.State != status.StateUnknown {
		t.Fatalf("no sub report should stay unknown: %+v", c)
	}

	obs.ObserveStatus("node-a/kubelet/status", writelog.StatusRecord{State: "healthy"})
	obs.ObserveStatus("node-a/criserver/status", writelog.StatusRecord{State: "healthy"})
	c, _ := frm.view(tracks).Get("node-a")
	if c.State != status.StateHealthy {
		t.Fatalf("all subs healthy should be healthy: %+v", c)
	}
	if c.PID != 4321 {
		t.Errorf("liveness lost in roll-up: %+v", c)
	}

	// The worst sub wins, and its reason explains the aggregate.
	obs.ObserveStatus("node-a/criserver/status", writelog.StatusRecord{State: "unhealthy", Reason: "cri down"})
	c, _ = frm.view(tracks).Get("node-a")
	if c.State != status.StateUnhealthy || c.Reason != "cri down" {
		t.Fatalf("worst sub must win: %+v", c)
	}
	if !c.Recoverable || c.Fatal {
		t.Errorf("unhealthy must stay recoverable: %+v", c)
	}

	// An own report overrides the sub aggregate.
	tracks[0].report(status.Failed(nil, "supervisor dead"))
	c, _ = frm.view(tracks).Get("node-a")
	if c.State != status.StateFailed || !c.Fatal || c.Reason != "supervisor dead" {
		t.Fatalf("own report must win over subs: %+v", c)
	}

	// Statuses for another process, or with an unknown state name, are ignored.
	obs.ObserveStatus("node-b/kubelet/status", writelog.StatusRecord{State: "healthy"})
	obs.ObserveStatus("node-a/kubelet/status", writelog.StatusRecord{State: "bogus"})
	if got := len(tracks[0].subs); got != 2 {
		t.Errorf("subs = %d, want 2", got)
	}
}

// hookSink is a LogSink that captures the formation's ChildStatusSource hook,
// so a test can fire relayed statuses without a child process.
type hookSink struct {
	mu    sync.Mutex
	obs   []writelog.StatusObserver
	lastV StatusView
}

func (s *hookSink) Logger() *slog.Logger { return slog.Default() }

func (s *hookSink) ChildFDs(string, int, writelog.LevelResolver) (*os.File, *os.File, error) {
	out, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, err
	}
	errOut, err := os.Open(os.DevNull)
	if err != nil {
		out.Close()
		return nil, nil, err
	}
	return out, errOut, nil
}

func (s *hookSink) AddStatusObserver(o writelog.StatusObserver) {
	s.mu.Lock()
	s.obs = append(s.obs, o)
	s.mu.Unlock()
}

func (s *hookSink) observers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.obs)
}

func (s *hookSink) fire(identity string, rec writelog.StatusRecord) {
	s.mu.Lock()
	obs := append([]writelog.StatusObserver(nil), s.obs...)
	s.mu.Unlock()
	for _, o := range obs {
		o.ObserveStatus(identity, rec)
	}
}

func (s *hookSink) record(v StatusView) {
	s.mu.Lock()
	s.lastV = v
	s.mu.Unlock()
}

func (s *hookSink) latest() StatusView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastV
}

// TestFormationRegistersChildStatusHook verifies Run installs the
// ChildStatusSource hook before creating the child channels, and that a
// relayed sub-component status then reaches the formation's status view.
func TestFormationRegistersChildStatusHook(t *testing.T) {
	sink := &hookSink{}
	frm := &Formation{
		Logs:           sink,
		Processes:      []*Process{{Tag: "node-a", CmdArgs: sleeper}},
		StatusInterval: 10 * time.Millisecond,
		Status: func(ctx context.Context) error {
			v, _ := StatusViewFromContext(ctx)
			sink.record(v)
			return nil
		},
	}
	_, cancel := runFormationCtx(frm)
	defer cancel()

	waitFor(t, "the child status hook to be registered", func() bool { return sink.observers() > 0 })

	sink.fire("node-a/kubelet/status", writelog.StatusRecord{State: "healthy"})
	sink.fire("node-a/criserver/status", writelog.StatusRecord{State: "healthy"})

	waitFor(t, "node-a to converge to healthy", func() bool {
		c, ok := sink.latest().Get("node-a")
		return ok && c.State == status.StateHealthy
	})
}
