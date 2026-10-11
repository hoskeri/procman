package loglevels

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/spf13/pflag"
)

var _ pflag.Value = (*Value)(nil)

// Value adapts LogLevels to pflag.Value so a CLI frontend can register a
// "--log-level" flag directly.  The flag string uses ParseLogLevels syntax
// ("info", "debug", or "info,api=debug").  Seed it with NewValue so a spec
// carrying only tag overrides keeps the caller's default:
//
//	levels := loglevels.NewValue(slog.LevelInfo)
//	fs.Var(levels, "log-level", "log level policy: level or tag=level,...")
//	...
//	fm.LogLevels = levels.Levels()
type Value struct {
	levels LogLevels
	seed   slog.Level
	set    bool
}

// NewValue returns a Value seeded with def as the default level.  Before the
// flag is set, Levels reports {Default: def}.
func NewValue(def slog.Level) *Value {
	return &Value{levels: LogLevels{Default: def}, seed: def}
}

// Set parses spec with ParseLogLevels, keeping the seed as the default when
// spec carries only tag overrides.  pflag calls it once per occurrence.
func (v *Value) Set(spec string) error {
	l, err := ParseLogLevels(spec, v.seed)
	if err != nil {
		return err
	}
	v.levels = l
	v.set = true
	return nil
}

// String renders the current policy in ParseLogLevels syntax (tags sorted for
// determinism).  pflag uses it for the flag's default in --help.
func (v *Value) String() string {
	var b strings.Builder
	b.WriteString(levelName(v.levels.Default))
	keys := make([]string, 0, len(v.levels.Tags))
	for k := range v.levels.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(",")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(levelName(v.levels.Tags[k]))
	}
	return b.String()
}

// Type implements pflag.Value.
func (v *Value) Type() string { return "loglevels" }

// Levels returns the parsed policy; callers assign it to Formation.LogLevels.
func (v *Value) Levels() LogLevels { return v.levels }

// Changed reports whether the flag was explicitly provided.  Callers use it
// to let --log-level win over a separate convenience flag such as --debug.
func (v *Value) Changed() bool { return v.set }

// SetDefault updates the default level used to seed future parses and to fill
// Levels while the flag has not been provided (e.g. after a later --debug
// parse resolves the level).
func (v *Value) SetDefault(def slog.Level) {
	v.seed = def
	if !v.set {
		v.levels.Default = def
	}
}

// levelName renders a level in the lowercase form ParseLevel accepts, so
// Value.String is round-trippable rather than slog's uppercase labels.
func levelName(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelInfo:
		return "info"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return l.String()
	}
}
