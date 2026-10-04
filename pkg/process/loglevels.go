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
	// Tags maps a log identity to its override.  Identity keys may be a
	// process tag ("webhook"), a full component path ("webhook/validate"), or
	// a component name alone ("validate"); see ForIdentity.
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

// ForIdentity returns the effective level and whether an override applies for
// a relayed record's identity: the process tag plus zero or more component
// group names.  It matches, in order of specificity:
//
//	webhook/validate   full path
//	validate           component path (all groups joined)
//	webhook            process tag
//
// and falls back to Default.  ok is false when nothing matches and Default is
// the zero value, so the caller keeps its ambient level.  Override entries for
// tags that match none of these forms are ignored.
func (l LogLevels) ForIdentity(processTag string, groups []string) (slog.Level, bool) {
	var candidates []string
	if component := strings.Join(groups, "/"); component != "" {
		candidates = append(candidates, processTag+"/"+component, component)
	}
	candidates = append(candidates, processTag)

	for _, key := range candidates {
		if key == "" {
			continue
		}
		if lvl, ok := l.Tags[key]; ok {
			return lvl, true
		}
	}
	return l.Default, l.Default != 0
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
