package writelog

import (
	"encoding/binary"
	"log/slog"
	"math"
	"time"
)

// Attrs section wire format (frame version 2), appended as the last section:
//
//	[count:uvarint]
//	per attr:
//	  [keylen:uvarint][key bytes]
//	  [kind:uint8]
//	  value payload, by kind:
//	    string/any: [len:uvarint][bytes]
//	    int64:      varint (zig-zag)
//	    uint64:     uvarint
//	    float64:    8 bytes little-endian IEEE-754
//	    bool:       1 byte (0/1)
//	    duration:   varint nanoseconds
//	    time:       varint UnixNano
//	    group:      [count:uvarint]{entry...}
//
// Unlike the previous JSON encoding, this preserves slog kinds, preserves
// attribute order, and needs no reflection or map allocation.
const (
	attrKindString byte = iota + 1
	attrKindInt64
	attrKindUint64
	attrKindFloat64
	attrKindBool
	attrKindDuration
	attrKindTime
	attrKindGroup
	attrKindAny
)

// appendAttrSection appends the encoded attrs section (count + entries).
// Empty-key group attrs are inlined first (see flattenInline) so the caller
// does not need to know slog's inline-group convention.
func appendAttrSection(dst []byte, attrs []slog.Attr) []byte {
	attrs = flattenInline(attrs)
	dst = binary.AppendUvarint(dst, uint64(len(attrs)))
	for _, a := range attrs {
		dst = appendAttrEntry(dst, a)
	}
	return dst
}

// flattenInline mirrors slog's inline-group semantics: an attr whose key is
// empty and whose value is a group (e.g. slog.Group("", ...)) is spliced into
// its parent, and an empty-key non-group attr is elided.  Named groups are
// flattened recursively.  This keeps `Log(ctx, lvl, msg, "", groupValue)`
// records (used by runkube's admission/authorization logging) intact instead
// of silently dropping them.
func flattenInline(attrs []slog.Attr) []slog.Attr {
	clean := true
	for _, a := range attrs {
		if a.Key == "" || a.Value.Resolve().Kind() == slog.KindGroup {
			clean = false
			break
		}
	}
	if clean {
		return attrs
	}

	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		v := a.Value.Resolve()
		if a.Key == "" {
			if v.Kind() == slog.KindGroup {
				out = append(out, flattenInline(v.Group())...)
			}
			continue
		}
		if v.Kind() == slog.KindGroup {
			a.Value = slog.GroupValue(flattenInline(v.Group())...)
		} else {
			a.Value = v
		}
		out = append(out, a)
	}
	return out
}

func appendAttrEntry(dst []byte, a slog.Attr) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(a.Key)))
	dst = append(dst, a.Key...)
	return appendAttrValue(dst, a.Value)
}

func appendAttrValue(dst []byte, v slog.Value) []byte {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		dst = append(dst, attrKindString)
		dst = binary.AppendUvarint(dst, uint64(len(s)))
		return append(dst, s...)
	case slog.KindInt64:
		dst = append(dst, attrKindInt64)
		return binary.AppendVarint(dst, v.Int64())
	case slog.KindUint64:
		dst = append(dst, attrKindUint64)
		return binary.AppendUvarint(dst, v.Uint64())
	case slog.KindFloat64:
		dst = append(dst, attrKindFloat64)
		return binary.LittleEndian.AppendUint64(dst, math.Float64bits(v.Float64()))
	case slog.KindBool:
		dst = append(dst, attrKindBool)
		if v.Bool() {
			return append(dst, 1)
		}
		return append(dst, 0)
	case slog.KindDuration:
		dst = append(dst, attrKindDuration)
		return binary.AppendVarint(dst, int64(v.Duration()))
	case slog.KindTime:
		dst = append(dst, attrKindTime)
		return binary.AppendVarint(dst, v.Time().UnixNano())
	case slog.KindGroup:
		g := v.Group()
		dst = append(dst, attrKindGroup)
		dst = binary.AppendUvarint(dst, uint64(len(g)))
		for _, a := range g {
			dst = appendAttrEntry(dst, a)
		}
		return dst
	default:
		// Any (and future kinds): carry the resolved text form.
		s := v.String()
		dst = append(dst, attrKindAny)
		dst = binary.AppendUvarint(dst, uint64(len(s)))
		return append(dst, s...)
	}
}

// unmarshalAttrs decodes an attrs section.  ok is false on malformed input;
// a nil/empty section decodes to (nil, true).
func unmarshalAttrs(data []byte) (attrs []slog.Attr, ok bool) {
	if len(data) == 0 {
		return nil, true
	}
	n, w := binary.Uvarint(data)
	if w <= 0 {
		return nil, false
	}
	b := data[w:]
	attrs = make([]slog.Attr, 0, min(n, 64))
	for i := uint64(0); i < n; i++ {
		a, rest, entryOK := decodeAttrEntry(b)
		if !entryOK {
			return nil, false
		}
		attrs = append(attrs, a)
		b = rest
	}
	if len(b) != 0 {
		return nil, false
	}
	return attrs, true
}

func decodeAttrEntry(b []byte) (slog.Attr, []byte, bool) {
	kl, w := binary.Uvarint(b)
	if w <= 0 || uint64(len(b)-w) < kl {
		return slog.Attr{}, nil, false
	}
	key := string(b[w : w+int(kl)])
	b = b[w+int(kl):]
	v, b, ok := decodeAttrValue(b)
	if !ok {
		return slog.Attr{}, nil, false
	}
	return slog.Attr{Key: key, Value: v}, b, true
}

func decodeAttrValue(b []byte) (slog.Value, []byte, bool) {
	if len(b) == 0 {
		return slog.Value{}, nil, false
	}
	kind := b[0]
	b = b[1:]
	switch kind {
	case attrKindString, attrKindAny:
		n, w := binary.Uvarint(b)
		if w <= 0 || uint64(len(b)-w) < n {
			return slog.Value{}, nil, false
		}
		s := string(b[w : w+int(n)])
		return slog.StringValue(s), b[w+int(n):], true
	case attrKindInt64:
		x, w := binary.Varint(b)
		if w <= 0 {
			return slog.Value{}, nil, false
		}
		return slog.Int64Value(x), b[w:], true
	case attrKindUint64:
		x, w := binary.Uvarint(b)
		if w <= 0 {
			return slog.Value{}, nil, false
		}
		return slog.Uint64Value(x), b[w:], true
	case attrKindFloat64:
		if len(b) < 8 {
			return slog.Value{}, nil, false
		}
		return slog.Float64Value(math.Float64frombits(binary.LittleEndian.Uint64(b))), b[8:], true
	case attrKindBool:
		if len(b) < 1 {
			return slog.Value{}, nil, false
		}
		return slog.BoolValue(b[0] == 1), b[1:], true
	case attrKindDuration:
		x, w := binary.Varint(b)
		if w <= 0 {
			return slog.Value{}, nil, false
		}
		return slog.DurationValue(time.Duration(x)), b[w:], true
	case attrKindTime:
		x, w := binary.Varint(b)
		if w <= 0 {
			return slog.Value{}, nil, false
		}
		return slog.TimeValue(time.Unix(0, x)), b[w:], true
	case attrKindGroup:
		n, w := binary.Uvarint(b)
		if w <= 0 {
			return slog.Value{}, nil, false
		}
		b = b[w:]
		g := make([]slog.Attr, 0, min(n, 32))
		for i := uint64(0); i < n; i++ {
			a, rest, ok := decodeAttrEntry(b)
			if !ok {
				return slog.Value{}, nil, false
			}
			g = append(g, a)
			b = rest
		}
		return slog.GroupValue(g...), b, true
	}
	return slog.Value{}, nil, false
}

// fitAttrs bounds an encoded attrs section to at most limit bytes.  It first
// truncates the longest string values (the common oversized attr is a single
// long error or stack string), then drops trailing attrs.  It returns nil when
// nothing fits.  Overflow is rare, so the decode/re-encode cost is acceptable.
func fitAttrs(data []byte, limit int) []byte {
	if len(data) == 0 {
		return nil
	}
	if limit <= 0 {
		return nil
	}
	if len(data) <= limit {
		return data
	}
	attrs, ok := unmarshalAttrs(data)
	if !ok {
		return nil
	}

	for i := 0; i < 32 && len(attrs) > 0; i++ {
		idx, n := -1, 0
		for j, a := range attrs {
			if a.Value.Kind() == slog.KindString && len(a.Value.String()) > n {
				idx, n = j, len(a.Value.String())
			}
		}
		if idx < 0 {
			break
		}
		encoded := appendAttrSection(nil, attrs)
		if len(encoded) <= limit {
			return encoded
		}
		s := attrs[idx].Value.String()
		keep := len(s) - (len(encoded) - limit) - 16
		if keep < 0 {
			keep = len(s) / 2
		}
		if keep < 0 {
			keep = 0
		}
		attrs[idx].Value = slog.StringValue(s[:keep])
	}

	for len(attrs) > 0 {
		encoded := appendAttrSection(nil, attrs)
		if len(encoded) <= limit {
			return encoded
		}
		attrs = attrs[:len(attrs)-1]
	}
	return nil
}
