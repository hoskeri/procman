package status

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestStateNamesRoundTrip(t *testing.T) {
	states := []State{
		StateUnknown, StateUnknownProcess, StateStarting,
		StateHealthy, StateDegraded, StateUnhealthy, StateFailed,
	}
	for _, s := range states {
		got, ok := ParseState(s.String())
		if !ok || got != s {
			t.Errorf("ParseState(%q) = %v, %v; want %v, true", s.String(), got, ok, s)
		}
	}
	if _, ok := ParseState("bogus"); ok {
		t.Error("ParseState(\"bogus\") should fail")
	}
}

// TestStateRecoverable pins the whole point of the vocabulary: everything is
// recoverable ("keep waiting") except StateFailed.
func TestStateRecoverable(t *testing.T) {
	for _, s := range []State{StateUnknown, StateUnknownProcess, StateStarting, StateHealthy, StateDegraded, StateUnhealthy} {
		if !s.Recoverable() {
			t.Errorf("%v should be recoverable", s)
		}
	}
	if StateFailed.Recoverable() {
		t.Error("StateFailed must not be recoverable")
	}
}

func TestStateReported(t *testing.T) {
	if StateUnknown.Reported() || StateUnknownProcess.Reported() {
		t.Error("liveness-derived states are not reports")
	}
	for _, s := range []State{StateStarting, StateHealthy, StateDegraded, StateUnhealthy, StateFailed} {
		if !s.Reported() {
			t.Errorf("%v is a reported state", s)
		}
	}
}

func TestStateLevel(t *testing.T) {
	want := map[State]slog.Level{
		StateHealthy:   slog.LevelDebug,
		StateStarting:  slog.LevelInfo,
		StateDegraded:  slog.LevelWarn,
		StateUnhealthy: slog.LevelError,
		StateFailed:    slog.LevelError,
	}
	for st, lvl := range want {
		if got := st.Level(); got != lvl {
			t.Errorf("%v.Level() = %v, want %v", st, got, lvl)
		}
	}
}

func TestConstructors(t *testing.T) {
	cause := errors.New("connection refused")

	if e := Healthy("serving %s", "ok"); e.State != StateHealthy || e.Reason != "serving ok" {
		t.Errorf("Healthy: %+v", e)
	}
	if e := Starting("waiting for cert"); e.State != StateStarting {
		t.Errorf("Starting: %+v", e)
	}
	d := Degraded(cause, "slow %d%%", 90)
	if d.State != StateDegraded || d.Unwrap() != cause || !strings.Contains(d.Error(), "slow 90%") {
		t.Errorf("Degraded: %+v (%q)", d, d.Error())
	}
	u := Unhealthy(cause, "etcd unreachable")
	if !u.Recoverable() {
		t.Error("Unhealthy must be recoverable")
	}
	f := Failed(cause, "cert %s unusable", "webhook")
	if f.Recoverable() || !f.Fatal() {
		t.Error("Failed must be fatal")
	}
	if !strings.Contains(f.Error(), "cert webhook unusable") {
		t.Errorf("Failed message: %q", f.Error())
	}
}

func TestErrorDetailAndIs(t *testing.T) {
	e := Failed(nil, "missing %s", "material")
	e.Detail = "pki store empty"

	if got := e.Error(); got != "missing material: pki store empty" {
		t.Errorf("Error() = %q", got)
	}

	if !errors.Is(e, ErrFailed) {
		t.Error("errors.Is(e, ErrFailed) should be true")
	}
	for _, s := range []error{ErrHealthy, ErrStarting, ErrDegraded, ErrUnhealthy} {
		if errors.Is(e, s) {
			t.Errorf("errors.Is(failed, %v) should be false", s)
		}
	}
	// A wrapped fatal stays matchable.
	wrapped := fmt.Errorf("start webhook: %w", e)
	if !errors.Is(wrapped, ErrFailed) {
		t.Error("wrapped fatal should still match ErrFailed")
	}
	// And the cause chain survives.
	cause := errors.New("boom")
	if !errors.Is(Failed(cause, "x"), cause) {
		t.Error("cause should be unwrappable")
	}
}

func TestFromError(t *testing.T) {
	if e := FromError(nil); e.State != StateHealthy {
		t.Errorf("nil -> %+v, want healthy", e)
	}
	if e := FromError(Unhealthy(nil, "flapping")); e.State != StateUnhealthy {
		t.Errorf("*Error passthrough: %+v", e)
	}
	plain := errors.New("dial tcp: refused")
	e := FromError(plain)
	if e.State != StateUnhealthy || !e.Recoverable() {
		t.Errorf("plain error -> %+v, want recoverable unhealthy", e)
	}
	if e.Unwrap() != plain {
		t.Error("plain error should be wrapped, not dropped")
	}
	var nilErr *Error
	if e := FromError(nilErr); e.State != StateHealthy {
		t.Errorf("typed nil *Error -> %+v, want healthy", e)
	}
}

func TestWrap(t *testing.T) {
	if Wrap(nil, StateFailed, "x") != nil {
		t.Error("Wrap(nil) should be nil")
	}
	cause := errors.New("eacces")
	if e := Wrap(cause, StateFailed, "cannot write %s", "key"); e.State != StateFailed || !strings.Contains(e.Reason, "key") {
		t.Errorf("Wrap: %+v", e)
	}
	// Wrapping a status in the same state is idempotent.
	orig := Unhealthy(cause, "down")
	if Wrap(fmt.Errorf("ctx: %w", orig), StateUnhealthy, "ignored") != orig {
		t.Error("Wrap should pass an already-classified error of the same state through")
	}
}

func TestLogValue(t *testing.T) {
	e := Failed(errors.New("boom"), "no cert")
	e.Detail = "detail here"
	v := e.LogValue()
	g := v.Group()
	keys := map[string]string{}
	for _, a := range g {
		keys[a.Key] = a.Value.String()
	}
	if keys["state"] != "failed" {
		t.Errorf("state attr: %v", keys)
	}
	if keys["reason"] != "no cert" || keys["detail"] != "detail here" {
		t.Errorf("attrs: %v", keys)
	}
	if _, ok := keys["cause"]; !ok {
		t.Errorf("expected a cause attr: %v", keys)
	}
}

func TestIdentity(t *testing.T) {
	for _, tc := range [][3]string{
		{"api", "0", "api"},
		{"api", "2", "api-2"},
		{"webhook", "-1", "webhook"},
	} {
		if got := Identity(tc[0], atoi(tc[1])); got != tc[2] {
			t.Errorf("Identity(%q, %s) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

func atoi(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

func TestSnapshotStringAndAge(t *testing.T) {
	snap := Snapshot{
		Identity: "webhook/validate",
		State:    StateUnhealthy,
		Reason:   "etcd unreachable",
		Detail:   "dial tcp",
		Since:    time.Now().Add(-90 * time.Second),
	}
	got := snap.String()
	for _, want := range []string{"UNHEALTHY", "etcd unreachable", "dial tcp", "1m30s"} {
		if !strings.Contains(got, want) {
			t.Errorf("Snapshot.String() = %q, missing %q", got, want)
		}
	}
	if snap.Age() < 89*time.Second {
		t.Errorf("Age() = %v", snap.Age())
	}
	if (Snapshot{}).Age() != 0 {
		t.Error("zero Since should give a zero Age")
	}
}
