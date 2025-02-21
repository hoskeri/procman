package termhandler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
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
	"\033[38;5;2m",
	"\033[38;5;3m",
	"\033[38;5;4m",
	"\033[38;5;5m",
	"\033[38;5;6m",
	"\033[38;5;9m",
	"\033[38;5;10m",
	"\033[38;5;11m",
	"\033[38;5;12m",
}

func randomColor() string {
	return fgcolors[rand.Intn(len(fgcolors))]
}

type Options struct {
	Level   slog.Leveler
	Columns int
	Colors  bool
}

type TermHandler struct {
	opts  Options
	group string
	color string
	attrs []slog.Attr
	mu    *sync.Mutex
	out   io.Writer
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

	a, err := out.SyscallConn()
	if err == nil {
		a.Control(func(fd uintptr) {
			h.opts.Colors = terminal.IsTerminal(int(fd))
		})
	}

	return h
}

func (h *TermHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.opts.Level.Level()
}

func (h *TermHandler) Handle(ctx context.Context, rec slog.Record) error {
	if len(h.group) == 0 {
		return nil
	}

	buf := []byte(ansiBold + h.color + h.group + ansiReset + rec.Message)
	l := len(buf)

	if h.opts.Columns > 0 {
		if l > h.opts.Columns {
			l = h.opts.Columns
		}
	}

	if buf[l-1] != '\n' {
		buf[l-1] = '\n'
	}

	h.mu.Lock()
	h.mu.Unlock()

	_, err := h.out.Write(buf[0:l])
	return err
}

func (h *TermHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *TermHandler) WithGroup(name string) slog.Handler {
	h2 := *h
	h2.group = fmt.Sprintf("%16s | ", name)
	if h2.opts.Colors {
		h2.color = randomColor()
	}
	return &h2
}
