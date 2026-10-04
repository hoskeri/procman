package writelog

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestAttrsRoundTripKinds verifies that every slog kind, attribute order, and
// nested groups survive the binary attrs codec.
func TestAttrsRoundTripKinds(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	attrs := []slog.Attr{
		slog.String("s", "hi"),
		slog.Int("i", -7),
		slog.Int64("i64", 1<<40),
		slog.Uint64("u", 1<<63),
		slog.Float64("f", 3.5),
		slog.Bool("b", true),
		slog.Duration("d", 1500*time.Millisecond),
		slog.Time("t", ts),
		slog.Group("g", slog.String("x", "y"), slog.Int("n", 1)),
		slog.Any("a", []int{1, 2, 3}),
	}

	got, ok := unmarshalAttrs(appendAttrSection(nil, attrs))
	if !ok {
		t.Fatal("decode failed")
	}
	if len(got) != len(attrs) {
		t.Fatalf("attr count: got %d, want %d", len(got), len(attrs))
	}

	wantKeys := []string{"s", "i", "i64", "u", "f", "b", "d", "t", "g", "a"}
	for i, k := range wantKeys {
		if got[i].Key != k {
			t.Errorf("attr[%d].Key: got %q, want %q (order not preserved?)", i, got[i].Key, k)
		}
	}

	if got[0].Value.Kind() != slog.KindString || got[0].Value.String() != "hi" {
		t.Errorf("string: %+v", got[0])
	}
	if got[1].Value.Kind() != slog.KindInt64 || got[1].Value.Int64() != -7 {
		t.Errorf("int: %+v", got[1])
	}
	if got[2].Value.Int64() != 1<<40 {
		t.Errorf("int64: %+v", got[2])
	}
	if got[3].Value.Uint64() != 1<<63 {
		t.Errorf("uint64: %+v", got[3])
	}
	if got[4].Value.Float64() != 3.5 {
		t.Errorf("float64: %+v", got[4])
	}
	if !got[5].Value.Bool() {
		t.Errorf("bool: %+v", got[5])
	}
	if got[6].Value.Duration() != 1500*time.Millisecond {
		t.Errorf("duration: %+v", got[6])
	}
	if !got[7].Value.Time().Equal(ts) {
		t.Errorf("time: %+v", got[7])
	}
	if got[8].Value.Kind() != slog.KindGroup {
		t.Fatalf("group kind: %+v", got[8])
	}
	g := got[8].Value.Group()
	if len(g) != 2 || g[0].Key != "x" || g[0].Value.String() != "y" || g[1].Value.Int64() != 1 {
		t.Errorf("group: %+v", g)
	}
	if got[9].Value.String() != "[1 2 3]" {
		t.Errorf("any: %+v", got[9])
	}
}

// TestAttrsInlineGroups verifies slog inline groups (empty-key group attrs,
// as produced by slog.Group("", ...) / Logger.Log with a "" key) are
// flattened into their members instead of being dropped.
func TestAttrsInlineGroups(t *testing.T) {
	attrs := []slog.Attr{
		slog.String("op", "CREATE"),
		{Key: "", Value: slog.Group("", "uid", "abc-123", "patched", false).Value},
		{Key: "", Value: slog.StringValue("elided")},
	}

	got, ok := unmarshalAttrs(appendAttrSection(nil, attrs))
	if !ok {
		t.Fatal("decode failed")
	}
	want := []string{"op", "uid", "patched"}
	if len(got) != len(want) {
		t.Fatalf("attr count: got %d (%+v), want %d", len(got), got, len(want))
	}
	for i, k := range want {
		if got[i].Key != k {
			t.Errorf("attr[%d].Key: got %q, want %q", i, got[i].Key, k)
		}
	}
	if got[1].Value.String() != "abc-123" || got[2].Value.Bool() {
		t.Errorf("inline values: %+v", got[1:])
	}

	// Inline groups nested inside a named group are flattened too.
	nested := []slog.Attr{slog.Group("req", slog.Group("", "id", 42))}
	got, ok = unmarshalAttrs(appendAttrSection(nil, nested))
	if !ok || len(got) != 1 || got[0].Key != "req" {
		t.Fatalf("nested inline group: ok=%v got=%+v", ok, got)
	}
	g := got[0].Value.Group()
	if len(g) != 1 || g[0].Key != "id" || g[0].Value.Int64() != 42 {
		t.Errorf("nested inline group: %+v", g)
	}
}

// TestFitAttrsTruncatesLongString verifies the overflow path keeps a decodable
// section and truncates the long string value rather than dropping it.
func TestFitAttrsTruncatesLongString(t *testing.T) {
	const limit = 4096
	attrs := appendAttrSection(nil, []slog.Attr{
		slog.String("err", strings.Repeat("x", 4*MaxFrameSize)),
		slog.Int("status", 500),
	})
	if len(attrs) <= limit {
		t.Fatalf("precondition: attrs should exceed limit, got %d", len(attrs))
	}

	got := fitAttrs(attrs, limit)
	if len(got) > limit {
		t.Fatalf("fit kept %d bytes, want <= %d", len(got), limit)
	}
	dec, ok := unmarshalAttrs(got)
	if !ok {
		t.Fatal("fit produced an undecodable section")
	}
	found := false
	for _, a := range dec {
		if a.Key == "err" {
			found = true
			if len(a.Value.String()) >= 4*MaxFrameSize {
				t.Errorf("err not truncated: %d bytes", len(a.Value.String()))
			}
		}
	}
	if !found {
		t.Error("err attr was dropped instead of truncated")
	}
}

// TestFitAttrsDropsTrailing verifies that when no string can absorb the
// overflow, trailing attrs are dropped and the section stays decodable.
func TestFitAttrsDropsTrailing(t *testing.T) {
	var attrs []slog.Attr
	for i := 0; i < 50; i++ {
		attrs = append(attrs, slog.Int(strings.Repeat("k", 12), i))
	}
	encoded := appendAttrSection(nil, attrs)
	limit := len(encoded) / 4
	got := fitAttrs(encoded, limit)
	if len(got) > limit {
		t.Fatalf("fit kept %d bytes, want <= %d", len(got), limit)
	}
	dec, ok := unmarshalAttrs(got)
	if !ok {
		t.Fatal("fit produced an undecodable section")
	}
	if len(dec) == 0 || len(dec) >= len(attrs) {
		t.Errorf("expected some but not all attrs, got %d of %d", len(dec), len(attrs))
	}
}
