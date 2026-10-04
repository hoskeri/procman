package writelog

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
)

// MaxFrameSize is the maximum encoded frame payload in bytes (for a single
// datagram). Records whose encoding exceeds this are shrink-to-fit by trimming
// the message text, then oversized attribute values, so a record is emitted
// rather than dropped whenever possible.
const MaxFrameSize = 16384

// StreamKind identifies which standard stream a framed record originated from. // This type is used to distinguish between stdout and stderr streams.
type StreamKind uint8

const (
	// StreamUnset indicates an unset stream (no frame)
	StreamUnset StreamKind = 0
	// StreamStdout indicates a stream originating from stdout
	StreamStdout StreamKind = 1
	// StreamStderr indicates a stream originating from stderr
	StreamStderr StreamKind = 2
)

func (s StreamKind) String() string {
	switch s {
	case StreamStdout:
		return "stdout"
	case StreamStderr:
		return "stderr"
	default:
		return "unset"
	}
}

// Frame is the wire representation of a single slog record sent over the
// SOCK_SEQPACKET log socket (a child's stdout or stderr in nested mode).
//
// The wire format is binary — see MarshalBinary / UnmarshalBinary for the
// on-the-wire layout.  Attrs are carried as a compact binary section (see
// attrs.go), preserving slog kinds and attribute order.
//
// Tag and Groups are distinct: Tag is the sender's display tag path (the
// component path, joined by "/"; the receiving formation prepends its own
// process tag), while Groups is the slog attribute namespace path used to
// qualify attrs.  Ordinary process output uses neither.
type Frame struct {
	Version int        // wire version (2)
	Level   int        // slog.Level as int
	Tag     string     // component tag path, "/"-joined (empty = no component)
	Stream  StreamKind // originating standard stream (stdout/stderr)
	Message string     // record message
	Groups  []string   // attr namespace path (slog WithGroup)
	Attrs   []byte     // encoded attrs section, nil when empty
}

// --- Binary wire format ---
//
// Header (fixed 8 bytes):
//   [0]     ver      uint8 (2)
//   [1]     flags    uint8 (bit0=has_tag, bit1=has_groups, bit2=has_attrs)
//   [2:6]   level    int32 little-endian
//   [6:8]   msglen   uint16 little-endian (message text length)
//   [8:]    message  msglen bytes
//
// Optional sections (present when the corresponding flag is set):
//   tag:     [len:uint8][data]
//   groups:  [count:uint8]{[len:uint8][data]}...
//   attrs:   remaining bytes of the message = encoded attrs section (attrs.go)

const (
	frameVersion = 2

	frameFlagHasTag    byte = 0x01
	frameFlagHasGroups byte = 0x02
	frameFlagHasAttrs  byte = 0x04

	// The originating stream is packed into flags bits 3-4 (0=unset,
	// 1=stdout, 2=stderr), so no extra header bytes or optional section
	// are needed and frames without the bits decode as StreamUnset.
	frameStreamShift      = 3
	frameStreamMask  byte = 0x18

	frameHeaderSize = 8 // ver(1) + flags(1) + level(4) + msglen(2)
)

func (f *Frame) marshalSize() int {
	n := frameHeaderSize + len(f.Message)
	if f.Tag != "" {
		n += 1 + len(f.Tag)
	}
	for _, g := range f.Groups {
		n += 1 + len(g)
	}
	if len(f.Attrs) > 0 {
		n += len(f.Attrs)
	}
	return n
}

// MarshalBinary encodes f into its binary wire format.
func (f *Frame) MarshalBinary() ([]byte, error) {
	if f.Version != frameVersion {
		return nil, fmt.Errorf("writelog: unsupported frame version %d", f.Version)
	}

	msg := []byte(f.Message)
	if len(msg) > maxMsgLen {
		msg = msg[:maxMsgLen]
	}

	buf := make([]byte, 0, f.marshalSize())

	// Header
	buf = append(buf, byte(f.Version))

	var flags byte
	if f.Tag != "" {
		flags |= frameFlagHasTag
	}
	if len(f.Groups) > 0 {
		flags |= frameFlagHasGroups
	}
	if len(f.Attrs) > 0 {
		flags |= frameFlagHasAttrs
	}
	flags |= byte(f.Stream&0x3) << frameStreamShift
	buf = append(buf, flags)

	var lvl [4]byte
	binary.LittleEndian.PutUint32(lvl[:], uint32(f.Level))
	buf = append(buf, lvl[:]...)

	var ml [2]byte
	binary.LittleEndian.PutUint16(ml[:], uint16(len(msg)))
	buf = append(buf, ml[:]...)

	buf = append(buf, msg...)

	// Tag
	if f.Tag != "" {
		buf = append(buf, byte(len(f.Tag)))
		buf = append(buf, f.Tag...)
	}

	// Groups
	if len(f.Groups) > 0 {
		if len(f.Groups) > 255 {
			// Truncate groups list — extremely unlikely in practice.
			f.Groups = f.Groups[:255]
		}
		buf = append(buf, byte(len(f.Groups)))
		for _, g := range f.Groups {
			if len(g) > 255 {
				g = g[:255]
			}
			buf = append(buf, byte(len(g)))
			buf = append(buf, g...)
		}
	}

	// Attrs (encoded attr section, remainder of message)
	if len(f.Attrs) > 0 {
		buf = append(buf, f.Attrs...)
	}

	return buf, nil
}

// UnmarshalBinary decodes f from its binary wire format.
func (f *Frame) UnmarshalBinary(data []byte) error {
	if len(data) < frameHeaderSize {
		return io.ErrUnexpectedEOF
	}

	ver := data[0]
	if ver != frameVersion {
		return fmt.Errorf("writelog: unsupported frame version %d", ver)
	}
	f.Version = int(ver)

	flags := data[1]
	f.Stream = StreamKind((flags & frameStreamMask) >> frameStreamShift)
	// Level is a signed slog.Level (Debug is negative), encoded as int32;
	// decode through int32 so it is not widened to a large positive value.
	f.Level = int(int32(binary.LittleEndian.Uint32(data[2:6])))

	msglen := int(binary.LittleEndian.Uint16(data[6:8]))
	end := 8 + msglen
	if end > len(data) {
		return io.ErrUnexpectedEOF
	}
	f.Message = string(data[8:end])

	off := end

	// Tag
	if flags&frameFlagHasTag != 0 {
		if off >= len(data) {
			return io.ErrUnexpectedEOF
		}
		tl := int(data[off])
		off++
		if off+tl > len(data) {
			return io.ErrUnexpectedEOF
		}
		f.Tag = string(data[off : off+tl])
		off += tl
	}

	// Groups
	if flags&frameFlagHasGroups != 0 {
		if off >= len(data) {
			return io.ErrUnexpectedEOF
		}
		gc := int(data[off])
		off++
		f.Groups = make([]string, 0, gc)
		for i := 0; i < gc; i++ {
			if off >= len(data) {
				return io.ErrUnexpectedEOF
			}
			gl := int(data[off])
			off++
			if off+gl > len(data) {
				return io.ErrUnexpectedEOF
			}
			f.Groups = append(f.Groups, string(data[off:off+gl]))
			off += gl
		}
	}

	// Attrs (remaining bytes = encoded attr section)
	if flags&frameFlagHasAttrs != 0 && off < len(data) {
		f.Attrs = make([]byte, len(data)-off)
		copy(f.Attrs, data[off:])
	}

	return nil
}

// maxMsgLen is the maximum message text length that fits in the uint16 msglen
// field. The total frame may still exceed MaxFrameSize due to tag/groups/attrs.
const maxMsgLen = 65535

// IsFramePrefix returns true when data starts with a valid binary frame header
// (version byte == frameVersion).  This is the fast gate for the dual-mode
// relay: a single byte comparison before attempting a full frame decode.
func IsFramePrefix(data []byte) bool {
	return len(data) >= frameHeaderSize && data[0] == frameVersion
}

// WriteFrame encodes f as a binary frame and writes it to sendFd (a
// non-blocking SOCK_SEQPACKET send socket).  Returns true when the frame was
// written, false when dropped (EAGAIN).
func WriteFrame(sendFd int, f *Frame) (bool, error) {
	b, err := f.MarshalBinary()
	if err != nil {
		return false, err
	}
	if len(b) > MaxFrameSize {
		f.fit(MaxFrameSize)
		if b, err = f.MarshalBinary(); err != nil {
			return false, err
		}
	}
	if len(b) > MaxFrameSize {
		return false, fmt.Errorf("writelog: frame %d bytes exceeds %d even after truncation", len(b), MaxFrameSize)
	}

	n, err := syscall.Write(sendFd, b)
	if err == nil && n == len(b) {
		return true, nil
	}
	if isEAGAIN(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return false, io.ErrShortWrite
}

// fit shrinks f in place so MarshalBinary encodes at most limit bytes.  The
// message text is trimmed first (the historical behavior); if the fixed
// overhead (mostly the attrs section) still leaves no room, attrs are shrunk
// by truncating their longest string values and dropping trailing attrs.
//
// Every slice is clamped to the string's actual length, so a short message
// paired with a large attrs payload cannot panic (the previous truncation
// sliced [:10] unconditionally, which crashed on messages shorter than that).
func (f *Frame) fit(limit int) {
	orig := f.Message
	if len(orig) > maxMsgLen {
		orig = orig[:maxMsgLen]
	}

	// Fixed overhead excluding the message and attrs (header + tag + groups).
	fixedNoAttrs := f.marshalSize() - len(f.Message) - len(f.Attrs)
	if fixedNoAttrs+len(f.Attrs)+len(orig) <= limit {
		f.Message = orig
		return
	}

	// Reserve up to half the frame for the message, and bound attrs to the
	// rest so the message is not silently emptied by a giant attrs payload.
	msgBudget := len(orig)
	if maxMsg := limit/2 - fixedNoAttrs; msgBudget > maxMsg {
		msgBudget = maxMsg
	}
	if msgBudget < 0 {
		msgBudget = 0
	}
	attrBudget := limit - fixedNoAttrs - msgBudget
	if attrBudget < 0 {
		attrBudget = 0
	}
	f.Attrs = fitAttrs(f.Attrs, attrBudget)

	avail := limit - fixedNoAttrs - len(f.Attrs)
	if avail < 0 {
		avail = 0
	}
	if avail > maxMsgLen {
		avail = maxMsgLen
	}
	if len(orig) > avail {
		f.Message = orig[:avail]
	} else {
		f.Message = orig
	}
}

// ReadFrame reads one message from recv (a *os.File wrapping a SOCK_SEQPACKET
// recv socket) and decodes it. When the write side is fully closed, ok is
// false and err is nil or io.EOF.
func ReadFrame(recv *os.File) (f Frame, ok bool, err error) {
	buf := make([]byte, MaxFrameSize+1024)
	n, err := recv.Read(buf)
	if err != nil {
		return Frame{}, false, err
	}
	if n == 0 {
		return Frame{}, false, nil
	}
	if err := f.UnmarshalBinary(buf[:n]); err != nil {
		return Frame{}, false, fmt.Errorf("bad frame: %w", err)
	}
	return f, true, nil
}

// --- FramerHandler: slog.Handler that encodes records as binary frames ---

// LevelResolver maps a relayed record's component tag path (relative to its
// process tag) to an effective minimum level.  ok is false when no override
// applies to that identity, in which case the handler's ambient level is used.
// A nil resolver means "no per-identity policy".  It is how a Formation's
// LogLevels policy reaches the parent-side sink: the sink passes the full
// group path minus the leading process tag.
type LevelResolver func(groups []string) (slog.Level, bool)

// FramerHandler is a slog.Handler that encodes each record as a Frame and
// writes it to a SOCK_SEQPACKET send socket. It implements the levelSetter
// interface for per-process log level overrides, TagHandler for the display
// tag path, and accepts a LevelResolver for per-component overrides.
type FramerHandler struct {
	sendFd    int           // raw socket fd (non-blocking SEQPACKET)
	stream    StreamKind    // stream stamp applied to every frame
	tagPath   []string      // display tag path (via WithTag / TagHandler)
	groupPath []string      // attr namespace path (via WithGroup)
	attrs     []slog.Attr   // accumulated from WithAttrs
	level     slog.Leveler  // base threshold
	override  slog.Leveler  // per-group threshold override (via WithOverride)
	resolver  LevelResolver // per-identity threshold policy (via WithResolver)
	drops     int64         // total dropped frames (O_NONBLOCK full)
	mu        sync.Mutex
}

// NewFramer returns a FramerHandler that writes frames to the given send
// socket fd (a non-blocking SOCK_SEQPACKET send socket).  stream stamps
// every emitted frame with its originating standard stream.  The caller is
// responsible for closing the fd after the formation exits.  level is the
// minimum level to emit (use slog.LevelInfo for default).
func NewFramer(sendFd int, stream StreamKind, level slog.Leveler) *FramerHandler {
	if level == nil {
		level = slog.LevelInfo
	}
	return &FramerHandler{
		sendFd: sendFd,
		stream: stream,
		level:  level,
	}
}

func (h *FramerHandler) clone() *FramerHandler {
	h2 := &FramerHandler{
		sendFd:    h.sendFd,
		stream:    h.stream,
		tagPath:   append([]string(nil), h.tagPath...),
		groupPath: append([]string(nil), h.groupPath...),
		attrs:     append([]slog.Attr(nil), h.attrs...),
		level:     h.level,
		override:  h.override,
		resolver:  h.resolver,
	}
	return h2
}

// RelativeTagPath drops the leading process tag from a sink's tag path.  The
// formation's sink receives the process tag via TaggedSink/WithTag before any
// component tags, so the remainder is the component identity the resolver
// matches against (see process.LogLevels.ForIdentity).
func RelativeTagPath(tagPath []string) []string {
	if len(tagPath) > 0 {
		return tagPath[1:]
	}
	return nil
}

// Enabled determines if the handler is enabled for the given slog.Level.
func (h *FramerHandler) Enabled(_ context.Context, l slog.Level) bool {
	if len(h.tagPath) == 0 && len(h.groupPath) == 0 {
		// Root handler with no tag or group: always enabled (slog may
		// probe it, but real component loggers carry a tag via WithTag).
		return true
	}
	threshold := h.level.Level()
	if h.override != nil {
		threshold = h.override.Level()
	}
	if h.resolver != nil {
		if lvl, ok := h.resolver(RelativeTagPath(h.tagPath)); ok {
			threshold = lvl
		}
	}
	return l >= threshold
}

// Handle processes the record: encodes it as a Frame and writes it to the send socket.
func (h *FramerHandler) Handle(_ context.Context, rec slog.Record) error {
	f := Frame{
		Version: frameVersion,
		Level:   int(rec.Level),
		Stream:  h.stream,
		Tag:     strings.Join(h.tagPath, "/"),
		Message: rec.Message,
		Groups:  h.groupPath,
	}

	// Collect record attrs + handler attrs.  The display tag is carried
	// separately in Tag (via WithTag), so a "tag" attr is now just an attr.
	// Empty-key attrs are kept here and normalized by appendAttrSection:
	// slog.Group("", ...) is an inline group, not a stray attr.
	var allAttrs []slog.Attr
	rec.Attrs(func(a slog.Attr) bool {
		allAttrs = append(allAttrs, a)
		return true
	})
	allAttrs = append(allAttrs, h.attrs...)

	if len(allAttrs) > 0 {
		f.Attrs = appendAttrSection(nil, allAttrs)
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
	}
	return nil
}

func isEAGAIN(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(syscall.Errno); ok && e == syscall.EAGAIN {
		return true
	}
	if pe, ok := err.(*os.PathError); ok {
		return isEAGAIN(pe.Err)
	}
	return false
}

// WithAttrs implements the slog.Handler interface.
func (h *FramerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h2 := h.clone()
	h2.attrs = append(h2.attrs, attrs...)
	return h2
}

// WithGroup implements the slog.Handler interface.  It records an attribute
// namespace (carried as Frame.Groups), never the display tag path.
func (h *FramerHandler) WithGroup(name string) slog.Handler {
	h2 := h.clone()
	h2.groupPath = append(h2.groupPath, name)
	return h2
}

// WithTag implements the TagHandler interface.  It extends the display tag
// path, which Handle encodes as Frame.Tag (not as a group).
func (h *FramerHandler) WithTag(name string) slog.Handler {
	h2 := h.clone()
	h2.tagPath = append(h2.tagPath, name)
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

// WithResolver implements the resolverSetter interface: returns a handler
// whose per-identity threshold is resolved from the component tag path
// (everything after the leading process tag).  A matched override wins over
// WithOverride; an unmatched path leaves the ambient threshold in place.
func (h *FramerHandler) WithResolver(resolver LevelResolver) slog.Handler {
	h2 := h.clone()
	h2.resolver = resolver
	return h2
}

// SetupSendSocket configures a SOCK_SEQPACKET send socket fd for non-blocking
// writes. Takes the raw fd number (as returned by Socketpair[1]). Returns
// the raw fd number — write operations must use syscall.Write to bypass Go's
// runtime poll.
func SetupSendSocket(fd int) int {
	_ = syscall.SetNonblock(fd, true)
	return fd
}

// SetupRecvSocket returns an *os.File wrapping the recv-side fd. The recv
// side uses blocking reads (Go's runtime poll handles this correctly for
// blocking reads).
func SetupRecvSocket(fd int) *os.File {
	return os.NewFile(uintptr(fd), "log-recv")
}
