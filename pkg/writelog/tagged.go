package writelog

import "log/slog"

// TaggedLogger returns a *slog.Logger for a named component hosted inside the
// current process.
//
// It exposes the same framing that nested procman formations use to multiplex
// their records, but as an ordinary slog logger.  When the process was
// launched by a procman formation (its stdout or stderr is a parent's
// SOCK_SEQPACKET log channel), the returned logger frames every record under
// tag and the parent renders the component as a sub-tag of the process — for
// example "webhook/authn".  Several components can therefore share one
// process while still presenting distinct, colored prefixes.
//
// The tag is carried as a display tag path (Frame.Tag) and rendered by the
// parent under the process tag; slog WithGroup remains available for ordinary
// attribute namespaces.
//
// When the process is not nested, the helper degrades gracefully to
// slog.Default() grouped under tag, so it is safe to call from any process
// (a library used both stand-alone and under procman).
//
// The standard streams are probed on every call; call it once per component
// (typically at startup) and retain the returned logger.  In nested mode the
// parent resolves log levels per component from the record's group path, so an
// override may name the process tag ("webhook"), the full path
// ("webhook/validate"), or the component alone ("validate").
func TaggedLogger(tag string) *slog.Logger {
	return taggedLogger(tag, ChildSinks)
}

// taggedLogger is TaggedLogger with an injected nested-sink probe so tests can
// supply socket-backed framers without touching fd 1/2.
func taggedLogger(tag string, probe func() (stdout, stderr *slog.Logger)) *slog.Logger {
	// A framer already installed as the process default (procman's own main
	// does this for nested formations) is reused: it carries the configured
	// level and avoids a second framer over the same fd.
	if _, ok := slog.Default().Handler().(*FramerHandler); ok {
		return WithTag(slog.Default(), tag)
	}

	stdout, stderr := probe()
	switch {
	case stderr != nil:
		return WithTag(stderr, tag)
	case stdout != nil:
		return WithTag(stdout, tag)
	default:
		return WithTag(slog.Default(), tag)
	}
}
