package process

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hoskeri/procman/pkg/writelog"
)

// bufSink is a LogSink that captures the formation's own records in a buffer
// and wires child output to /dev/null.
type bufSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *bufSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *bufSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *bufSink) Logger() *slog.Logger {
	// Debug level so the test sees every lifecycle event regardless of level.
	return slog.New(slog.NewTextHandler(s, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (s *bufSink) ChildFDs(string, int, writelog.LevelResolver) (*os.File, *os.File, error) {
	stdout, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		stdout.Close()
		return nil, nil, err
	}
	return stdout, stderr, nil
}

// TestFormationName verifies that Formation.Name is validated with the tag
// rules and defaults to "procman" when empty.
func TestFormationName(t *testing.T) {
	if err := (&Formation{Name: "Bad Name"}).Run(context.Background()); err == nil || !strings.Contains(err.Error(), "formation name") {
		t.Fatalf("invalid name: got %v, want a formation name error", err)
	}
	if err := (&Formation{Name: "good-name"}).Run(context.Background()); err != nil {
		t.Fatalf("valid name: unexpected error %v", err)
	}
	if err := (&Formation{}).Run(context.Background()); err != nil {
		t.Fatalf("default name: unexpected error %v", err)
	}
}

// TestLifecycleEventsEmitted verifies that the formation's own starting /
// started / exited events are published through the log stream.
func TestLifecycleEventsEmitted(t *testing.T) {
	s := &bufSink{}
	frm := &Formation{
		Name:      "myapp",
		Logs:      s,
		Processes: []*Process{{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", "true"}}},
	}

	// web exits on its own, so Run returns an *ExitError; the events are what
	// this test asserts.
	_ = frm.Run(context.Background())

	out := s.String()
	for _, want := range []string{"starting web", "started web", "exited web"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in lifecycle output, got:\n%s", want, out)
		}
	}
}
