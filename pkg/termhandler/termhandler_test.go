package termhandler

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// newTestHandler returns a TermHandler writing into a buffer, so tests don't
// need a real *os.File (New requires one for terminal detection).
func newTestHandler(global slog.Level) (*TermHandler, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &TermHandler{
		out:  buf,
		mu:   &sync.Mutex{},
		opts: Options{Level: global},
	}, buf
}

// TestPerGroupLevelOverride verifies that WithOverride only affects the named
// group while other groups inherit the global Options.Level.
func TestPerGroupLevelOverride(t *testing.T) {
	th, buf := newTestHandler(slog.LevelInfo)

	// Only the "quiet" group is overridden up; "web" inherits the global Info.
	web := slog.New(th.WithGroup("web"))
	quiet := slog.New(th.WithOverride("quiet", slog.LevelError))

	web.Info("web info")       // Info >= Info (global) -> logged
	web.Error("web error")     // logged
	quiet.Info("quiet info")   // Info < Error (override) -> suppressed
	quiet.Error("quiet error") // Error >= Error -> logged

	out := buf.String()
	t.Logf("handler output:\n%s", out)

	for _, want := range []string{"web info", "web error", "quiet error"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "quiet info") {
		t.Error("expected \"quiet info\" to be suppressed by the per-group Error override")
	}
}

// TestPerGroupLevelOverrideDown verifies a per-group override can also lower
// the threshold below the global level, restoring verbose output.
func TestPerGroupLevelOverrideDown(t *testing.T) {
	th, buf := newTestHandler(slog.LevelWarn)

	verbose := slog.New(th.WithOverride("verbose", slog.LevelDebug))
	verbose.Info("verbose info") // Info >= Debug (override) -> shown despite global Warn

	if !strings.Contains(buf.String(), "verbose info") {
		t.Errorf("expected verbose info to be shown via per-group Debug override, got:\n%s", buf.String())
	}

	// Control: a group without an override stays suppressed at global Warn.
	buf.Reset()
	normal := slog.New(th.WithGroup("normal"))
	normal.Info("normal info")
	if strings.Contains(buf.String(), "normal info") {
		t.Error("expected normal info to be suppressed by the global Warn level")
	}
}

// TestOverrideSurvivesReGroup verifies that the override survives the extra
// WithGroup/WithAttrs wrapping that writelog applies on top of the
// per-process logger, and that the root handler is left untouched.
func TestOverrideSurvivesReGroup(t *testing.T) {
	th, buf := newTestHandler(slog.LevelInfo)

	// Simulate Formation.Run + writelog: build the per-process logger, then
	// re-group it the way writelog.Stream does.
	procLogger := slog.New(th.WithOverride("quiet", slog.LevelError))
	wrapped := procLogger.WithGroup("quiet").With(slog.String("tag", "quiet"))

	wrapped.Info("quiet info")   // Info < Error -> suppressed
	wrapped.Error("quiet error") // shown

	out := buf.String()
	if strings.Contains(out, "quiet info") {
		t.Error("expected quiet info to stay suppressed after re-grouping")
	}
	if !strings.Contains(out, "quiet error") {
		t.Errorf("expected quiet error to be logged after re-grouping, got:\n%s", out)
	}

	// The root handler must be unaffected: a fresh group still inherits Info.
	buf.Reset()
	web := slog.New(th.WithGroup("web"))
	web.Info("web info")
	if !strings.Contains(buf.String(), "web info") {
		t.Error("expected web info to be logged at the inherited global level")
	}
}
