package process

import (
	"fmt"
	"log/slog"
	"strings"
)

// LogLevels configures the log level for a formation's process output: a
// default plus per-tag overrides.  Tags are process tags (Procfile names,
// component names, node names).
//
// This is the library home for the CLI-level "--log-level" policy: a bare
// level name sets Default, and tag=level entries override individual process
// tags.  Embedders that build a Formation programmatically can set LogLevels
// directly; CLI frontends can use ParseLogLevels.
type LogLevels struct {
	// Default applies to any tag without an override.  The zero value is
	// slog.LevelInfo.
	Default slog.Level
	// Tags maps a process tag to its override.
	Tags map[string]slog.Level
}

// For returns the effective level for tag: the override when present,
// otherwise Default.
func (l LogLevels) For(tag string) slog.Level {
	if lvl, ok := l.Tags[tag]; ok {
		return lvl
	}
	return l.Default
}

// ParseLevel parses a level name accepted by ParseLogLevels: error, warn,
// info, or debug.
func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "error":
		return slog.LevelError, nil
	case "warn":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	}
	return 0, fmt.Errorf("unknown level %q (want error, warn, info or debug)", s)
}

// ParseLogLevels parses a comma-separated log-level spec into a LogLevels.  A
// bare entry sets Default; a tag=level entry sets Tags[tag].  def seeds
// Default so a spec containing only overrides preserves the caller's default
// (e.g. a CLI flag default).
//
// Examples: "error", "info,api=debug", "warn,etcd=error,node-a=debug".
func ParseLogLevels(s string, def slog.Level) (LogLevels, error) {
	l := LogLevels{Default: def}
	if s == "" {
		return l, fmt.Errorf("empty")
	}

	parsed := false
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if tag, lvlStr, ok := strings.Cut(part, "="); ok {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				return l, fmt.Errorf("empty component tag in %q", part)
			}
			lvl, err := ParseLevel(strings.TrimSpace(lvlStr))
			if err != nil {
				return l, err
			}
			if l.Tags == nil {
				l.Tags = map[string]slog.Level{}
			}
			l.Tags[tag] = lvl
			parsed = true
			continue
		}

		lvl, err := ParseLevel(part)
		if err != nil {
			return l, err
		}
		l.Default = lvl
		parsed = true
	}

	if !parsed {
		return l, fmt.Errorf("no levels specified")
	}

	return l, nil
}
