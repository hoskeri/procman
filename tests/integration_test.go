// Package tests holds integration tests that exercise the real procman and
// trebuchet binaries end to end.
//
// The Makefile builds every binary once into _output/$GOOS_GOARCH/bin and
// exports its path as PROCMAN_BIN, so `make test` runs these after a single
// build. With a bare `go test ./...` (PROCMAN_BIN unset) they skip.
package tests

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// procmanBin resolves the prebuilt procman binary, skipping the test when the
// Makefile did not point PROCMAN_BIN at it.
func procmanBin(t *testing.T) string {
	t.Helper()
	b := os.Getenv("PROCMAN_BIN")
	if b == "" {
		t.Skip("PROCMAN_BIN not set; run `make test` to build binaries and run integration tests")
	}
	abs, err := filepath.Abs(b)
	if err != nil {
		t.Fatalf("resolve PROCMAN_BIN: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("procman binary %q missing; run `make test`", abs)
	}
	return abs
}

// trebuchetPath is the trebuchet binary sitting next to procman in the output
// bin directory.
func trebuchetPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(procmanBin(t)), "trebuchet")
}

// writeProcfile renders a Procfile template (substituting @TREBUCHET@ with
// the prebuilt trebuchet path) into a temp dir and returns its path.
func writeProcfile(t *testing.T, template string) string {
	t.Helper()
	dir := t.TempDir()
	pf := filepath.Join(dir, "Procfile")
	text := strings.ReplaceAll(template, "@TREBUCHET@", trebuchetPath(t))
	if err := os.WriteFile(pf, []byte(text), 0o644); err != nil {
		t.Fatalf("write Procfile: %v", err)
	}
	return pf
}

// fixtureProcfile renders one of the checked-in tests/Procfile fixtures.
func fixtureProcfile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return writeProcfile(t, string(raw))
}

// runProcman runs the procman binary against pf with optional extra flags and
// returns its stdout, stderr, and exit code. Any failure that is not an
// ordinary non-zero exit (e.g. the binary failing to start) is fatal.
func runProcman(t *testing.T, pf string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	full := append([]string{"-f", pf}, args...)
	cmd := exec.Command(procmanBin(t), full...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	start := time.Now()
	err := cmd.Run()
	t.Logf("procman %v: exit=%d elapsed=%s", full, cmd.ProcessState.ExitCode(), time.Since(start))
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("run procman: %v (stderr: %s)", err, errOut.String())
	}
	return out.String(), errOut.String(), cmd.ProcessState.ExitCode()
}

// TestProcfileClean: both processes print a burst of log lines (through
// procman's throttling pipeline) and exit cleanly; the first to exit on its
// own carries code 0, so the formation exits 0.
func TestProcfileClean(t *testing.T) {
	_, stderr, exit := runProcman(t, fixtureProcfile(t, "Procfile.clean"))
	if exit != 0 {
		t.Errorf("expected exit 0, got %d (stderr: %s)", exit, stderr)
	}
}

// TestProcfileOneFailed: web fails fast with code 1 while work would run for
// ~4s on its own. The formation must terminate work (turning it into a
// teardown, not an exit), attribute the collapse to web, and exit with 1.
func TestProcfileOneFailed(t *testing.T) {
	start := time.Now()
	_, stderr, exit := runProcman(t, fixtureProcfile(t, "Procfile.onefailed"))
	elapsed := time.Since(start)
	if exit != 1 {
		t.Errorf("expected exit 1, got %d (stderr: %s)", exit, stderr)
	}
	if !strings.Contains(stderr, "web exited, exit code 1") {
		t.Errorf("expected stderr to attribute the failure to web, got: %s", stderr)
	}
	// work alone (count=400 × 10ms) needs ~4s; if the teardown did not cut it
	// short the run would take that long.
	if elapsed > 3*time.Second {
		t.Errorf("expected teardown to terminate work shortly after web failed, took %v", elapsed)
	}
}

// TestInterruptIsGraceful: SIGINT to procman is a user-initiated shutdown:
// children are SIGTERMed, procman exits 0 (not 130), well before the child
// would have finished on its own.
func TestInterruptIsGraceful(t *testing.T) {
	pf := writeProcfile(t, `web: "@TREBUCHET@" -count=1000000`)
	cmd := exec.Command(procmanBin(t), "-f", pf)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start procman: %v", err)
	}
	time.Sleep(400 * time.Millisecond) // let the formation come up
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("procman did not exit within 5s of SIGINT (stderr: %s)", errOut.String())
	}
	if exit := cmd.ProcessState.ExitCode(); exit != 0 {
		t.Errorf("expected exit 0 after SIGINT, got %d (stderr: %s)", exit, errOut.String())
	}
}

// TestLaunchFailure: a process whose executable does not exist fails the
// formation with exit code 1.
func TestLaunchFailure(t *testing.T) {
	pf := writeProcfile(t, `web: /nonexistent/procman-missing-binary-xyz`)
	_, _, exit := runProcman(t, pf)
	if exit != 1 {
		t.Errorf("expected exit 1, got %d", exit)
	}
}

// TestHighVolumeThroughput: a real trebuchet run — the stand-in user
// application, ~100 msg/s for ~2s — through procman's piped log pipeline.
// trebuchet's own -max-block assertion is what detects a throttling
// regression in the writelog path: if procman ever back-pressured the child,
// trebuchet would trip it and exit 1.
func TestHighVolumeThroughput(t *testing.T) {
	pf := writeProcfile(t, `web: "@TREBUCHET@" -count=200`)
	_, stderr, exit := runProcman(t, pf)
	if exit != 0 {
		t.Errorf("expected exit 0, got %d (stderr: %s)", exit, stderr)
	}
}
