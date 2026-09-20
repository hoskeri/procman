package termhandler

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestHandler returns a TermHandler writing into a buffer, so tests don't
// need a real *os.File (New requires one for terminal detection).
func newTestHandler(global slog.Level) (*TermHandler, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &TermHandler{
		out:     buf,
		mu:      &sync.Mutex{},
		palette: fgcolors,
		opts:    Options{Level: global},
	}, buf
}

// TestPerGroupLevelOverride verifies that WithOverride only affects the named
// group while other groups inherit the global Options.Level.
func TestPerGroupLevelOverride(t *testing.T) {
	th, buf := newTestHandler(slog.LevelInfo)

	// Only the "quiet" group is overridden up; "web" inherits the global Info.
	web := slog.New(th.WithGroup("web"))
	quiet := slog.New(th.WithGroup("quiet").(*TermHandler).WithOverride("quiet", slog.LevelError))

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

	verbose := slog.New(th.WithGroup("verbose").(*TermHandler).WithOverride("verbose", slog.LevelDebug))
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

// TestColumnsTruncation verifies that Options.Columns truncates a log message
// to at most Columns visible bytes (ANSI escapes occupy no columns) before it
// reaches the output writer.
func TestColumnsTruncation(t *testing.T) {
	th, buf := newTestHandler(slog.LevelInfo)
	th.opts.Columns = 8

	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, strings.Repeat("a", 11), 0)
	if err := th.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got, want := buf.String(), "aaaaaaaa\n"; got != want {
		t.Errorf("Columns=8 truncation: want %q, got %q", want, got)
	}
}

// TestColumnsTruncationAnsi verifies that the colored prefix's ANSI escape
// sequences do not consume the truncation budget: with Columns=21 the line
// keeps the full colored prefix (19 visible chars) plus exactly 2 payload
// bytes, even though the raw byte length of the prefix alone far exceeds the
// budget — a naive byte-counting truncator would cut inside the prefix and
// drop every payload byte.
func TestColumnsTruncationAnsi(t *testing.T) {
	th, buf := newTestHandler(slog.LevelInfo)
	th.opts.Colors = true
	th.opts.Columns = 21
	th.groupPath = []string{"web"}
	th.linePrefix = th.buildPrefix()

	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, strings.Repeat("a", 30), 0)
	if err := th.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	out := buf.String()
	if got := strings.Count(out, "a"); got != 2 {
		t.Errorf("escape-aware truncation: want 2 visible a's, got %d (line %q)", got, out)
	}
	if !strings.HasSuffix(out, "aa\n") {
		t.Errorf("expected the line to end with %q, got %q", "aa\\n", out)
	}
}

// TestNewColorsForced verifies that Options.Colors=true forces color even on a
// non-terminal (so --output term works on piped stdout), while the default
// auto-detects (color off for a pipe).
func TestNewColorsForced(t *testing.T) {
	// A pipe end is not a terminal.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	if !New(w, &Options{Colors: true}).opts.Colors {
		t.Error("Options.Colors=true should force color even when not a terminal")
	}
	if New(w, &Options{}).opts.Colors {
		t.Error("non-terminal without forced color should not enable color")
	}
	if New(w, &Options{}).opts.Columns != 0 {
		t.Error("Options.Columns should default to 0 for non-terminal output (truncation off)")
	}
	if New(w, &Options{Columns: -1}).opts.Columns != -1 {
		t.Error("negative Options.Columns should be preserved (truncation disabled)")
	}
}

// TestIsTerminal reports correctness of the helper used by main for --output.
func TestIsTerminal(t *testing.T) {
	if IsTerminal(nil) {
		t.Error("IsTerminal(nil) should be false")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(w) {
		t.Error("IsTerminal(pipe) should be false")
	}
}

// TestColorSupport verifies the COLORTERM/TERM heuristics used to select the
// palette depth (0 = 16 colors, 1 = 256, 2 = 24-bit).
func TestColorSupport(t *testing.T) {
	oldCt, oldTerm := os.Getenv("COLORTERM"), os.Getenv("TERM")
	defer func() {
		os.Setenv("COLORTERM", oldCt)
		os.Setenv("TERM", oldTerm)
	}()

	tests := []struct {
		ct    string
		term  string
		want  int
	}{
		{"", "", 0},
		{"", "xterm", 0},
		{"", "linux", 0},
		{"", "xterm-256color", 1},
		{"", "tmux-256color", 1},
		{"256color", "xterm", 1},
		{"truecolor", "xterm", 2},
		{"24bit", "xterm-256color", 2},
	}
	for _, tt := range tests {
		os.Setenv("COLORTERM", tt.ct)
		os.Setenv("TERM", tt.term)
		if got := colorSupport(); got != tt.want {
			t.Errorf("colorSupport(COLORTERM=%q, TERM=%q): got %d, want %d", tt.ct, tt.term, got, tt.want)
		}
	}
}

// TestPaletteFor verifies the palette shapes and that the expanded palettes
// avoid the white and near-white ANSI slots.
func TestPaletteFor(t *testing.T) {
	base := PaletteFor(0)
	if len(base) == 0 {
		t.Error("16-color palette must not be empty")
	}

	p256 := PaletteFor(1)
	if len(p256) < len(base) {
		t.Errorf("256-color palette should grow: %d < %d", len(p256), len(base))
	}
	for _, c := range p256 {
		if !strings.HasPrefix(c, "\033[38;5;") {
			t.Errorf("256-color entry %q not an ANSI 256-color prefix", c)
		}
		// Whites/greys to avoid: 7, 15, 231 (cube white), 251-255 (grays).
		for _, w := range []string{";7m", ";15m", ";231m", ";251m", ";252m", ";253m", ";254m", ";255m"} {
			if strings.HasSuffix(c, w) {
				t.Errorf("256-color palette must not use near-white slot %s (%q)", w, c)
			}
		}
	}

	true := PaletteFor(2)
	if len(true) < len(base) {
		t.Errorf("24-bit palette should grow: %d < %d", len(true), len(base))
	}
	for _, c := range true {
		if !strings.HasPrefix(c, "\033[38;2;") {
			t.Errorf("24-bit entry %q not an ANSI truecolor prefix", c)
		}
		if strings.Contains(c, "255;255;255") {
			t.Errorf("24-bit palette must not contain pure white (%q)", c)
		}
	}
}

// TestNoEchoUntaintedPipe verifies NoEcho is a no-op for non-terminals and
// that the returned restore function can be called harmlessly.
func TestNoEchoUntaintedPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	restore := NoEcho(w)
	restore() // must not crash or alter anything
	NoEcho(nil)()
}

func TestShortenMiddle(t *testing.T) {
	tests := []struct {
		input   string
		maxLen  int
		want    string
	}{
		{"web", 16, "web"},
		{"node-1/kubelet", 16, "node-1/kubelet"},
		{"node-1/kubelet/cri", 16, "node-1/...et/cri"},
		{"node-1/kubelet/cri-server", 16, "node-1/...server"},
		{"node-1/kubelet/cri-server", 8, "nod...er"},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"a", 1, "a"},
		{"", 16, ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shortenMiddle(tt.input, tt.maxLen)
			if len(got) > tt.maxLen {
				t.Errorf("length: got %d, want <= %d", len(got), tt.maxLen)
			}
			if got != tt.want {
				t.Errorf("shortenMiddle(%q, %d): got %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}
