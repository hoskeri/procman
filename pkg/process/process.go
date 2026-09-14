package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hoskeri/procman/pkg/procfile"
	"github.com/hoskeri/procman/pkg/writelog"
)

// Process represents a single running process.
type Process struct {
	// Short Tag representing the type of process.
	Tag string
	// Index is the n'th process of a type.
	Index int
	// Actual resolved environment.
	Environ []string
	// Actual command line, including executable.
	CmdArgs []string
	// Working directory
	Workdir string
	// LogLevel overrides the default log level for a process.
	LogLevel slog.Level
}

// Formation is the set of process from a procfile.
type Formation struct {
	Workdir   string
	Processes []*Process
	Sink      *slog.Logger
}

func New(fpath string) (*Formation, error) {
	f := &Formation{
		Sink: slog.Default(),
	}
	if err := f.LoadFile(fpath); err != nil {
		return nil, err
	}

	return f, nil
}

func (l *Formation) LoadFile(fpath string) error {
	if fpath == "" {
		return nil
	}

	src, err := os.Open(fpath)
	if err != nil {
		return err
	}

	l.Workdir, _ = filepath.Abs(path.Dir(fpath))

	if l.Workdir != "" {
		slog.Debug("using workdir", "workdir", l.Workdir)
	}

	return l.Load(src)
}

func (l *Formation) Load(src io.ReadCloser) error {
	records, err := procfile.Parse(src)
	if err != nil {
		return err
	}

	ps := []*Process{}
	for _, r := range records {
		if len(r.CmdArgs) == 0 {
			// e.g. a line like "web:" — shellwords parses the empty command
			// into no arguments. Fail fast here instead of panicking on
			// p.CmdArgs[0] inside a goroutine later.
			return fmt.Errorf("process %q: no command", r.Tag)
		}
		ps = append(ps, &Process{
			Tag:     r.Tag,
			CmdArgs: r.CmdArgs,
			Workdir: l.Workdir,
		})
	}

	l.Processes = ps
	return nil
}

// levelSetter is implemented by slog.Handlers that can produce per-group
// handlers with an overridden minimum log level (e.g. termhandler.TermHandler).
// It is matched structurally to avoid a hard package dependency.
type levelSetter interface {
	WithOverride(name string, lvl slog.Leveler) slog.Handler
}

// Run executes every process concurrently. The first process to exit on its
// own — cleanly or crashing — brings the formation down: the siblings are
// terminated (SIGTERM, then SIGKILL after WaitDelay) and Run returns that
// process's status as an *ExitError carrying its exit code. If the formation
// was instead shut down by canceling ctx (e.g. a user interrupt), every
// process was torn down rather than exiting of its own accord, and Run
// returns nil.
func (l *Formation) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)
	for _, p := range l.Processes {
		// Give each process its own logger. When the sink's handler supports
		// per-group overrides and the process carries a non-zero LogLevel,
		// filter this process's output at that level; otherwise the process
		// inherits the handler's global level. A zero-value LogLevel
		// (slog.LevelInfo) means "no override".
		procLogger := l.Sink
		if p.LogLevel != 0 {
			if ls, ok := l.Sink.Handler().(levelSetter); ok {
				procLogger = slog.New(ls.WithOverride(p.Tag, p.LogLevel))
			}
		}

		eg.Go(func() error {
			logger := l.Sink.WithGroup("procman")
			logger.Warn(fmt.Sprintf("starting %s", p.Tag))
			err := p.run(ctx, withLogger(procLogger))
			if err != nil {
				logger.Warn(err.Error())
			}
			return err
		})
	}

	return eg.Wait()
}

type runOptions struct {
	logger *slog.Logger
}

func (ro *runOptions) Apply(os ...runOption) {
	for _, o := range os {
		o(ro)
	}
}

// baseEnv forwards a whitelist of host environment variables to child
// processes, plus any explicit overrides from e (which win on collision).
// Variables not listed here are intentionally not inherited; callers that
// need more must pass them via Process.Environ.
func baseEnv(e ...string) (ret []string) {
	for _, k := range []string{
		"PATH",
		"HOME",
		"USER",
		"USERNAME",
		"LOGNAME",
		"SHELL",
		"TERM",
		"LANG",
		"TMPDIR",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
	} {
		v := os.Getenv(k)
		if v != "" {
			ret = append(ret, k+"="+v)
		}
	}
	return append(ret, e...)
}

type runOption func(o *runOptions)

func withLogger(l *slog.Logger) runOption {
	return func(o *runOptions) {
		o.logger = l
	}
}

func (p *Process) run(ctx context.Context, opt ...runOption) error {
	o := &runOptions{
		logger: slog.Default(),
	}
	o.Apply(opt...)

	if len(p.CmdArgs) == 0 {
		// Defensive: Formation.Load already rejects empty commands, but a
		// library caller can construct a Process directly.
		return fmt.Errorf("%s: no command", p.Tag)
	}

	c := exec.CommandContext(ctx, p.CmdArgs[0], p.CmdArgs[1:]...)
	// Process output is logged at a fixed base level; Process.LogLevel only
	// adjusts the per-group minimum threshold (see Formation.Run), so the two
	// never cancel each other out. Each stream uses the default bounded queue
	// (writelog.DefaultMaxQueue); StreamConfig.MaxQueue remains available to
	// embedders who need to bound it.
	stdout := writelog.Stream(o.logger, p.Tag, slog.LevelInfo, writelog.StreamConfig{})
	stderr := writelog.Stream(o.logger, p.Tag, slog.LevelInfo, writelog.StreamConfig{})
	defer stdout.Close()
	defer stderr.Close()
	c.Stdin = nil
	c.Stdout = stdout
	c.Stderr = stderr
	c.WaitDelay = 1 * time.Second
	c.Env = baseEnv(p.Environ...)
	c.Dir = p.Workdir
	// Soft shutdown: first TERM the whole process group, then KILL it if it
	// has not exited within WaitDelay. Processes spawned in the group die
	// too, so no orphans are left behind. exec's own fallback after WaitDelay
	// would KILL only the direct child, orphaning any grandchildren of a
	// TERM-ignoring process, so the group-wide KILL is scheduled here.
	c.Cancel = func() error {
		err := syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			// The process is already gone (e.g. it exited just as the context
			// was canceled); returning the error would surface it to exec as
			// "exec: canceling Cmd: no such process".
			return nil
		}
		if err == nil {
			time.AfterFunc(c.WaitDelay, func() {
				// Best-effort: the group signal is a no-op (ESRCH) if the
				// process exited during the grace period.
				_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			})
		}
		return err
	}
	c.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	slog.Debug("c.run", "c.args", p.CmdArgs)
	err := c.Run()
	slog.Debug("c.exit", "err", err)
	// A canceled context here means this process did not stop on its own: the
	// formation was torn down (user interrupt, or a sibling exiting first) and
	// the cancellation above killed it. That is not an exit this process chose,
	// so report nothing and let Formation.Run attribute the outcome. The first
	// process to exit on its own is exactly the one that observes a live
	// context: its own return is what cancels the group.
	if ctx.Err() != nil {
		return nil
	}
	return pf(p.Tag, err)
}

// ExitError reports that a process exited on its own — the event that brings
// the whole formation down. Formation.Run returns the first such error, so
// callers can propagate the process's own status as the formation's status:
// Code is 0 through 255 for a normal exit, 128+signum when the process died
// of a signal, and 0 for a clean self-exit.
type ExitError struct {
	Tag  string
	Code int
	msg  string
}

func (e *ExitError) Error() string { return e.msg }

// ExitCode follows the convention used by os.Exit and exec.ExitError.
func (e *ExitError) ExitCode() int { return e.Code }

// pf wraps a process's exit so Formation.Run can attribute the collapse. A
// nil exec error means a clean self-exit; even that is reported as an error
// (with code 0) so that any process exiting on its own stops the others too.
func pf(tag string, err error) error {
	switch ee := err.(type) {
	case *exec.ExitError:
		if code := ee.ExitCode(); code != -1 {
			return &ExitError{Tag: tag, Code: code, msg: fmt.Sprintf("%s exited, exit code %d", tag, code)}
		}
		ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus)
		if !ok {
			return &ExitError{Tag: tag, Code: 137, msg: fmt.Sprintf("%s killed", tag)}
		}
		return &ExitError{Tag: tag, Code: 128 + int(ws.Signal()), msg: fmt.Sprintf("%s signalled, %s", tag, ws.Signal())}
	case nil:
		return &ExitError{Tag: tag, Code: 0, msg: fmt.Sprintf("%s exited", tag)}
	}

	// Not an exit (e.g. the executable could not be launched): keep the cause,
	// but tag it so the failure is attributable.
	return fmt.Errorf("%s: %w", tag, err)
}

func (p *Process) Exec(ctx context.Context, opt ...runOption) error {
	if len(p.CmdArgs) == 0 {
		return fmt.Errorf("%s: no command", p.Tag)
	}
	slog.Debug("p.exec", "args", p.CmdArgs)
	e, err := exec.LookPath(p.CmdArgs[0])
	if err != nil {
		return err
	}
	return syscall.Exec(e, p.CmdArgs, p.Environ)
}
