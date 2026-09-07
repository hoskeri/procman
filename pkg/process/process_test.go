package process

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/hoskeri/procman/pkg/termhandler"
)

func TestProcess(t *testing.T) {
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	jh := slog.NewJSONHandler(devNull, &slog.HandlerOptions{})
	lg := slog.New(jh)
	p := &Process{
		Tag: "hello",
		CmdArgs: []string{
			"/usr/bin/sh", "-c",
			">&2 echo stderr; echo stdout",
		},
	}

	err := p.run(context.Background(), withLogger(lg))
	if err == nil {
		t.Fatal("expected non-nil error on clean exit: any process exit cancels the formation")
	}
	if !strings.Contains(err.Error(), "hello exited") {
		t.Fatalf("expected clean-exit error, got: %v", err)
	}
}

// TestPerProcessLogLevelOverride runs two real processes through Formation.Run
// and verifies that a per-process LogLevel override suppresses that process's
// output while other processes keep the global level.
func TestPerProcessLogLevelOverride(t *testing.T) {
	logFile, err := os.CreateTemp(t.TempDir(), "procman-loglevel-*.log")
	if err != nil {
		t.Fatalf("create temp log file: %v", err)
	}
	defer logFile.Close()

	th := termhandler.New(logFile, &termhandler.Options{Level: slog.LevelInfo})
	lg := slog.New(th)

	frm := &Formation{
		Sink: lg,
		Processes: []*Process{
			{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", "echo web-message"}},
			{Tag: "quiet", CmdArgs: []string{"/bin/sh", "-c", "echo quiet-message"}, LogLevel: slog.LevelError},
		},
	}

	// Processes exit cleanly, so Formation.Run returns a non-nil error by
	// design (any process exit cancels the group); the output is what matters.
	_ = frm.Run(context.Background())

	data, err := os.ReadFile(logFile.Name())
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	out := string(data)
	t.Logf("process output:\n%s", out)

	if !strings.Contains(out, "web-message") {
		t.Errorf("expected web output to be logged at the global Info level, got:\n%s", out)
	}
	if strings.Contains(out, "quiet-message") {
		t.Errorf("expected quiet output to be suppressed by its per-process Error override, got:\n%s", out)
	}
}

// TestFormation verifies that Formation.Load converts parsed records into
// Processes wired to the formation's Workdir. Procfile parsing itself is
// covered by pkg/procfile/procfile_test.go.
func TestFormation(t *testing.T) {
	frm := &Formation{
		Workdir: "/srv/app",
	}
	err := frm.Load(io.NopCloser(strings.NewReader("web: ./webserver \"hello world\"\ndb: ./mysql 'a b c'")))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	want := []*Process{
		{Tag: "web", CmdArgs: []string{"./webserver", "hello world"}, Workdir: "/srv/app"},
		{Tag: "db", CmdArgs: []string{"./mysql", "a b c"}, Workdir: "/srv/app"},
	}
	if diff := cmp.Diff(want, frm.Processes); diff != "" {
		t.Fatalf("unexpected processes (-want, +got):\n%s", diff)
	}
}
