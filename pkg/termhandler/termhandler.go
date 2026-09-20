package termhandler

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/nerdmaster/terminal"
)

// ANSI modes
const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
)

// echoFlag is the ECHO bit of c_lflag; its value (0x8) is identical across
// the Unices procman targets (see syscall/zerrors_*; Linux only defines the
// ECHO* variants, so it is spelled out here).
const echoFlag = 0x8

var fgcolors = []string{
	"\033[38;5;1m",
	"\033[38;5;2m",
	"\033[38;5;3m",
	"\033[38;5;4m",
	"\033[38;5;5m",
	"\033[38;5;6m",
	"\033[38;5;9m",
	"\033[38;5;10m",
	"\033[38;5;11m",
	"\033[38;5;12m",
	"\033[38;5;13m",
	"\033[38;5;15m",
}

// fgcolors256 spreads 16 hues around the wheel on the 6x6x6 color cube,
// skipping the grays and whites (7, 15, 231, 251-255) so every entry stays
// saturated and readable on a dark background.
var fgcolors256 = []string{
	"\033[38;5;196m", // red
	"\033[38;5;208m", // orange
	"\033[38;5;214m", // goldenrod
	"\033[38;5;226m", // yellow
	"\033[38;5;154m", // chartreuse
	"\033[38;5;46m",  // green
	"\033[38;5;48m",  // spring green
	"\033[38;5;51m",  // cyan
	"\033[38;5;45m",  // turquoise
	"\033[38;5;39m",  // azure
	"\033[38;5;27m",  // blue
	"\033[38;5;63m",  // periwinkle
	"\033[38;5;99m",  // violet
	"\033[38;5;129m", // purple
	"\033[38;5;165m", // magenta
	"\033[38;5;201m", // pink
}

// fgcolorsTrue is the 24-bit analogue: saturated hues evenly spaced around
// the wheel, none of them white or near-white.
var fgcolorsTrue = []string{
	"\033[38;2;255;0;0m",      // red
	"\033[38;2;255;128;0m",    // orange
	"\033[38;2;255;192;64m",   // amber
	"\033[38;2;255;255;0m",    // yellow
	"\033[38;2;160;255;64m",   // chartreuse
	"\033[38;2;0;255;0m",      // green
	"\033[38;2;0;255;160m",    // spring green
	"\033[38;2;0;255;255m",    // cyan
	"\033[38;2;0;192;255m",    // sky
	"\033[38;2;0;120;255m",    // azure
	"\033[38;2;48;80;255m",    // blue
	"\033[38;2;120;72;255m",   // periwinkle
	"\033[38;2;180;0;255m",    // purple
	"\033[38;2;255;0;255m",    // magenta
	"\033[38;2;255;96;192m",   // pink
	"\033[38;2;255;128;128m",  // coral
}

// colorSupport probes the environment the way common terminal tools do:
// COLORTERM=truecolor|24bit selects 24-bit color, COLORTERM=256color or a
// TERM ending in -256color selects the 256-color palette, and everything else
// falls back to the basic 16-color ANSI range.
func colorSupport() int {
	ct := os.Getenv("COLORTERM")
	if ct == "truecolor" || ct == "24bit" {
		return 2
	}
	if ct == "256color" || strings.Contains(os.Getenv("TERM"), "256color") {
		return 1
	}
	return 0
}

// PaletteFor returns the ANSI prefix palette for a color depth from
// [colorSupport]: 0 → 16 colors, 1 → 256 colors, 2 → 24-bit truecolor.
func PaletteFor(depth int) []string {
	if depth >= 2 {
		return fgcolorsTrue
	}
	if depth == 1 {
		return fgcolors256
	}
	return fgcolors
}

type Options struct {
	Level slog.Leveler
	// Columns truncates each emitted line (prefix included) to this many
	// bytes. 0 (the default) auto-resolves to the terminal width when the
	// output is a tty, leaving truncation off otherwise; negative values
	// disable truncation explicitly.
	Columns int
	Colors  bool
}

type TermHandler struct {
	opts       Options
	groupPath  []string       // accumulated group path (innermost is last)
	override   slog.Leveler   // per-process level threshold (via WithOverride)
	color      string         // ANSI color for the innermost group
	attrs      []slog.Attr
	palette    []string       // ANSI prefixes selected by colorSupport
	linePrefix string         // cached render of the full group path
	prefixVis  int            // visible width of linePrefix (see buildPrefix)
	mu         *sync.Mutex
	out        io.Writer
}

var _ slog.Handler = &TermHandler{}

func New(out *os.File, opts *Options) *TermHandler {
	h := &TermHandler{out: out, mu: &sync.Mutex{}}
	if opts != nil {
		h.opts = *opts
	}
	if h.opts.Level == nil {
		h.opts.Level = slog.LevelInfo
	}
	// Colors auto-detect: enabled only when out is a terminal; --output term
	// forces them via Options.Colors = true before calling New.
	h.opts.Colors = h.opts.Colors || IsTerminal(out)
	// Palette selection mirrors the terminal's color depth (see colorSupport).
	h.palette = PaletteFor(colorSupport())
	// Columns auto-resolve: 0 (the default) means "terminal width" when out
	// is a tty, else no truncation. Negative values keep truncation off.
	if h.opts.Columns == 0 {
		h.opts.Columns = TerminalWidth(out)
	}
	return h
}

// TerminalWidth reports the width in columns of f when it is a terminal, or 0
// otherwise (including when f is nil or the size query fails). It backs the
// Options.Columns auto-resolution in New.
func TerminalWidth(f *os.File) int {
	if f == nil {
		return 0
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return 0
	}
	var width int
	_ = conn.Control(func(fd uintptr) {
		if w, _, werr := terminal.GetSize(int(fd)); werr == nil && w > 0 {
			width = w
		}
	})
	return width
}

// IsTerminal reports whether f is a terminal. It mirrors the check TermHandler
// performs internally so callers can branch on tty-ness (e.g. choosing an
// output handler) without constructing a handler.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var isTerm bool
	_ = conn.Control(func(fd uintptr) {
		isTerm = terminal.IsTerminal(int(fd))
	})
	return isTerm
}

// NoEcho disables terminal echo on f when f is a terminal and returns a
// function that restores the previous terminal state. It returns a no-op when
// f is not a terminal or the termios round-trip fails. procman never reads
// stdin, so without this, keystrokes on the controlling terminal are echoed
// into the streaming log output — the caller restores the state on every exit
// path (os.Exit bypasses defer).
func NoEcho(f *os.File) func() {
	if f == nil {
		return func() {}
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return func() {}
	}
	var saved syscall.Termios
	var ok bool
	_ = conn.Control(func(fd uintptr) {
		if _, _, ierr := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&saved)), 0, 0, 0); ierr == 0 {
			off := saved
			off.Lflag &^= echoFlag
			if _, _, oerr := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&off)), 0, 0, 0); oerr == 0 {
				ok = true
			}
		}
	})
	if !ok {
		return func() {}
	}
	return func() {
		_ = conn.Control(func(fd uintptr) {
			syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&saved)), 0, 0, 0)
		})
	}
}

// colorFor hashes tag with FNV-1a to assign a consistent entry from the
// handler's palette (chosen by colorSupport at New time).
func (h *TermHandler) colorFor(tag string) string {
	hv := fnv.New32()
	hv.Write([]byte(tag))
	return h.palette[int(hv.Sum32())%len(h.palette)]
}

// buildPrefix returns the full group path prefix for the current
// groupPath, optionally colored. Groups are joined with "/" — the restricted
// tag character set (lowercase alphanumeric + dash) guarantees no ambiguity.
func (h *TermHandler) buildPrefix() string {
	// innermost group for color
	innermost := ""
	if len(h.groupPath) > 0 {
		innermost = h.groupPath[len(h.groupPath)-1]
	}

	// Combine all groups into a single path, right-justified in 16 columns.
	combined := strings.Join(h.groupPath, "/")
	padded := fmt.Sprintf("%16s", shortenMiddle(combined, 16))
	// The uncolored prefix is padded (always exactly 16 ASCII bytes, since
	// tags are restricted to lowercase alphanumerics and dashes) plus
	// " | " — so its visible width is known without any escape scanning.
	h.prefixVis = len(padded) + 3
	prefix := padded + " | "

	if h.opts.Colors && innermost != "" {
		c := h.colorFor(innermost)
		return string(ansiBold) + c + prefix + ansiReset
	}
	return prefix
}

// shortenMiddle truncates s to fit maxLen by replacing the middle section
// with "...".  If s already fits it is returned unchanged.
func shortenMiddle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	ellipsis := "..."
	avail := maxLen - len(ellipsis)
	if avail < 2 {
		return s[:maxLen]
	}
	left := (avail + 1) / 2 // left-heavy bias
	right := avail - left
	return s[:left] + ellipsis + s[len(s)-right:]
}

// WithOverride returns a handler that overrides the minimum log level for
// the upcoming group (set by the caller's subsequent WithGroup).  Unlike the
// old behavior, WithOverride does NOT set the group path — it only stores
// the level threshold; the group is added by writelog's WithGroup, avoiding
// group name duplication.
func (h *TermHandler) WithOverride(name string, lvl slog.Leveler) slog.Handler {
	h2 := h.clone()
	h2.override = lvl
	return h2
}

func (h *TermHandler) clone() *TermHandler {
	h2 := *h
	h2.groupPath = append([]string(nil), h.groupPath...)
	h2.attrs = append([]slog.Attr(nil), h.attrs...)
	return &h2
}

func (h *TermHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if len(h.groupPath) == 0 {
		// Bare root handler — not yet associated with any process group;
		// slog never calls Handle on it.
		return false
	}
	threshold := h.opts.Level
	if h.override != nil {
		threshold = h.override
	}
	return l >= threshold.Level()
}

func (h *TermHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message == "" {
		return nil
	}

	// Trim the message *before* prepending the colored prefix: the prefix's
	// visible width is fixed (prefixVis), so the budget left for the payload
	// is Columns minus that. No escape scanning is needed — the ANSI codes
	// are added afterwards and never land in the trimmed region. Escapes
	// inside child output still count as bytes here (they are rare, and a
	// cut inside one is self-healing: an ESC begins every new line and
	// aborts any dangling sequence).
	msg := rec.Message
	if h.opts.Columns > 0 {
		avail := h.opts.Columns - h.prefixVis
		if avail < 0 {
			avail = 0
		}
		if len(msg) > avail {
			msg = msg[:avail]
		}
	}

	out := []byte(h.linePrefix + msg)
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.out.Write(out)
	return err
}

func (h *TermHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h2 := h.clone()
	h2.attrs = append(h2.attrs, attrs...)
	return h2
}

func (h *TermHandler) WithGroup(name string) slog.Handler {
	// Append the new group to the path and recompute prefix/color.
	h2 := h.clone()
	h2.groupPath = append(h2.groupPath, name)
	h2.linePrefix = h2.buildPrefix()
	return h2
}