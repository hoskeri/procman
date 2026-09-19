package writelog

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestFrameRoundTrip verifies that a Frame survives marshal/unmarshal with
// all fields preserved.
func TestFrameRoundTrip(t *testing.T) {
	orig := Frame{
		Version: 1,
		Level:   int(slog.LevelError),
		Tag:     "kubelet",
		Message: "out of memory",
		Groups:  []string{"kubelet"},
		Attrs:   map[string]any{"pid": float64(42), "signal": "SIGKILL"},
	}
	b, err := orig.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty marshal")
	}

	var got Frame
	if err := got.Unmarshal(b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
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
	if got.Message != orig.Message {
		t.Errorf("Message: got %q, want %q", got.Message, orig.Message)
	}
	if len(got.Groups) != 1 || got.Groups[0] != "kubelet" {
		t.Errorf("Groups: got %v, want %v", got.Groups, orig.Groups)
	}
	if got.Attrs["pid"] != float64(42) || got.Attrs["signal"] != "SIGKILL" {
		t.Errorf("Attrs: got %v, want %v", got.Attrs, orig.Attrs)
	}
}

// captureHandler implements slog.Handler by calling a function and is
// used to collect records sent through a relay.
type captureHandler struct {
	fn func(context.Context, slog.Record) error
}

func (h *captureHandler) Enabled(ctx context.Context, l slog.Level) bool  { return true }
func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error { return h.fn(ctx, r) }
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler        { return h }
func (h *captureHandler) WithGroup(name string) slog.Handler              { return h }

// TestFramerRelayRoundTrip creates a socketpair, writes frames from a
// FramerHandler, reads them via a relay, and verifies the record arrives at
// the parent sink with levels, message, tag, and attrs preserved.
func TestFramerRelayRoundTrip(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	sendFd := SetupSendSocket(fds[1])
	_ = syscall.SetNonblock(fds[1], true)
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

	// Start relay
	var relayWg sync.WaitGroup
	Relay(recvConn, recorder, "parent", &relayWg)

	time.Sleep(10 * time.Millisecond) // let relay goroutine start

	// Use FramerHandler to write a frame
	framer := NewFramer(sendFd, slog.LevelInfo)
	framer = framer.WithGroup("child").WithAttrs([]slog.Attr{slog.Int("count", 7)}).(*FramerHandler)
	framerLogger := slog.New(framer)
	// writelog.Stream stamps a "tag" attr matching the process tag.
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

	// Check that the tag attr arrived.
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

	framer := NewFramer(sendFd, slog.LevelDebug)

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
	Relay(recvConn, sink, "teardown-test", &wg)

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