package process

import (
	"log/slog"
	"testing"
)

func TestParseLogLevels(t *testing.T) {
	tests := []struct {
		name string
		in   string
		def  slog.Level
		want LogLevels
	}{{
		name: "bare default",
		in:   "debug",
		def:  slog.LevelError,
		want: LogLevels{Default: slog.LevelDebug},
	}, {
		name: "pairs only keep flag default",
		in:   "api=debug,node-a=warn",
		def:  slog.LevelError,
		want: LogLevels{
			Default: slog.LevelError,
			Tags:    map[string]slog.Level{"api": slog.LevelDebug, "node-a": slog.LevelWarn},
		},
	}, {
		name: "default and overrides",
		in:   "info,etcd=error,controller=debug",
		def:  slog.LevelError,
		want: LogLevels{
			Default: slog.LevelInfo,
			Tags:    map[string]slog.Level{"etcd": slog.LevelError, "controller": slog.LevelDebug},
		},
	}, {
		name: "spaces tolerated",
		in:   " warn , api = debug ",
		def:  slog.LevelError,
		want: LogLevels{
			Default: slog.LevelWarn,
			Tags:    map[string]slog.Level{"api": slog.LevelDebug},
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLogLevels(tt.in, tt.def)
			if err != nil {
				t.Fatalf("ParseLogLevels(%q): %v", tt.in, err)
			}
			if got.Default != tt.want.Default {
				t.Errorf("Default = %v, want %v", got.Default, tt.want.Default)
			}
			if len(got.Tags) != len(tt.want.Tags) {
				t.Fatalf("Tags = %v, want %v", got.Tags, tt.want.Tags)
			}
			for tag, want := range tt.want.Tags {
				if got := got.Tags[tag]; got != want {
					t.Errorf("Tags[%q] = %v, want %v", tag, got, want)
				}
			}
		})
	}
}

func TestParseLogLevelsErrors(t *testing.T) {
	for _, in := range []string{"", "verbose", "api=verbose", ",,", "=debug"} {
		if _, err := ParseLogLevels(in, slog.LevelError); err == nil {
			t.Errorf("ParseLogLevels(%q) succeeded, want error", in)
		}
	}
}

func TestLogLevelsFor(t *testing.T) {
	l := LogLevels{
		Default: slog.LevelWarn,
		Tags:    map[string]slog.Level{"api": slog.LevelDebug},
	}
	if got := l.For("api"); got != slog.LevelDebug {
		t.Errorf("For(api) = %v, want debug", got)
	}
	if got := l.For("web"); got != slog.LevelWarn {
		t.Errorf("For(web) = %v, want warn (default)", got)
	}

	// Zero value: default is Info, no overrides.
	if got := (LogLevels{}).For("web"); got != slog.LevelInfo {
		t.Errorf("zero LogLevels.For(web) = %v, want info", got)
	}
}

// TestProcessLogLevelPrecedence verifies explicit Process.LogLevel beats the
// formation's tag override and default, and that an unset process defers to
// the formation.
func TestProcessLogLevelPrecedence(t *testing.T) {
	levels := LogLevels{
		Default: slog.LevelWarn,
		Tags:    map[string]slog.Level{"web": slog.LevelError},
	}

	explicit := &Process{Tag: "web", LogLevel: slog.LevelDebug}
	if got := explicit.logLevel(levels); got != slog.LevelDebug {
		t.Errorf("explicit Process.LogLevel = %v, want debug", got)
	}

	tagged := &Process{Tag: "web"}
	if got := tagged.logLevel(levels); got != slog.LevelError {
		t.Errorf("tag override = %v, want error", got)
	}

	def := &Process{Tag: "worker"}
	if got := def.logLevel(levels); got != slog.LevelWarn {
		t.Errorf("default = %v, want warn", got)
	}

	// Without a formation policy, unset processes report the zero level
	// (ChildFDs treats that as "no override").
	if got := (&Process{Tag: "web"}).logLevel(LogLevels{}); got != slog.LevelInfo {
		t.Errorf("no policy = %v, want info", got)
	}
}
