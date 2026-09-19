package writelog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"io"
	"sync"
	"time"
	"unicode"
)

// MaxFrameSize is the maximum encoded frame payload in bytes (for a single
// datagram). Records whose JSON encoding exceeds this are truncated by
// trimming the message text; if still too large they are silently dropped.
const MaxFrameSize = 16384

// Frame is the wire representation of a single slog record sent over the
// dedicated log socket.
type Frame struct {
	Version int                `json:"v"`
	Level   int                `json:"lvl"`              // slog.Level as int
	Tag     string             `json:"tag,omitempty"`    // innermost process tag
	Message string             `json:"msg,omitempty"`    // record message
	Groups  []string           `json:"groups,omitempty"` // handler group path (innermost is last)
	Attrs   map[string]any     `json:"attrs,omitempty"`  // string-keyed attributes
}

// Marshal returns the JSON encoding of f.
func (f *Frame) Marshal() ([]byte, error) {
	return json.Marshal(f)
}

// Unmarshal decodes a JSON frame into f.
func (f *Frame) Unmarshal(data []byte) error {
	return json.Unmarshal(data, f)
}

// WriteFrame encodes f as JSON and writes it to the send socket fd.
// The fd must be a non-blocking SEQPACKET socket (see SetupSendSocket).
// Returns true when the frame was written, false when dropped (EAGAIN).
func WriteFrame(sendFd int, f *Frame) (bool, error) {
	b, err := f.Marshal()
	if err != nil {
		return false, err
	}
	if len(b) > MaxFrameSize {
		// Truncate: trim the message text until the frame fits.
		for len(b) > MaxFrameSize && len(f.Message) > 0 {
			keep := len(f.Message) * 3 / 4
			if keep < 10 {
				break
			}
			f.Message = f.Message[:keep]
			b, _ = f.Marshal()
		}
		if len(b) > MaxFrameSize {
			return false, fmt.Errorf("frame %d bytes exceeds %d even after truncation", len(b), MaxFrameSize)
		}
	}

	// Use raw syscall.Write to bypass Go's runtime poll, which would
	// block on a non-blocking SEQPACKET socket when the buffer is full.
	n, err := syscall.Write(sendFd, b)
	if err == nil && n == len(b) {
		return true, nil
	}
	if err, ok := err.(syscall.Errno); ok && err == syscall.EAGAIN {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return false, io.ErrShortWrite
}

// ReadFrame reads one message from recv (a *os.File wrapping a SOCK_SEQPACKET
// recv socket) and decodes it. When the write side is fully closed, ok is
// false.
func ReadFrame(recv *os.File) (f Frame, ok bool, err error) {
	buf := make([]byte, MaxFrameSize+1024)
	n, err := recv.Read(buf)
	if err != nil {
		return Frame{}, false, err
	}
	if n == 0 {
		return Frame{}, false, io.EOF
	}
	if err := f.Unmarshal(buf[:n]); err != nil {
		return Frame{}, false, fmt.Errorf("bad frame: %w", err)
	}
	return f, true, nil
}

// --- FramerHandler: slog.Handler that encodes records as frames ---

// FramerHandler is a slog.Handler that encodes each record as a Frame and
// writes it to a SOCK_SEQPACKET send socket. It implements the levelSetter
// interface for per-process log level overrides.
type FramerHandler struct {
	sendFd    int              // raw socket fd (non-blocking SEQPACKET)
	groupPath []string         // accumulated from WithGroup
	attrs     []slog.Attr      // accumulated from WithAttrs
	level     slog.Leveler     // base threshold
	override  slog.Leveler     // per-group threshold override (via WithOverride)
	drops     int64            // total dropped frames (O_NONBLOCK full)
	mu        sync.Mutex
}

// NewFramer returns a FramerHandler that writes frames to the given send
// socket fd (a non-blocking SOCK_SEQPACKET send socket returned by
// SetupSendSocket). The caller is responsible for closing the fd after the
// formation exits. level is the minimum level to emit (use slog.LevelInfo
// for default).
func NewFramer(sendFd int, level slog.Leveler) *FramerHandler {
	if level == nil {
		level = slog.LevelInfo
	}
	return &FramerHandler{
		sendFd: sendFd,
		level:  level,
	}
}

func (h *FramerHandler) clone() *FramerHandler {
	h2 := &FramerHandler{
		sendFd:   h.sendFd,
		groupPath: append([]string(nil), h.groupPath...),
		attrs:    append([]slog.Attr(nil), h.attrs...),
		level:    h.level,
		override: h.override,
	}
	return h2
}

func (h *FramerHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if len(h.groupPath) == 0 {
		// Root un-grouped handler: always enabled (slog may probe it,
		// but it's never called with actual records since all our
		// loggers go through WithGroup).
		return true
	}
	threshold := h.level.Level()
	if h.override != nil {
		threshold = h.override.Level()
	}
	return l >= threshold
}

// attrToJSON converts a slog.Value to a Go value suitable for JSON.
func attrToJSON(v slog.Value) any {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	case slog.KindGroup:
		m := make(map[string]any, len(v.Group()))
		for _, a := range v.Group() {
			m[a.Key] = attrToJSON(a.Value)
		}
		return m
	default:
		// KindAny, KindLogValuer (already resolved): stringify.
		s := v.String()
		// If the string looks like a number or bool, quote it so JSON
		// round-trips preserve the type hint.  This is a heuristic;
		// embedders who rely on exact types should use a richer schema.
		if looksLikeJSONToken(s) {
			s = fmt.Sprintf("%q", s)
		}
		return s
	}
}

func looksLikeJSONToken(s string) bool {
	if s == "true" || s == "false" || s == "null" {
		return true
	}
	if len(s) > 0 && (s[0] == '"' || s[0] == '{' || s[0] == '[') {
		return true
	}
	// Number-like.
	hasDigit := false
	for _, r := range s {
		if unicode.IsDigit(r) {
			hasDigit = true
			break
		}
	}
	return hasDigit
}

func (h *FramerHandler) Handle(ctx context.Context, rec slog.Record) error {
	f := Frame{
		Version: 1,
		Level:   int(rec.Level),
		Message: rec.Message,
		Groups:  h.groupPath,
	}

	// Merge record attrs with handler attrs.
	var allAttrs map[string]any
	if len(h.attrs) > 0 {
		allAttrs = make(map[string]any, len(h.attrs)+4)
	}
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "" {
			return true // skip empty-key attrs
		}
		if allAttrs == nil {
			allAttrs = make(map[string]any)
		}
		allAttrs[a.Key] = attrToJSON(a.Value)
		return true
	})
	for _, a := range h.attrs {
		if a.Key == "" {
			continue
		}
		if allAttrs == nil {
			allAttrs = make(map[string]any)
		}
		allAttrs[a.Key] = attrToJSON(a.Value)
	}

	// Extract tag from attrs — writelog.Stream stamps "tag" on every line.
	if allAttrs != nil {
		if t, ok := allAttrs["tag"]; ok {
			if s, ok := t.(string); ok {
				f.Tag = s
			}
		}
		if len(allAttrs) > 0 {
			// Drop the tag from attrs — it's in the frame's tag field.
			delete(allAttrs, "tag")
			f.Attrs = allAttrs
		}
	}

	// Write to socket (O_NONBLOCK).
	written, err := WriteFrame(h.sendFd, &f)
	if err != nil && !isEAGAIN(err) {
		return err
	}
	if !written {
		h.mu.Lock()
		h.drops++
		h.mu.Unlock()
		// Log the drop counter periodically (best-effort: another frame
		// may also be dropped — counter is imprecise under overload).
	}
	return nil
}

func isEAGAIN(err error) bool {
	if err == nil {
		return false
	}
	// Direct syscall.Errno (e.g., from os.File.Write on a non-blocking fd).
	if e, ok := err.(syscall.Errno); ok && e == syscall.EAGAIN {
		return true
	}
	// Unwrap from *os.PathError.
	if pe, ok := err.(*os.PathError); ok {
		return isEAGAIN(pe.Err)
	}
	return false
}

func (h *FramerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h2 := h.clone()
	h2.attrs = append(h2.attrs, attrs...)
	return h2
}

func (h *FramerHandler) WithGroup(name string) slog.Handler {
	h2 := h.clone()
	h2.groupPath = append(h2.groupPath, name)
	return h2
}

// WithOverride implements the levelSetter interface: returns a handler for
// the named group with per-process level override. Unlike termhandler, we
// do NOT set the group path here — the caller (writelog.Stream) also calls
// WithGroup, so the group is set there. We only store the level override.
func (h *FramerHandler) WithOverride(name string, lvl slog.Leveler) slog.Handler {
	h2 := h.clone()
	h2.override = lvl
	return h2
}

// SetupSendSocket configures a SOCK_SEQPACKET send socket fd for
// non-blocking writes. Takes the raw fd number (as returned by
// Socketpair[1]). Returns the raw fd number, NOT an *os.File —
// write operations must use syscall.Write to bypass Go's runtime poll.
func SetupSendSocket(fd int) int {
	_ = syscall.SetNonblock(fd, true)
	return fd
}

// SetupRecvSocket returns an *os.File wrapping the recv-side fd.
// The recv side uses blocking reads (Go's runtime poll handles this
// correctly for blocking reads).
func SetupRecvSocket(fd int) *os.File {
	return os.NewFile(uintptr(fd), "log-recv")
}