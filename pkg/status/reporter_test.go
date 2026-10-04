package status

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hoskeri/procman/pkg/writelog"
)

// newTestReporter returns a Reporter writing logfmt records into buf, with the
// tag path applied the way a real component logger is.
func newTestReporter(buf *bytes.Buffer, level slog.Level, tags ...string) *Reporter {
	th := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})
	logger := slog.New(th)
	for _, tag := range tags {
		logger = writelog.WithTag(logger, tag)
	}
	return NewReporter(logger)
}

func TestReporterEmitsReservedAttrs(t *testing.T) {
	buf := &bytes.Buffer{}
	// Debug so the healthy record (logged at Debug) is visible.
	r := newTestReporter(buf, slog.LevelDebug, "webhook", "validate")

	r.OK("serving")
	r.Degraded(nil, "slow: %d%%", 90)

	out := buf.String()
	for _, want := range []string{
		`status=healthy`,
		`msg=serving`,
		`status=degraded`,
		`msg="slow: 90%"`,
		`level=WARN`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestReporterLevelsFollowState(t *testing.T) {
	for _, tc := range []struct {
		report func(*Reporter)
		level  string
	}{
		{func(r *Reporter) { r.OK("x") }, "level=DEBUG"},
		{func(r *Reporter) { r.Starting("x") }, "level=INFO"},
		{func(r *Reporter) { r.Degraded(nil, "x") }, "level=WARN"},
		{func(r *Reporter) { r.Unhealthy(nil, "x") }, "level=ERROR"},
		{func(r *Reporter) { r.Failed(nil, "x") }, "level=ERROR"},
	} {
		buf := &bytes.Buffer{}
		r := newTestReporter(buf, slog.LevelDebug)
		tc.report(r)
		if !strings.Contains(buf.String(), tc.level) {
			t.Errorf("expected %s, got:\n%s", tc.level, buf.String())
		}
	}
}

// TestReporterDedupesUnchanged checks that a component polling in a loop does
// not flood the log: an identical repeat is suppressed, while a real change is
// emitted.
func TestReporterDedupesUnchanged(t *testing.T) {
	buf := &bytes.Buffer{}
	r := newTestReporter(buf, slog.LevelDebug)

	r.Unhealthy(nil, "etcd unreachable")
	r.Unhealthy(nil, "etcd unreachable")
	r.Unhealthy(nil, "etcd unreachable")
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("expected 1 record for 3 identical reports, got %d:\n%s", n, buf.String())
	}

	r.OK("recovered")
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Errorf("expected a record for the state change, got %d:\n%s", n, buf.String())
	}
}

func TestReporterSnapshot(t *testing.T) {
	r := NewReporter(nil)

	if snap := r.Snapshot(); snap.State != StateUnknown {
		t.Errorf("fresh Reporter snapshot = %+v, want unknown", snap)
	}

	r.Failed(nil, "cert unusable")
	snap := r.Snapshot()
	if snap.State != StateFailed || snap.Reason != "cert unusable" {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.Recoverable {
		t.Error("failed snapshot must not be recoverable")
	}
	if snap.Since.IsZero() {
		t.Error("snapshot Since must be set")
	}

	r.OK("all good")
	if snap := r.Snapshot(); snap.State != StateHealthy {
		t.Errorf("snapshot after OK = %+v", snap)
	}
}

// TestReporterSnapshotRestartsSince verifies the held duration restarts on a
// state change, so a reader can tell "unhealthy for 2m" from "just went
// unhealthy".
func TestReporterSnapshotRestartsSince(t *testing.T) {
	base := time.Now().Add(-2 * time.Minute)
	var calls int
	r := NewReporter(nil)
	// A clock the test advances between reports.
	r.now = func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * time.Second)
	}

	r.Unhealthy(nil, "down")
	first := r.Snapshot().Since

	r.OK("up")
	second := r.Snapshot().Since

	if !second.After(first) {
		t.Errorf("Since should restart on a state change: %v then %v", first, second)
	}
}

func TestReporterCarriesCauseAndDetail(t *testing.T) {
	buf := &bytes.Buffer{}
	r := newTestReporter(buf, slog.LevelDebug)
	r.Unhealthy(errTest("connection refused"), "etcd %s", "unreachable at 127.0.0.1:2379")

	out := buf.String()
	if !strings.Contains(out, "etcd unreachable at 127.0.0.1:2379") {
		t.Errorf("expected detail in the message, got:\n%s", out)
	}
	if !strings.Contains(out, "cause=\"connection refused\"") {
		t.Errorf("expected the cause attr, got:\n%s", out)
	}
}

func TestReporterLog(t *testing.T) {
	buf := &bytes.Buffer{}
	r := newTestReporter(buf, slog.LevelDebug)
	r.Log(context.Background(), Unhealthy(nil, "via Log"))
	if !strings.Contains(buf.String(), "status=unhealthy") {
		t.Errorf("Log did not publish:\n%s", buf.String())
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
