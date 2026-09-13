package writelog

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreams(t *testing.T) {
	testOutput := &bytes.Buffer{}
	jh := slog.NewJSONHandler(testOutput, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, s slog.Attr) slog.Attr {
			if s.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return s
		},
	})
	lg := slog.New(jh)

	s := Stream(lg, "op0", slog.LevelInfo, StreamConfig{})
	// Two writes that together form two complete lines and one partial line.
	s.Write([]byte("line1\nline2\npartial"))
	// Close should flush the partial line.
	s.Close()

	t.Logf("\n%s\n", testOutput.String())

	got := testOutput.String()
	for _, want := range []string{
		`"msg":"line1\n"`,
		`"msg":"line2\n"`,
		`"msg":"partial"`,
		`"op0":{"tag":"op0"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestStreamLevelRespected(t *testing.T) {
	testOutput := &bytes.Buffer{}
	jh := slog.NewJSONHandler(testOutput, &slog.HandlerOptions{
		Level: slog.LevelWarn,
		ReplaceAttr: func(_ []string, s slog.Attr) slog.Attr {
			if s.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return s
		},
	})
	lg := slog.New(jh)

	// Stream at INFO level — handler is set to WARN, so nothing should appear.
	s := Stream(lg, "proc", slog.LevelInfo, StreamConfig{})
	s.Write([]byte("should not appear\n"))
	s.Close()

	if testOutput.Len() != 0 {
		t.Errorf("expected no output at INFO level with WARN handler, got: %s", testOutput.String())
	}

	// Stream at WARN level — should appear.
	testOutput.Reset()
	s2 := Stream(lg, "proc", slog.LevelWarn, StreamConfig{})
	s2.Write([]byte("should appear\n"))
	s2.Close()

	if !strings.Contains(testOutput.String(), "should appear") {
		t.Errorf("expected output at WARN level, got: %s", testOutput.String())
	}
}

// recordHandler is a minimal slog.Handler that records each record's message
// (i.e. the line emitted by a stream) in the order received.
type recordHandler struct {
	mu  sync.Mutex
	got []string
}

func (r *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (r *recordHandler) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.got = append(r.got, rec.Message)
	r.mu.Unlock()
	return nil
}

func (r *recordHandler) WithGroup(string) slog.Handler      { return r }
func (r *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *recordHandler) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.got...)
}

// TestStreamDrainOrder verifies that, once Close returns, every line that was
// enqueued has been handed to the sink in FIFO order, with the final partial
// line emitted last. This guards the async drain vs. the synchronous legacy
// behavior.
func TestStreamDrainOrder(t *testing.T) {
	rh := &recordHandler{}
	s := Stream(slog.New(rh), "web", slog.LevelInfo, StreamConfig{MaxQueue: 16})

	s.Write([]byte("l1\nl2\n"))
	s.Write([]byte("partial")) // no trailing newline
	s.Close()

	got := rh.snapshot()
	want := []string{"l1\n", "l2\n", "partial"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("drain order/flush mismatch:\n  got:  %v\n  want: %v", got, want)
	}
}

// gateHandler blocks every Handle call on a release channel until the test
// closes it. It records messages once unblocked. The started channel fires
// the first time Handle blocks, so a test can wait until the drain worker is
// definitively stuck inside a sink call before producing more output.
type gateHandler struct {
	mu      sync.Mutex
	got     []string
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (g *gateHandler) Enabled(context.Context, slog.Level) bool { return true }

func (g *gateHandler) Handle(_ context.Context, rec slog.Record) error {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("gateHandler: timed out waiting for release")
	}
	g.mu.Lock()
	g.got = append(g.got, rec.Message)
	g.mu.Unlock()
	return nil
}

func (g *gateHandler) WithGroup(string) slog.Handler      { return g }
func (g *gateHandler) WithAttrs([]slog.Attr) slog.Handler { return g }

func (g *gateHandler) snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.got...)
}

// TestStreamTailDropUnderBackpressure verifies the core throttle contract:
// when the sink is stuck (slow terminal), Write never blocks the producer,
// and once the sink unblocks the only lines that survive are the newest ones
// in the ring (oldest tail-dropped under the configured depth).
func TestStreamTailDropUnderBackpressure(t *testing.T) {
	gh := &gateHandler{release: make(chan struct{}), started: make(chan struct{})}
	s := Stream(slog.New(gh), "web", slog.LevelInfo, StreamConfig{MaxQueue: 2})

	// Seed one line so the drain worker enters Handle and blocks there,
	// proving writes do not block the producer even while the sink is wedged.
	// Write the seed and wait until the worker is blocked inside Handle.
	done := make(chan struct{})
	go func() {
		s.Write([]byte("l0\n"))
		close(done)
	}()
	select {
	case <-gh.started:
	case <-time.After(5 * time.Second):
		t.Fatal("drain worker never entered Handle")
	}

	// Producer must not block even though the sink is wedged.
	writeDone := make(chan struct{})
	go func() {
		// Fill well beyond capacity; older lines must be tail-dropped, not
		// queued forever.
		for _, l := range []string{"l1\n", "l2\n", "l3\n", "l4\n", "l5\n", "l6\n"} {
			if _, err := s.Write([]byte(l)); err != nil {
				t.Errorf("Write errored while sink was wedged: %v", err)
			}
		}
		close(writeDone)
	}()
	select {
	case <-writeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked while sink was wedged — producer back-pressured")
	}

	// Unblock the sink; the worker drains the ring (newest 2) and the seed.
	close(gh.release)
	s.Close()

	got := gh.snapshot()
	// Seed (l0, in-flight) + newest two of the ring (l5, l6). l1-l4 tail-dropped.
	want := []string{"l0\n", "l5\n", "l6\n"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("unexpected surviving lines under backpressure:\n  got:  %v\n  want: %v", got, want)
	}
}

// TestStreamDefaultQueue verifies a zero-value config uses DefaultMaxQueue and
// that writes to a closed stream are dropped (not fatal) when the worker has
// already exited.
func TestStreamDefaultQueue(t *testing.T) {
	rh := &recordHandler{}
	s := Stream(slog.New(rh), "web", slog.LevelInfo, StreamConfig{})
	if s.(*stream).maxQ != DefaultMaxQueue {
		t.Fatalf("zero-value config did not default to DefaultMaxQueue(%d)", DefaultMaxQueue)
	}
	s.Write([]byte("hello\n"))
	s.Close()
	if got := rh.snapshot(); fmt.Sprint(got) != fmt.Sprint([]string{"hello\n"}) {
		t.Fatalf("expected [hello\\n], got %v", got)
	}
}

func BenchmarkStreams(b *testing.B) {
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	jh := slog.NewJSONHandler(devNull, &slog.HandlerOptions{})
	lg := slog.New(jh)
	s := Stream(lg, "web", slog.LevelInfo, StreamConfig{})
	s2 := Stream(lg, "web2", slog.LevelInfo, StreamConfig{})
	for range b.N {
		s.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrstaaaaaaasdsdffdsdsdsdsdsdsd" + "\n" + "a00002ghijklno")))
		s.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
		s2.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrst" + "\n" + "a00002ghijklmno")))
		s2.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
	}
	_ = s.Close()
	_ = s2.Close()
}
