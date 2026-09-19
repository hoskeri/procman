package termhandler

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/nerdmaster/terminal"
)

// ANSI modes
const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
)

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

func randomColor(tag string) string {
	h := fnv.New32()
	h.Write([]byte(tag))
	return fgcolors[int(h.Sum32())%len(fgcolors)]
}

type Options struct {
	Level   slog.Leveler
	Columns int
	Colors  bool
}

type TermHandler struct {
	opts       Options
	groupPath  []string       // accumulated group path (innermost is last)
	override   slog.Leveler   // per-process level threshold (via WithOverride)
	color      string         // ANSI color for the innermost group
	attrs      []slog.Attr
	linePrefix string         // cached render of the full group path
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
	return h
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

// buildPrefix returns the full group path prefix string for the current
// groupPath, optionally colored. Color is derived from the innermost group.
func (h *TermHandler) buildPrefix() string {
	// innermost group for color
	innermost := ""
	if len(h.groupPath) > 0 {
		innermost = h.groupPath[len(h.groupPath)-1]
	}

	var prefix string
	for _, g := range h.groupPath {
		prefix += fmt.Sprintf("%16s | ", g)
	}

	if h.opts.Colors && innermost != "" {
		c := randomColor(innermost)
		return string(ansiBold) + c + prefix + ansiReset
	}
	return prefix
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

	buf := []byte(h.linePrefix + rec.Message)
	if len(buf) == 0 {
		return nil
	}

	l := len(buf)
	if h.opts.Columns > 0 && l > h.opts.Columns {
		l = h.opts.Columns
	}
	out := buf[:l]
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