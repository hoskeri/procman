package termhandler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// ANSI modes
const (
	ansiReset          = "\033[0m"
	ansiFaint          = "\033[2m"
	ansiResetFaint     = "\033[22m"
	ansiBrightRed      = "\033[91m"
	ansiBrightGreen    = "\033[92m"
	ansiBrightYellow   = "\033[93m"
	ansiBrightRedFaint = "\033[91;2m"
)

type Options struct {
	Level   slog.Leveler
	Columns int
}

type TermHandler struct {
	opts  Options
	group string
	attrs []slog.Attr
	mu    *sync.Mutex
	out   io.Writer
}

var _ slog.Handler = &TermHandler{}

func New(out io.Writer, opts *Options) *TermHandler {
	h := &TermHandler{out: out, mu: &sync.Mutex{}}
	if opts != nil {
		h.opts = *opts
	}

	if h.opts.Level == nil {
		h.opts.Level = slog.LevelInfo
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

	buf := []byte(ansiBrightGreen + h.group + ansiReset + rec.Message)
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
	return &h2
}
