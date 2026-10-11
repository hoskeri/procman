package process

import (
	"log/slog"

	"github.com/hoskeri/procman/pkg/loglevels"
)

// The log-level policy lives in pkg/loglevels.  These aliases keep the
// historical process.* names working for existing embedders (e.g. runkube);
// new code should import pkg/loglevels directly.
type LogLevels = loglevels.LogLevels

// ParseLevel is re-exported from pkg/loglevels.
func ParseLevel(s string) (slog.Level, error) { return loglevels.ParseLevel(s) }

// ParseLogLevels is re-exported from pkg/loglevels.
func ParseLogLevels(s string, def slog.Level) (LogLevels, error) {
	return loglevels.ParseLogLevels(s, def)
}
