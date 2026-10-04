package writelog

import (
	"context"
	"log/slog"
	"sync"
	"syscall"
	"testing"
	"time"
)

// collector is a StatusObserver that records what a relay observed.
type collector struct {
	mu    sync.Mutex
	got   map[string][]StatusRecord
	ready chan struct{}
	once  sync.Once
}

func newCollector() *collector {
	return &collector{got: map[string][]StatusRecord{}, ready: make(chan struct{})}
}

func (c *collector) ObserveStatus(identity string, rec StatusRecord) {
	c.mu.Lock()
	c.got[identity] = append(c.got[identity], rec)
	c.mu.Unlock()
	c.once.Do(func() { close(c.ready) })
}

func (c *collector) records(identity string) []StatusRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[identity]
}

// TestFrameStatusExtraction covers the wire contract: a status record is an
// ordinary frame whose reserved attrs are recovered verbatim, and an ordinary
// log record is not mistaken for one.
func TestFrameStatusExtraction(t *testing.T) {
	// Status record: state in the reserved attr, reason in the message.
	f := Frame{
		Version: frameVersion,
		Level:   int(slog.LevelError),
		Stream:  StreamStdout,
		Tag:     "validate",
		Message: "etcd unreachable",
		Attrs: appendAttrSection(nil, []slog.Attr{
			slog.String(AttrStatus, "unhealthy"),
			slog.Duration(AttrSince, 90*time.Second),
			slog.String(AttrDetail, "dial tcp: refused"),
		}),
	}
	rec, ok := frameStatus(f)
	if !ok {
		t.Fatal("frameStatus did not recognize a status frame")
	}
	if rec.State != "unhealthy" || rec.Reason != "etcd unreachable" {
		t.Errorf("rec = %+v", rec)
	}
	if rec.Detail != "dial tcp: refused" {
		t.Errorf("detail = %q", rec.Detail)
	}
	if rec.Since != 90*time.Second {
		t.Errorf("since = %v", rec.Since)
	}

	// An ordinary log record is not a status, even with attrs.
	plain := Frame{Version: frameVersion, Message: "listening", Attrs: appendAttrSection(nil, []slog.Attr{slog.Int("port", 6443)})}
	if _, ok := frameStatus(plain); ok {
		t.Error("a plain record must not be read as a status")
	}
	if _, ok := frameStatus(Frame{Version: frameVersion, Message: "no attrs"}); ok {
		t.Error("a record without attrs must not be read as a status")
	}
}

func TestJoinIdentity(t *testing.T) {
	for _, tc := range []struct{ child, tag, want string }{
		{"webhook", "validate", "webhook/validate"},
		{"webhook", "", "webhook"},
		{"", "validate", "validate"},
		{"", "", ""},
	} {
		if got := joinIdentity(tc.child, tc.tag); got != tc.want {
			t.Errorf("joinIdentity(%q, %q) = %q, want %q", tc.child, tc.tag, got, tc.want)
		}
	}
}

// TestDualRelayObservesStatuses drives the full path — a child framer writes a
// status record, the parent's relay decodes it, records it under
// "child/validate", and still re-emits it as a log record.
func TestDualRelayObservesStatuses(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	sendFd := fds[1]
	recvConn := SetupRecvConn(fds[0])

	var (
		mu   sync.Mutex
		seen []slog.Record
	)
	sink := slog.New(&captureHandler{fn: func(ctx context.Context, r slog.Record) error {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		return nil
	}})

	obs := newCollector()
	var relayWg sync.WaitGroup
	DualRelay(recvConn, sink, "child", StreamStdout, &relayWg, obs)
	time.Sleep(10 * time.Millisecond)

	// The child publishes through a status-style record on a tagged framer.
	framer := NewFramer(sendFd, StreamStdout, slog.LevelDebug)
	framer = framer.WithTag("validate").(*FramerHandler)
	logger := slog.New(framer)
	logger.LogAttrs(context.Background(), slog.LevelError, "etcd unreachable",
		slog.String(AttrStatus, "unhealthy"),
		slog.String(AttrDetail, "dial tcp: refused"))

	select {
	case <-obs.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("relay never observed the status")
	}

	recs := obs.records("child/validate")
	if len(recs) != 1 {
		t.Fatalf("expected 1 status record, got %+v", recs)
	}
	if recs[0].State != "unhealthy" || recs[0].Reason != "etcd unreachable" || recs[0].Detail != "dial tcp: refused" {
		t.Errorf("observed %+v", recs[0])
	}
	if recs := obs.records("child"); len(recs) != 0 {
		t.Errorf("status should be filed under the component identity only, got %+v", recs)
	}

	syscall.Close(sendFd)
	relayWg.Wait()
	recvConn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("expected the status to still be re-emitted as a record, got %d", len(seen))
	}
	if seen[0].Message != "etcd unreachable" {
		t.Errorf("re-emitted message = %q", seen[0].Message)
	}
}
