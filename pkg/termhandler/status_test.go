package termhandler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hoskeri/procman/pkg/status"
	"github.com/hoskeri/procman/pkg/writelog"
)

// waitStatus polls for a relayed status to land in the registry: the relay runs
// on its own goroutine, so the assertion cannot simply follow the write.
func waitStatus(t *testing.T, th *TermHandler, identity string, want status.State) status.Snapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap, ok := th.Status(identity)
		if ok && snap.State == want {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %q never reached %v (last: %+v, rows: %+v)", identity, want, snap, th.Statuses())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestStatusRegistryProcessLifecycle covers the "unknown is specialized"
// contract: a running process that never reports is StateUnknown with a pid, and
// once it is reaped it becomes StateUnknownProcess with its exit code.
func TestStatusRegistryProcessLifecycle(t *testing.T) {
	th := New(context.Background(), nil, nil, nil, &Options{Plain: true})
	defer th.Close()

	th.ProcessStarted("api", 4321)
	snap, ok := th.Status("api")
	if !ok {
		t.Fatal("expected a row after ProcessStarted")
	}
	if snap.State != status.StateUnknown || snap.PID != 4321 || snap.Exited {
		t.Fatalf("after start: %+v", snap)
	}
	if snap.Since.IsZero() {
		t.Error("a spawned process must be timestamped even before it reports")
	}
	if snap.ExitCode != -1 {
		t.Errorf("exit code should be unknown, got %d", snap.ExitCode)
	}

	th.ProcessExited("api", 3)
	snap, _ = th.Status("api")
	if snap.State != status.StateUnknownProcess {
		t.Errorf("a silent process that exited must become UnknownProcess, got %+v", snap)
	}
	if snap.PID != 0 || !snap.Exited || snap.ExitCode != 3 {
		t.Errorf("after exit: %+v", snap)
	}
}

// TestStatusRegistryKeepsReportedStateOnExit verifies a process that reported
// and then died keeps its reported state (dying while healthy is itself a
// fact) while still recording that it is gone.
func TestStatusRegistryKeepsReportedStateOnExit(t *testing.T) {
	th := New(context.Background(), nil, nil, nil, &Options{Plain: true})
	defer th.Close()

	th.ProcessStarted("webhook", 99)
	th.ReportStatus("webhook", status.Healthy("serving"))

	snap, _ := th.Status("webhook")
	if snap.State != status.StateHealthy || snap.Reason != "serving" {
		t.Fatalf("after report: %+v", snap)
	}
	healthySince := snap.Since

	th.ProcessExited("webhook", 0)
	snap, _ = th.Status("webhook")
	if snap.State != status.StateHealthy {
		t.Errorf("reported state should survive the exit, got %+v", snap)
	}
	if !snap.Exited || snap.PID != 0 || snap.ExitCode != 0 {
		t.Errorf("liveness after exit: %+v", snap)
	}
	if !snap.Since.Equal(healthySince) {
		t.Errorf("the reported state was held since %v, not restarted to %v", healthySince, snap.Since)
	}
}

// TestStatusRegistryRestartsSinceOnChange verifies the held duration restarts
// when the state (or its reason) changes, so "unhealthy for 2m" is meaningful.
func TestStatusRegistryRestartsSinceOnChange(t *testing.T) {
	th := New(context.Background(), nil, nil, nil, &Options{Plain: true})
	defer th.Close()

	th.ProcessStarted("api", 1)
	th.ReportStatus("api", status.Unhealthy(nil, "etcd unreachable"))
	first, _ := th.Status("api")
	if first.Since.IsZero() {
		t.Fatal("Since must be set")
	}
	if first.Recoverable != true {
		t.Error("unhealthy must be recoverable")
	}

	// Same state, same reason: the report is not a new condition.
	th.ReportStatus("api", status.Unhealthy(nil, "etcd unreachable"))
	same, _ := th.Status("api")
	if !same.Since.Equal(first.Since) {
		t.Errorf("Since restarted on an identical report: %v -> %v", first.Since, same.Since)
	}

	th.ReportStatus("api", status.Failed(nil, "data dir unwritable"))
	failed, _ := th.Status("api")
	if failed.Recoverable {
		t.Error("failed must not be recoverable")
	}
	if !failed.Since.After(first.Since) {
		t.Error("Since must restart on a state change")
	}

	th.ReportStatus("api", status.Healthy("serving"))
	back, _ := th.Status("api")
	if back.State != status.StateHealthy || back.Recoverable != true {
		t.Errorf("recovery should return to healthy: %+v", back)
	}
}

func TestStatusRegistryStatusesSorted(t *testing.T) {
	th := New(context.Background(), nil, nil, nil, &Options{Plain: true})
	defer th.Close()

	for _, id := range []string{"webhook", "api", "etcd"} {
		th.ProcessStarted(id, 1)
	}
	got := th.Statuses()
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	for i, want := range []string{"api", "etcd", "webhook"} {
		if got[i].Identity != want {
			t.Errorf("row %d = %q, want %q", i, got[i].Identity, want)
		}
	}
	if _, ok := th.Status("nope"); ok {
		t.Error("unknown identity should not resolve")
	}
}

// TestChildStatusRelayedIntoRegistry is the end-to-end nested case: a child
// process (or a nested formation) pushes a status through its framer, and the
// parent's relay records it under "webhook/validate" while rendering the line.
func TestChildStatusRelayedIntoRegistry(t *testing.T) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()

	th := New(context.Background(), nil, stdoutW, stdoutW, &Options{})
	th.ProcessStarted("webhook", 4242)

	outFD, errFD, err := th.ChildFDs("webhook", 0, nil)
	if err != nil {
		t.Fatalf("ChildFDs: %v", err)
	}

	// The child side: a framer over its stdout channel, tagged like a
	// sub-component, feeding records from a status.Reporter.
	framer := writelog.NewFramer(writelog.SetupSendSocket(int(outFD.Fd())), writelog.StreamStdout, slog.LevelDebug)
	rep := status.NewReporter(writelog.WithTag(slog.New(framer), "validate"))
	rep.Unhealthy(nil, "etcd unreachable")
	rep.OK("serving")

	snap := waitStatus(t, th, "webhook/validate", status.StateHealthy)
	if snap.Reason != "serving" {
		t.Errorf("relayed snapshot = %+v", snap)
	}
	if snap.PID != 4242 || snap.Exited {
		t.Errorf("liveness should come from the spawn registration: %+v", snap)
	}

	errFD.Close()
	outFD.Close()
	th.Close()
	stdoutW.Close()

	out, _ := io.ReadAll(stdoutR)
	s := string(out)
	// Healthy is logged at Debug, so at the default Info level only the
	// unhealthy line renders — the registry still saw both (asserted above).
	for _, want := range []string{
		"webhook/validate | UNHEALTHY etcd unreachable",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output, got:\n%s", want, s)
		}
	}
	// The reserved status attrs are already in the message; they must not be
	// duplicated as logfmt attributes.
	if strings.Contains(s, "status=unhealthy") || strings.Contains(s, "since=") {
		t.Errorf("reserved status attrs should be filtered from the rendered line:\n%s", s)
	}
}

// TestRegistryRecordsStatusEvenWhenLevelFiltered pins the one deliberate
// divergence from the log path: a status that the level policy hides from the
// log stream is still recorded.
func TestRegistryRecordsStatusEvenWhenLevelFiltered(t *testing.T) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()

	// Level Error: healthy (Debug) and starting (Info) records are dropped.
	th := New(context.Background(), nil, stdoutW, stdoutW, &Options{Level: slog.LevelError})
	outFD, errFD, err := th.ChildFDs("api", 0, nil)
	if err != nil {
		t.Fatalf("ChildFDs: %v", err)
	}
	framer := writelog.NewFramer(writelog.SetupSendSocket(int(outFD.Fd())), writelog.StreamStdout, slog.LevelDebug)
	rep := status.NewReporter(writelog.WithTag(slog.New(framer), "serve"))
	rep.OK("serving")

	waitStatus(t, th, "api/serve", status.StateHealthy)

	errFD.Close()
	outFD.Close()
	th.Close()
	stdoutW.Close()

	out, _ := io.ReadAll(stdoutR)
	if strings.Contains(string(out), "serving") {
		t.Errorf("the healthy record should have been filtered at Level=Error, got:\n%s", out)
	}
}

// TestObserveStatusUnknownName verifies a malformed/absent state is ignored
// rather than corrupting the registry.
func TestObserveStatusUnknownName(t *testing.T) {
	th := New(context.Background(), nil, nil, nil, &Options{Plain: true})
	defer th.Close()

	th.ObserveStatus("api", writelog.StatusRecord{State: "bogus", Reason: "x"})
	if _, ok := th.Status("api"); ok {
		t.Error("an unknown state name should not create a row")
	}

	th.ObserveStatus("", writelog.StatusRecord{State: "healthy"})
	if rows := th.Statuses(); len(rows) != 0 {
		t.Errorf("an empty identity should be ignored, got %+v", rows)
	}

	th.ObserveStatus("api", writelog.StatusRecord{State: "failed", Reason: "bad", Detail: "d", Since: 90 * time.Second})
	snap, ok := th.Status("api")
	if !ok || snap.State != status.StateFailed || snap.Detail != "d" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Age() < 89*time.Second {
		t.Errorf("Since should be timed from the reported duration, Age=%v", snap.Age())
	}
	if snap.Recoverable {
		t.Error("failed must not be recoverable")
	}
}

// observedStatus is one status a hook saw.
type observedStatus struct {
	identity string
	rec      writelog.StatusRecord
}

// statusHook is a writelog.StatusObserver that reports what it saw on a
// channel, for the AddStatusObserver test.
type statusHook struct {
	got chan observedStatus
}

func newStatusHook() *statusHook { return &statusHook{got: make(chan observedStatus, 4)} }

func (h *statusHook) ObserveStatus(identity string, rec writelog.StatusRecord) {
	select {
	case h.got <- observedStatus{identity: identity, rec: rec}:
	default:
	}
}

// TestAddStatusObserverReceivesRelayedStatuses verifies the formation's hook:
// a status relayed from a child channel reaches a registered observer in
// addition to the handler's own registry, with the child-prefixed identity.
func TestAddStatusObserverReceivesRelayedStatuses(t *testing.T) {
	th := New(context.Background(), nil, nil, nil, &Options{Plain: true})
	defer th.Close()

	hook := newStatusHook()
	th.AddStatusObserver(hook)

	out, errFD, err := th.ChildFDs("node-a", 0, nil)
	if err != nil {
		t.Fatalf("ChildFDs: %v", err)
	}
	defer out.Close()
	defer errFD.Close()

	framer := writelog.NewFramer(int(out.Fd()), writelog.StreamStdout, slog.LevelDebug)
	logger := writelog.WithTag(slog.New(framer), "kubelet/status")
	logger.LogAttrs(context.Background(), slog.LevelDebug, "ok", slog.String(writelog.AttrStatus, "healthy"))

	select {
	case got := <-hook.got:
		if got.identity != "node-a/kubelet/status" {
			t.Errorf("identity = %q, want node-a/kubelet/status", got.identity)
		}
		if got.rec.State != "healthy" {
			t.Errorf("record = %+v", got.rec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("registered observer never saw the relayed status")
	}
}
