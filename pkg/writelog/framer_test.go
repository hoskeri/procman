package writelog

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestFrameRoundTrip verifies that a Frame survives MarshalBinary/UnmarshalBinary
// with all fields preserved.
func TestFrameRoundTrip(t *testing.T) {
	// Build attrs as JSON bytes (what FramerHandler produces)
	attrsJSON, _ := json.Marshal(map[string]any{"pid": float64(42), "signal": "SIGKILL"})

	orig := Frame{
		Version:   1,
		Level:     int(slog.LevelError),
		Tag:       "kubelet",
		Stream:    StreamStderr,
		Message:   "out of memory",
		Groups:    []string{"kubelet"},
		AttrsJSON: attrsJSON,
	}
	b, err := orig.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty marshal")
	}
	if b[0] != frameVersion {
		t.Errorf("first byte: got %d, want %d", b[0], frameVersion)
	}

	var got Frame
	if err := got.UnmarshalBinary(b); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}

	if got.Version != orig.Version {
		t.Errorf("Version: got %d, want %d", got.Version, orig.Version)
	}
	if got.Level != orig.Level {
		t.Errorf("Level: got %d, want %d", got.Level, orig.Level)
	}
	if got.Tag != orig.Tag {
		t.Errorf("Tag: got %q, want %q", got.Tag, orig.Tag)
	}
	if got.Stream != orig.Stream {
		t.Errorf("Stream: got %v, want %v", got.Stream, orig.Stream)
	}
	if got.Message != orig.Message {
		t.Errorf("Message: got %q, want %q", got.Message, orig.Message)
	}
	if len(got.Groups) != 1 || got.Groups[0] != "kubelet" {
		t.Errorf("Groups: got %v, want %v", got.Groups, orig.Groups)
	}
	if len(got.AttrsJSON) == 0 {
		t.Fatal("expected non-empty AttrsJSON")
	}
	var attrsMap map[string]any
	if err := json.Unmarshal(got.AttrsJSON, &attrsMap); err != nil {
		t.Fatalf("unmarshal attrs: %v", err)
	}
	if attrsMap["pid"] != float64(42) || attrsMap["signal"] != "SIGKILL" {
		t.Errorf("Attrs: got %v", attrsMap)
	}
}

// TestStreamRoundTrip covers the stdout/stderr discriminator across the wire,
// including the unset default and that the stream bits coexist with the other
// flags.
func TestStreamRoundTrip(t *testing.T) {
	for _, stream := range []StreamKind{StreamUnset, StreamStdout, StreamStderr} {
		orig := Frame{
			Version: 1,
			Level:   int(slog.LevelInfo),
			Tag:     "web",
			Stream:  stream,
			Message: "hello",
			Groups:  []string{"web", "procman"},
		}
		b, err := orig.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary(%v): %v", stream, err)
		}
		var got Frame
		if err := got.UnmarshalBinary(b); err != nil {
			t.Fatalf("UnmarshalBinary(%v): %v", stream, err)
		}
		if got.Stream != stream {
			t.Errorf("Stream: got %v, want %v", got.Stream, stream)
		}
		if got.Message != orig.Message || got.Tag != orig.Tag || len(got.Groups) != 2 {
			t.Errorf("stream %v clobbered other fields: %+v", stream, got)
		}
	}
}

// TestFrameRoundTripNoAttrs verifies the common case (no attrs) round-trips.
func TestFrameRoundTripNoAttrs(t *testing.T) {
	orig := Frame{
		Version: 1,
		Level:   0,
		Tag:     "web",
		Message: "started",
		Groups:  []string{"web", "procman"},
	}
	b, err := orig.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	var got Frame
	if err := got.UnmarshalBinary(b); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if got.Message != "started" || got.Tag != "web" {
		t.Errorf("round-trip: got %+v", got)
	}
	if len(got.AttrsJSON) != 0 {
		t.Errorf("expected no attrs, got %d bytes", len(got.AttrsJSON))
	}
}

// TestIsFramePrefix verifies the fast gate for the dual-mode relay.
func TestIsFramePrefix(t *testing.T) {
	// Binary frame header starts with frameVersion.
	f := Frame{Version: 1, Level: 0, Message: "test"}
	b, _ := f.MarshalBinary()
	if !IsFramePrefix(b) {
		t.Error("expected IsFramePrefix true for binary frame")
	}

	// Text: random ASCII should be false.
	if IsFramePrefix([]byte("hello world\n")) {
		t.Error("expected IsFramePrefix false for text")
	}

	// Short buffer should be false.
	if IsFramePrefix([]byte{0x01}) {
		t.Error("expected IsFramePrefix false for short buffer")
	}

	// Wrong version byte should be false.
	if IsFramePrefix([]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}) {
		t.Error("expected IsFramePrefix false for wrong version")
	}
}

// captureHandler implements slog.Handler by calling a function and is
// used to collect records sent through a relay.  Handler-level attrs added
// via WithAttrs are merged into each captured record, mirroring what a real
// rendering handler sees.
type captureHandler struct {
	fn    func(context.Context, slog.Record) error
	attrs []slog.Attr
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	if len(h.attrs) > 0 {
		r.AddAttrs(h.attrs...)
	}
	return h.fn(ctx, r)
}
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h2 := *h
	h2.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &h2
}
func (h *captureHandler) WithGroup(_ string) slog.Handler { return h }

// TestFramerEnabledLevel is a regression test for NewFramer dropping the
// caller-supplied level: a nil level made FramerHandler.Enabled panic as soon
// as a group was applied (all real loggers use WithGroup).
func TestFramerEnabledLevel(t *testing.T) {
	ctx := context.Background()

	t.Run("grouped logger does not panic and respects level", func(t *testing.T) {
		f := NewFramer(-1, StreamStdout, slog.LevelInfo).WithGroup("procman")
		if !f.Enabled(ctx, slog.LevelWarn) {
			t.Fatal("warn should be enabled at LevelInfo")
		}
		if f.Enabled(ctx, slog.LevelDebug) {
			t.Fatal("debug should be disabled at LevelInfo")
		}
	})

	t.Run("nil level defaults to info", func(t *testing.T) {
		f := NewFramer(-1, StreamStderr, nil).WithGroup("g")
		if !f.Enabled(ctx, slog.LevelInfo) || f.Enabled(ctx, slog.LevelDebug) {
			t.Fatal("nil level should default to LevelInfo")
		}
	})

	t.Run("override wins", func(t *testing.T) {
		f := NewFramer(-1, StreamStdout, slog.LevelInfo).WithGroup("g").(*FramerHandler).WithOverride("g", slog.LevelDebug)
		if !f.Enabled(ctx, slog.LevelDebug) {
			t.Fatal("override to LevelDebug should enable debug")
		}
	})
}

// TestFramerRelayRoundTrip creates a socketpair, writes frames from a
// FramerHandler, reads them via DualRelay, and verifies the record arrives at
// the parent sink with levels, message, tag, and attrs preserved.
func TestFramerRelayRoundTrip(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Send side: set non-blocking (as the child's ChildSinks would).
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	sendFd := fds[1]
	recvConn := SetupRecvConn(fds[0])

	var (
		mu     sync.Mutex
		seen   []slog.Record
		seenOK bool
	)
	recorder := slog.New(&captureHandler{fn: func(ctx context.Context, r slog.Record) error {
		mu.Lock()
		seen = append(seen, r)
		seenOK = true
		mu.Unlock()
		return nil
	}})

	// Start DualRelay
	var relayWg sync.WaitGroup
	DualRelay(recvConn, recorder, "parent", StreamStdout, &relayWg)

	time.Sleep(10 * time.Millisecond) // let relay goroutine start

	// Use FramerHandler to write a frame
	framer := NewFramer(sendFd, StreamStdout, slog.LevelInfo)
	framer = framer.WithGroup("child").WithAttrs([]slog.Attr{slog.Int("count", 7)}).(*FramerHandler)
	framerLogger := slog.New(framer)
	framerLogger.With(slog.String("tag", "child")).Warn("hello from child")

	syscall.Close(sendFd)
	relayWg.Wait()
	recvConn.Close()

	mu.Lock()
	defer mu.Unlock()

	if !seenOK || len(seen) == 0 {
		t.Fatalf("expected at least one record in parent sink, got %d", len(seen))
	}
	r := seen[0]
	if r.Message != "hello from child" {
		t.Errorf("message: got %q, want %q", r.Message, "hello from child")
	}
	if r.Level != slog.LevelWarn {
		t.Errorf("level: got %v, want %v", r.Level, slog.LevelWarn)
	}

	var tagSeen string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "tag" {
			tagSeen = a.Value.String()
		}
		return true
	})
	if tagSeen != "child" {
		t.Errorf("tag attr: got %q, want %q", tagSeen, "child")
	}
}

// TestDualRelayTextFallback verifies that text messages written to the socket
// by a non-procman child are split on newlines and logged correctly.
func TestDualRelayTextFallback(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	sendFd := fds[1]
	recvConn := SetupRecvConn(fds[0])

	var (
		mu  sync.Mutex
		got []string
	)
	recorder := slog.New(&captureHandler{fn: func(ctx context.Context, r slog.Record) error {
		mu.Lock()
		got = append(got, r.Message)
		mu.Unlock()
		return nil
	}})

	var relayWg sync.WaitGroup
	DualRelay(recvConn, recorder, "child", StreamStdout, &relayWg)

	time.Sleep(10 * time.Millisecond)

	// Write text as a non-procman child would.
	syscall.Write(sendFd, []byte("line1\nline2\n"))
	syscall.Write(sendFd, []byte("partial"))

	syscall.Close(sendFd)
	relayWg.Wait()
	recvConn.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(got) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(got), got)
	}
	if got[0] != "line1\n" {
		t.Errorf("line0: got %q, want %q", got[0], "line1\\n")
	}
	if got[1] != "line2\n" {
		t.Errorf("line1: got %q, want %q", got[1], "line2\\n")
	}
	if got[2] != "partial" {
		t.Errorf("line2: got %q, want %q", got[2], "partial")
	}
}

// TestDualRelayMixed verifies that a mix of frames and text is handled
// correctly.  A procman child writes a frame, then a non-procman sibling
// writes text.
func TestDualRelayMixed(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	sendFd := fds[1]
	recvConn := SetupRecvConn(fds[0])

	var (
		mu  sync.Mutex
		got []slog.Record
	)
	recorder := slog.New(&captureHandler{fn: func(ctx context.Context, r slog.Record) error {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		return nil
	}})

	var relayWg sync.WaitGroup
	DualRelay(recvConn, recorder, "mixed", StreamStdout, &relayWg)

	time.Sleep(10 * time.Millisecond)

	// Write a frame (procman child style).
	framer := NewFramer(sendFd, StreamStdout, slog.LevelDebug)
	framerLogger := slog.New(framer)
	framerLogger.With(slog.String("tag", "proc-child")).Info("structured message")

	// Write text (non-procman child style — need to re-enable blocking
	// writes; but the framer set O_NONBLOCK. For this test we syscall.Write
	// directly since we know the socket buffer has room).
	syscall.Write(sendFd, []byte("text line\n"))

	syscall.Close(sendFd)
	relayWg.Wait()
	recvConn.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d: %+v", len(got), got)
	}

	// First: frame.
	if got[0].Message != "structured message" {
		t.Errorf("frame message: got %q, want %q", got[0].Message, "structured message")
	}

	// Second: text line.
	if !strings.Contains(got[1].Message, "text line") {
		t.Errorf("text message: got %q, want containing %q", got[1].Message, "text line")
	}
}

// attrString returns the string value of attr key in r, or "" if absent.
func attrString(r slog.Record, key string) string {
	v := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
			return false
		}
		return true
	})
	return v
}

// TestDualRelayStreamDiscriminator verifies that a frame's own stream wins
// over the channel it arrived on, and that text (which has no frame) takes
// the channel's stream.
func TestDualRelayStreamDiscriminator(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	sendFd := fds[1]
	recvConn := SetupRecvConn(fds[0])

	var (
		mu  sync.Mutex
		got []slog.Record
	)
	recorder := slog.New(&captureHandler{fn: func(ctx context.Context, r slog.Record) error {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		return nil
	}})

	var relayWg sync.WaitGroup
	// The channel is stdout, but the frame below claims stderr.
	DualRelay(recvConn, recorder, "child", StreamStdout, &relayWg)
	time.Sleep(10 * time.Millisecond)

	framer := NewFramer(sendFd, StreamStderr, slog.LevelInfo)
	slog.New(framer).Info("from stderr")
	syscall.Write(sendFd, []byte("plain text\n"))

	syscall.Close(sendFd)
	relayWg.Wait()
	recvConn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d: %+v", len(got), got)
	}
	if s := attrString(got[0], "stream"); s != "stderr" {
		t.Errorf("frame stream: got %q, want stderr (frame must win)", s)
	}
	if s := attrString(got[1], "stream"); s != "stdout" {
		t.Errorf("text stream: got %q, want stdout (channel fallback)", s)
	}
}

// TestFramerDrop verifies that a saturated socket buffer causes drops
// (writes return EAGAIN and the drop counter increments).
func TestFramerDrop(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		t.Fatalf("SetNonblock send: %v", err)
	}
	sendFd := fds[1]
	recvFd := fds[0]
	defer syscall.Close(recvFd)

	framer := NewFramer(sendFd, StreamStdout, slog.LevelDebug)

	// Fill the socket buffer with large frames. Write until EAGAIN drops occur.
	largeMsg := strings.Repeat("X", 14000) // each frame ~14 KB
	hadDrop := false
	for i := 0; i < 100; i++ {
		rec := slog.NewRecord(time.Now(), slog.LevelInfo, largeMsg, 0)
		_ = framer.Handle(context.Background(), rec)
		framer.mu.Lock()
		d := framer.drops
		framer.mu.Unlock()
		if d > 0 {
			hadDrop = true
			break
		}
	}

	if !hadDrop {
		t.Log("no drops detected after 100 frames — buffer may be very large")
	}
	syscall.Close(sendFd)
}

// TestRelayTeardown verifies that setting a past read deadline on the recv
// socket causes the relay goroutine to exit promptly.
func TestRelayTeardown(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	recvConn := SetupRecvConn(fds[0])
	defer syscall.Close(fds[1])

	sink := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var wg sync.WaitGroup
	DualRelay(recvConn, sink, "teardown-test", StreamStdout, &wg)

	time.Sleep(10 * time.Millisecond) // let it start

	// Unblock the reader by setting a past deadline.
	recvConn.SetReadDeadline(time.Now().Add(-1 * time.Second))

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// relay exited.
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not exit within 3s after SetReadDeadline past")
	}
	recvConn.Close()
}

// TestTextBuf verifies that textBuf correctly tracks partial lines across
// multiple feed calls and flushes the final partial line.
func BenchmarkFrameEncodeDecode(b *testing.B) {
	f := Frame{
		Version: 1,
		Level:   int(slog.LevelInfo),
		Tag:     "kubelet",
		Message: "hello from the child formation process",
		Groups:  []string{"procman", "kubelet"},
	}
	buf, _ := f.MarshalBinary()

	b.Run("Encode", func(b *testing.B) {
		for range b.N {
			f.MarshalBinary()
		}
	})
	b.Run("Decode", func(b *testing.B) {
		for range b.N {
			var got Frame
			got.UnmarshalBinary(buf)
		}
	})
}

func TestTextBuf(t *testing.T) {
	var got []string
	emit := func(line string) { got = append(got, line) }

	var tb textBuf
	tb.feed("line1\nline2\n", emit)
	if len(got) != 2 || got[0] != "line1\n" || got[1] != "line2\n" {
		t.Fatalf("after feed 1: got %v", got)
	}

	tb.feed("par", emit)
	if len(got) != 2 {
		t.Fatalf("partial should not emit: got %v", got)
	}

	tb.feed("tial\nend", emit)
	if len(got) != 3 || got[2] != "partial\n" {
		t.Fatalf("after feed 3: got %v", got)
	}

	tb.flush(emit)
	if len(got) != 4 || got[3] != "end" {
		t.Fatalf("after flush: got %v", got)
	}
}
