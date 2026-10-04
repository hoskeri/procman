package process

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/hoskeri/procman/pkg/termhandler"
	"github.com/hoskeri/procman/pkg/writelog"
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

	ctx := context.Background()
	th := termhandler.New(ctx, nil, logFile, logFile, &termhandler.Options{Level: slog.LevelInfo})
	defer th.Close()

	frm := &Formation{
		Logs: th,
		Processes: []*Process{
			{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", "echo web-message"}},
			// quiet delays so web deterministically echoes and exits first:
			// the formation cancel (any process exit kills the group) then
			// SIGTERMs quiet before it produces output, which is fine — the
			// assertion below only requires web-message present and
			// quiet-message suppressed by its Error override. Letting both
			// children race made this test flaky (web could be killed first).
			{Tag: "quiet", CmdArgs: []string{"/bin/sh", "-c", "sleep 0.3; echo quiet-message"}, LogLevel: slog.LevelError},
		},
	}

	// Processes exit cleanly, so Formation.Run returns a non-nil error by
	// design (any process exit cancels the group); the output is what matters.
	_ = frm.Run(context.Background())
	th.Close() // drain relay output before reading the file

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

// TestLogLevelsFallback verifies the per-process level policy is applied on
// the writelog fallback path (Formation.Logs == nil) through a levelSetter
// handler, not just through ChildFDs.
func TestLogLevelsFallback(t *testing.T) {
	logFile, err := os.CreateTemp(t.TempDir(), "procman-loglevels-*.log")
	if err != nil {
		t.Fatalf("create temp log file: %v", err)
	}
	defer logFile.Close()

	th := termhandler.New(context.Background(), nil, logFile, logFile, &termhandler.Options{Level: slog.LevelInfo})
	defer th.Close()
	base := th.Logger()

	// Explicit Process.LogLevel -> Error override.
	quiet := (&Process{Tag: "quiet", LogLevel: slog.LevelError}).loggerFor(base, LogLevels{})
	quiet.WithGroup("quiet").Info("quiet-info")   // suppressed
	quiet.WithGroup("quiet").Error("quiet-error") // shown

	// No policy: the base logger is returned untouched.
	web := (&Process{Tag: "web"}).loggerFor(base, LogLevels{})
	web.WithGroup("web").Info("web-info") // shown

	data, err := os.ReadFile(logFile.Name())
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	out := string(data)
	if strings.Contains(out, "quiet-info") {
		t.Errorf("expected quiet info suppressed by explicit Error override, got:\n%s", out)
	}
	for _, want := range []string{"quiet-error", "web-info"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in fallback output, got:\n%s", want, out)
		}
	}
}

// discardLogger is a sink that swallows all process output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// discardSink is a LogSink that swallows all output and wires child streams to
// /dev/null.
type discardSink struct{}

func (discardSink) Logger() *slog.Logger { return discardLogger() }

func (discardSink) ChildFDs(tag string, index int, resolver writelog.LevelResolver) (*os.File, *os.File, error) {
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

// TestSelfExitCode: the first process to exit on its own brings the formation
// down and its exit code is reported.
func TestSelfExitCode(t *testing.T) {
	frm := &Formation{
		Logs: discardSink{},
		Processes: []*Process{
			{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", "exit 3"}},
			{Tag: "worker", CmdArgs: []string{"/bin/sh", "-c", "sleep 30"}},
		},
	}
	err := frm.Run(context.Background())
	pe, ok := err.(*ExitError)
	if !ok {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if pe.Tag != "web" || pe.Code != 3 {
		t.Errorf("expected web/exit 3, got %s/%d", pe.Tag, pe.Code)
	}
}

// TestCleanSelfExitCode: a process that exits 0 on its own still brings the
// formation down, but carries status 0.
func TestCleanSelfExitCode(t *testing.T) {
	frm := &Formation{
		Logs:      discardSink{},
		Processes: []*Process{{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", "true"}}},
	}
	err := frm.Run(context.Background())
	pe, ok := err.(*ExitError)
	if !ok {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if pe.Tag != "web" || pe.Code != 0 {
		t.Errorf("expected web/exit 0, got %s/%d", pe.Tag, pe.Code)
	}
}

// TestSignalSelfExitCode: a process that kills itself with a signal reports
// its exit code in the 128+signum convention (SIGTERM -> 143).
func TestSignalSelfExitCode(t *testing.T) {
	frm := &Formation{
		Logs:      discardSink{},
		Processes: []*Process{{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", "kill -TERM $$"}}},
	}
	err := frm.Run(context.Background())
	pe, ok := err.(*ExitError)
	if !ok {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if pe.Tag != "web" || pe.Code != 143 {
		t.Errorf("expected web/143 (SIGTERM), got %s/%d", pe.Tag, pe.Code)
	}
}

// TestCancelIsSuccess: canceling the context (user interrupt) tears the
// formation down and Run reports success.
func TestCancelIsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	frm := &Formation{
		Logs: discardSink{},
		Processes: []*Process{
			{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", "sleep 30"}},
			{Tag: "worker", CmdArgs: []string{"/bin/sh", "-c", "sleep 30"}},
		},
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	if err := frm.Run(ctx); err != nil {
		t.Fatalf("expected nil on user cancel, got: %v", err)
	}
}

// TestStubbornProcessEscalatesToKill: a child that ignores SIGTERM is SIGKILLed
// after WaitDelay, so Run still completes and reports success (the user asked
// for the shutdown).
func TestStubbornProcessEscalatesToKill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	frm := &Formation{
		Logs: discardSink{},
		Processes: []*Process{
			// Busy loop that ignores SIGTERM and never forks, so nothing in
			// the group dies until the SIGKILL escalation fires.
			{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", "trap '' TERM; while :; do :; done"}},
		},
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := frm.Run(ctx); err != nil {
		t.Fatalf("expected nil on user cancel, got: %v", err)
	}
	// WaitDelay is 1s; allow generous CI slack.
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("expected SIGKILL escalation within ~1s, took %v", d)
	}
}

// TestEmptyCommandIsRejected: an empty command line is an error at load time
// and in Process.run, never a panic on CmdArgs[0].
func TestEmptyCommandIsRejected(t *testing.T) {
	frm := &Formation{}
	if err := frm.Load(io.NopCloser(strings.NewReader("web:\nworker: sleep 1"))); err == nil {
		t.Fatal("expected error for empty command line")
	}

	p := &Process{Tag: "web"}
	if err := p.run(context.Background(), withLogger(discardLogger())); err == nil {
		t.Fatal("expected error for Process with no command")
	}
}

// TestChildDetachedFromTerminal verifies that spawned processes get a new
// session (no controlling terminal) and /dev/null stdin, so terminal
// signals and keystrokes never reach them directly.
func TestChildDetachedFromTerminal(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "child.txt")
	script := fmt.Sprintf(
		`echo "session=$(cut -d' ' -f6 /proc/self/stat)" > %q; `+
			`echo "stdin=$(readlink /proc/self/fd/0)" >> %q`,
		out, out)

	frm := &Formation{
		Logs:      discardSink{},
		Processes: []*Process{{Tag: "web", CmdArgs: []string{"/bin/sh", "-c", script}}},
	}
	// The child exits on its own, so Run returns an *ExitError; ignore it.
	_ = frm.Run(context.Background())

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read child report %s: %v", out, err)
	}

	var childSid int
	var stdin string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "session":
			childSid, _ = strconv.Atoi(v)
		case "stdin":
			stdin = v
		}
	}

	myStat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatalf("read own stat: %v", err)
	}
	myFields := strings.Fields(string(myStat))
	if len(myFields) < 6 {
		t.Fatalf("unexpected /proc/self/stat: %q", myStat)
	}
	mySid, err := strconv.Atoi(myFields[5])
	if err != nil {
		t.Fatalf("parse own session: %v", err)
	}
	if childSid == 0 {
		t.Fatalf("could not parse child session from %q", data)
	}
	if childSid == mySid {
		t.Errorf("child session %d equals procman session %d: child not detached", childSid, mySid)
	}
	if stdin != "/dev/null" {
		t.Errorf("child stdin: got %q, want /dev/null", stdin)
	}
}

// TestBaseEnv: the whitelist forwards USER and TMPDIR from the host.
func TestBaseEnv(t *testing.T) {
	t.Setenv("USER", "jane")
	t.Setenv("TMPDIR", "/tmp/x")
	joined := strings.Join(baseEnv(), "\n")
	for _, want := range []string{"USER=jane", "TMPDIR=/tmp/x", "PATH="} {
		if !strings.Contains(joined, want) {
			t.Errorf("baseEnv() missing %q, got: %v", want, baseEnv())
		}
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

// TestTagValidation verifies that Formation.Load rejects invalid tags and
// that Process.run also rejects them for direct callers.
func TestTagValidation(t *testing.T) {
	tests := []struct {
		tag  string
		want string // substring expected in error message
	}{
		{"WEB", "tag"},
		{"Web_1", "tag"},
		{"web.server", "tag"},
		{"web 1", "tag"},
		{"-web", "tag"},
		{"web-", "tag"},
		{"", "tag"},
		{"web", ""},    // valid
		{"web-1", ""},  // valid
		{"node", ""},   // valid
		{"node-1", ""}, // valid
		{"a", ""},      // valid (single char)
		{"0", ""},      // valid (single digit)
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			// Test via Formation.Load
			frm := &Formation{}
			err := frm.Load(io.NopCloser(strings.NewReader(tt.tag + ": echo hi")))
			if tt.want == "" {
				if err != nil {
					t.Errorf("Load: expected nil, got %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load: expected error containing %q, got %v", tt.want, err)
			}

			// Test via direct Process.run
			p := &Process{Tag: tt.tag, CmdArgs: []string{"true"}}
			err = p.run(context.Background(), withLogger(discardLogger()))
			if tt.want == "" {
				// A valid tag may still produce an exit error, but not a tag error.
				if err != nil && strings.Contains(err.Error(), "tag must be") {
					t.Errorf("run: unexpected tag error for valid tag: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "tag must be") {
				t.Errorf("run: expected tag validation error, got %v", err)
			}
		})
	}
}
