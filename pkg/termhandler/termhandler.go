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
	group      string
	name       string
	override   slog.Leveler
	color      string
	linePrefix string
	attrs      []slog.Attr
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

	// Colors explicitly set to true forces color even when the output is not a
	// terminal (e.g. --output term on a piped stdout). Otherwise color is
	// auto-detected: enabled only when out is a terminal.
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

// WithOverride returns a handler for the given group whose minimum log level
// is lvl, overriding the inherited Options.Level for that group only. The
// original level is preserved, so sub-groups still inherit it unless they too
// are overridden.
func (h *TermHandler) WithOverride(name string, lvl slog.Leveler) slog.Handler {
	h2 := h.groupHandler(name)
	h2.override = lvl
	return h2
}

func (h *TermHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if h.name == "" {
		return false
	}

	// opts.Level is the inherited original level; override, when set, is the
	// per-process threshold. Both are plain immutable fields on this handler,
	// so no locking is needed here.
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

	// Can't possibly be efficient.
	buf := []byte(h.linePrefix + rec.Message)

	if len(buf) == 0 {
		return nil
	}

	l := len(buf)
	if h.opts.Columns > 0 && l > h.opts.Columns {
		l = h.opts.Columns
	}
	// Emit at most l bytes, ensuring a trailing newline so lines stay intact
	// for downstream consumers.
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
	h2 := *h
	h2.attrs = append(h2.attrs, attrs...)
	return &h2
}

func (h *TermHandler) groupHandler(name string) *TermHandler {
	h2 := *h
	h2.name = name
	h2.group = fmt.Sprintf("%16s | ", name)
	if h2.opts.Colors {
		h2.color = randomColor(name)
	}
	h2.linePrefix = string(ansiBold + h2.color + h2.group + ansiReset)
	return &h2
}

func (h *TermHandler) WithGroup(name string) slog.Handler {
	return h.groupHandler(name)
}
