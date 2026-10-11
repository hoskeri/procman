package loglevels

import (
	"log/slog"
	"testing"
)

func TestValueDefaultsAndParsing(t *testing.T) {
	v := NewValue(slog.LevelInfo)
	if v.Changed() {
		t.Error("fresh Value reports Changed")
	}
	if got := v.Levels(); got.Default != slog.LevelInfo || len(got.Tags) != 0 {
		t.Errorf("fresh Levels = %+v, want {Default: info}", got)
	}
	if got := v.String(); got != "info" {
		t.Errorf("fresh String = %q, want %q", got, "info")
	}

	if err := v.Set("warn,api=debug"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !v.Changed() {
		t.Error("after Set, Changed = false")
	}
	got := v.Levels()
	if got.Default != slog.LevelWarn || got.Tags["api"] != slog.LevelDebug {
		t.Errorf("Levels = %+v, want {Default: warn, api: debug}", got)
	}
	if s := v.String(); s != "warn,api=debug" {
		t.Errorf("String = %q, want %q", s, "warn,api=debug")
	}
}

// TestValueSeedPreservedForOverridesOnly pins that a spec carrying only
// tag=level overrides keeps the seed default.
func TestValueSeedPreservedForOverridesOnly(t *testing.T) {
	v := NewValue(slog.LevelWarn)
	if err := v.Set("api=debug"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := v.Levels()
	if got.Default != slog.LevelWarn || got.Tags["api"] != slog.LevelDebug {
		t.Errorf("Levels = %+v, want {Default: warn, api: debug}", got)
	}
}

func TestValueSetError(t *testing.T) {
	v := NewValue(slog.LevelInfo)
	if err := v.Set("verbose"); err == nil {
		t.Fatal("Set(verbose) succeeded, want error")
	}
	// A failed Set leaves the previous policy intact.
	if v.Changed() {
		t.Error("failed Set marked the Value changed")
	}
	if got := v.Levels().Default; got != slog.LevelInfo {
		t.Errorf("after failed Set, Default = %v, want info", got)
	}
}

func TestValueSetDefault(t *testing.T) {
	v := NewValue(slog.LevelInfo)
	v.SetDefault(slog.LevelDebug)
	if got := v.Levels().Default; got != slog.LevelDebug {
		t.Errorf("SetDefault changed Default to %v, want debug", got)
	}

	// Once the flag was set, SetDefault only updates the seed for a later
	// parse, not the current policy.
	if err := v.Set("error"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v.SetDefault(slog.LevelWarn)
	if got := v.Levels().Default; got != slog.LevelError {
		t.Errorf("SetDefault overwrote an explicit flag: Default = %v, want error", got)
	}
}
