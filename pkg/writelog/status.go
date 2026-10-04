package writelog

// Status wire contract (see pkg/status for the domain vocabulary).
//
// A status record is an ordinary framed slog record carrying two reserved
// attributes, so it needs no new wire format and interoperates with a parent
// that knows nothing about status: it just renders the line like any other.

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
