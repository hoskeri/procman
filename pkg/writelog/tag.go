package writelog

import "log/slog"

// TagHandler is implemented by procman sink handlers (the terminal renderer
// and FramerHandler) that track a display tag path separately from slog
// attribute groups.
//
// WithGroup always denotes an attribute namespace and never changes the
// displayed prefix; WithTag extends the display path that becomes the colored
// prefix (and the Frame.Tag on the wire).
//
// Handlers without this interface (a plain slog.TextHandler) receive the tag
// through WithGroup, and WithTag falls back to that.
type TagHandler interface {
	slog.Handler
	WithTag(name string) slog.Handler
}

// WithTag extends logger's display tag path when its handler supports it,
// falling back to WithGroup for ordinary slog handlers.
func WithTag(logger *slog.Logger, name string) *slog.Logger {
	if th, ok := logger.Handler().(TagHandler); ok {
		return slog.New(th.WithTag(name))
	}
	return logger.WithGroup(name)
}

// TagCapable reports whether logger's handler tracks a separate display tag
// path (renderer or FramerHandler).
func TagCapable(logger *slog.Logger) bool {
	_, ok := logger.Handler().(TagHandler)
	return ok
}

// TaggedSink is WithTag for procman's own line sinks.  When the sink is not
// tag-capable (a plain slog.TextHandler), it also attaches a "tag" attribute
// so the tag stays visible in the plain rendering, mirroring the historical
// Stream/relay behavior.
func TaggedSink(logger *slog.Logger, name string) *slog.Logger {
	if th, ok := logger.Handler().(TagHandler); ok {
		return slog.New(th.WithTag(name))
	}
	return logger.WithGroup(name).With(slog.String("tag", name))
}
