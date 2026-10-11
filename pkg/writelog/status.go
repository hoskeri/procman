package writelog

import (
	"context"
	"log/slog"
	"time"
)

// Status wire contract (see pkg/status for the domain vocabulary).
//
// A status record is an ordinary framed slog record carrying two reserved
// attributes, so it needs no new wire format and interoperates with a parent
// that knows nothing about status: it just renders the line like any other.

// statusEmitter is implemented by a handler that carries records across a
// process boundary (a nested FramerHandler): it can emit a status record
// without applying the level policy, because the receiving side keeps a
// level-independent status registry and applies its own level when it
// renders.  A root renderer does not implement it, so its status lines stay
// subject to the level policy.
type statusEmitter interface {
	emitStatus(rec slog.Record)
}

// EmitStatus logs a status record, bypassing the level policy when logger's
// handler is a process boundary.  It is the transport half of the status path
// and is what makes a filtered-out `healthy` line still update the parent's
// registry under `--log-level error`.
func EmitStatus(ctx context.Context, logger *slog.Logger, level slog.Level, msg string, attrs ...slog.Attr) {
	if logger == nil {
		return
	}
	if em, ok := logger.Handler().(statusEmitter); ok {
		var rec slog.Record
		rec.Time = time.Now()
		rec.Level = level
		rec.Message = msg
		rec.AddAttrs(attrs...)
		em.emitStatus(rec)
		return
	}
	logger.LogAttrs(ctx, level, msg, attrs...)
}

// Reserved attribute names.  They follow the existing synthetic-attribute
// convention (like "tag" and "stream"): framers carry them on the wire and
// renderers filter them from the formatted attribute list, because the state
// is already part of the rendered message.
const (
	// AttrStatus carries the status.State name (see status.State.String).
	AttrStatus = "status"
	// AttrSince carries how long the state has been held.
	AttrSince = "since"
	// AttrDetail carries an optional longer explanation.
	AttrDetail = "detail"
)
