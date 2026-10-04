package writelog

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

// groupRecord captures the group path a record was emitted under, so tests can
// assert the nesting the parent relay applies.
type groupRecord struct {
	groups  []string
	message string
}

// groupRecorder is a slog.Handler that remembers the WithGroup chain leading
// to each record.  Unlike captureHandler (which collapses groups), it keeps
// them separate.
type groupRecorder struct {
	groups []string
	mu     *sync.Mutex
	got    *[]groupRecord
}

func (h *groupRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *groupRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.got = append(*h.got, groupRecord{
		groups:  append([]string(nil), h.groups...),
		message: r.Message,
	})
	return nil
}

func (h *groupRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *groupRecorder) WithGroup(name string) slog.Handler {
	h2 := *h
	h2.groups = append(append([]string(nil), h.groups...), name)
	return &h2
}

// TestTaggedLoggerNested verifies that a component logger built from a nested
// framer multiplexes onto the parent channel and lands under
// "<process>/<tag>" after the relay.
func TestTaggedLoggerNested(t *testing.T) {
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
		got []groupRecord
	)
	recorder := slog.New(&groupRecorder{mu: &mu, got: &got})

	var relayWg sync.WaitGroup
	DualRelay(recvConn, recorder, "webhook", StreamStderr, &relayWg)
	time.Sleep(10 * time.Millisecond) // let the relay goroutine start

	// Same shape ChildSinks produces for a nested process: a stderr framer.
	probe := func() (stdout, stderr *slog.Logger) {
		return nil, slog.New(NewFramer(SetupSendSocket(sendFd), StreamStderr, slog.LevelDebug))
	}
	logger := taggedLogger("authn", probe)
	logger.Info("admission check")

	syscall.Close(sendFd)
	relayWg.Wait()
	recvConn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d: %+v", len(got), got)
	}
	if want := []string{"webhook", "authn"}; !reflect.DeepEqual(got[0].groups, want) {
		t.Errorf("groups: got %v, want %v", got[0].groups, want)
	}
	if got[0].message != "admission check" {
		t.Errorf("message: got %q", got[0].message)
	}
}

// TestTaggedLoggerNestedFallsBackToStdout verifies the stream preference: a
// stderr framer wins when present, but a stdout-only channel is still used.
func TestTaggedLoggerNestedFallsBackToStdout(t *testing.T) {
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
		got []groupRecord
	)
	recorder := slog.New(&groupRecorder{mu: &mu, got: &got})
	var relayWg sync.WaitGroup
	DualRelay(recvConn, recorder, "webhook", StreamStdout, &relayWg)
	time.Sleep(10 * time.Millisecond)

	probe := func() (stdout, stderr *slog.Logger) {
		return slog.New(NewFramer(SetupSendSocket(sendFd), StreamStdout, slog.LevelDebug)), nil
	}
	taggedLogger("authz", probe).Info("decision")

	syscall.Close(sendFd)
	relayWg.Wait()
	recvConn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	if want := []string{"webhook", "authz"}; !reflect.DeepEqual(got[0].groups, want) {
		t.Errorf("groups: got %v, want %v", got[0].groups, want)
	}
}

// TestTaggedLoggerReusesDefaultFramer verifies that an already-installed
// framer default (as procman's own main installs in nested mode) is reused
// instead of probing the standard streams again.
func TestTaggedLoggerReusesDefaultFramer(t *testing.T) {
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
		got []groupRecord
	)
	recorder := slog.New(&groupRecorder{mu: &mu, got: &got})
	var relayWg sync.WaitGroup
	DualRelay(recvConn, recorder, "webhook", StreamStderr, &relayWg)
	time.Sleep(10 * time.Millisecond)

	prev := slog.Default()
	slog.SetDefault(slog.New(NewFramer(SetupSendSocket(sendFd), StreamStderr, slog.LevelInfo)))
	defer slog.SetDefault(prev)

	called := false
	probe := func() (stdout, stderr *slog.Logger) {
		called = true
		return nil, nil
	}
	taggedLogger("authn", probe).Info("reused")

	if called {
		t.Error("probe must not be called when the default handler is already a framer")
	}

	syscall.Close(sendFd)
	relayWg.Wait()
	recvConn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	if want := []string{"webhook", "authn"}; !reflect.DeepEqual(got[0].groups, want) {
		t.Errorf("groups: got %v, want %v", got[0].groups, want)
	}
}

// TestTaggedLoggerNotNested verifies the graceful fallback: with no nested
// channel the helper groups the process's default logger under the tag.
func TestTaggedLoggerNotNested(t *testing.T) {
	var (
		mu  sync.Mutex
		got []groupRecord
	)
	prev := slog.Default()
	slog.SetDefault(slog.New(&groupRecorder{mu: &mu, got: &got}))
	defer slog.SetDefault(prev)

	probe := func() (stdout, stderr *slog.Logger) { return nil, nil }
	taggedLogger("audit", probe).Info("standalone")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	if want := []string{"audit"}; !reflect.DeepEqual(got[0].groups, want) {
		t.Errorf("groups: got %v, want %v", got[0].groups, want)
	}
}
