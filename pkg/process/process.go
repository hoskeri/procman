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
	// LogLevel is an explicit per-process level override.  A non-zero value
	// wins over Formation.LogLevels; the zero value (slog.LevelInfo) means
	// "unset" and defers to the formation's default/tag override.
	LogLevel slog.Level
}

// LogSink is the formation's output destination.  It provides the logger for
// the formation's own records and vends per-child log descriptors so process
// stdout/stderr can be captured without the formation knowing the transport
// (terminal rendering, nested framing, ...).  termhandler.TermHandler
// implements it.
type LogSink interface {
	// Logger returns the sink for the formation's own lifecycle records.
	Logger() *slog.Logger
	// ChildFDs returns the child-side stdout and stderr descriptors for a
	// process; the caller owns and closes them after the process exits.  The
	// resolver carries the formation's log-level policy so the sink can apply
	// per-component overrides to relayed records.
	ChildFDs(tag string, index int, resolver writelog.LevelResolver) (stdout, stderr *os.File, err error)
}

// Formation is the set of process from a procfile.
type Formation struct {
	Workdir   string
	Processes []*Process
	// Logs is the output facade.  When nil, child output falls back to the
	// writelog text pipeline into slog.Default().
	Logs LogSink
	// LogLevels sets the default log level and per-tag overrides for process
	// output.  An explicit Process.LogLevel wins; otherwise the tag override,
	// then Default.  The zero value leaves output at the sink's own level
	// (Default=Info, no overrides).
	LogLevels LogLevels
}

// New parses a new procfile formatted specification.
func New(fpath string) (*Formation, error) {
	f := &Formation{}
	if err := f.LoadFile(fpath); err != nil {
		return nil, err
	}

	return f, nil
}

// LoadFile loads a procfile.
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

// Load loads a procfile from a reader.
func (l *Formation) Load(src io.ReadCloser) error {
	records, err := procfile.Parse(src)
	if err != nil {
		return err
	}

	ps := []*Process{}
	for _, r := range records {
		if !validTag(r.Tag) {
			return fmt.Errorf("process tag %q: must be lowercase alphanumeric (may contain dashes)", r.Tag)
		}
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

// logLevel returns the effective level for p: an explicit Process.LogLevel
// wins; otherwise the formation's tag override or default applies.
func (p *Process) logLevel(levels LogLevels) slog.Level {
	if p.LogLevel != 0 {
		return p.LogLevel
	}
	return levels.For(p.Tag)
}

// resolver returns the per-identity log-level policy for p.  An explicit
// Process.LogLevel wins for every record; otherwise the formation's LogLevels
// policy is consulted with the record's component tag path, so overrides can
// name the process tag ("webhook"), the full path ("webhook/validate"), or the
// component alone ("validate").  See LogLevels.ForIdentity.
func (p *Process) resolver(levels LogLevels) writelog.LevelResolver {
	explicit := p.LogLevel
	tag := p.Tag
	return func(groups []string) (slog.Level, bool) {
		if explicit != 0 {
			return explicit, true
		}
		return levels.ForIdentity(tag, groups)
	}
}

// applies reports whether levels overrides anything for p (i.e. the formation
// carries an explicit policy rather than the zero value).
func (levels LogLevels) applies(p *Process) bool {
	if p.LogLevel != 0 {
		return true
	}
	if _, ok := levels.Tags[p.Tag]; ok {
		return true
	}
	return levels.Default != 0
}

// levelSetter is implemented by slog.Handlers that can produce per-group
// handlers with an overridden minimum log level (e.g. termhandler's renderer
// and writelog.FramerHandler).  It is matched structurally to avoid a hard
// package dependency.
type levelSetter interface {
	WithOverride(name string, lvl slog.Leveler) slog.Handler
}

// loggerFor returns base with p's effective level applied as a per-group
// override, when the formation has a level policy and the handler supports it.
// It backs the writelog fallback used when Formation.Logs is nil; the LogSink
// path receives the level directly through ChildFDs.
func (p *Process) loggerFor(base *slog.Logger, levels LogLevels) *slog.Logger {
	if base == nil || !levels.applies(p) {
		return base
	}
	if ls, ok := base.Handler().(levelSetter); ok {
		return slog.New(ls.WithOverride(p.Tag, p.logLevel(levels)))
	}
	return base
}

// validTag reports whether tag conforms to the enforced naming convention:
// lowercase alphanumeric, may contain interior dashes (no leading/trailing).
func validTag(tag string) bool {
	if len(tag) == 0 {
		return false
	}
	if tag[0] == '-' || tag[len(tag)-1] == '-' {
		return false
	}
	for _, c := range tag {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

// Run executes every process concurrently. The first process to exit on its
// own — cleanly or crashing — brings the formation down: the siblings are
// terminated (SIGTERM, then SIGKILL after WaitDelay) and Run returns that
// process's status as an *ExitError carrying its exit code. If the formation
// was instead shut down by canceling ctx (e.g. a user interrupt), every
// process was torn down rather than exiting of its own accord, and Run
// returns nil.
//
// Child stdout/stderr are captured through the formation's Logs facade (e.g.
// termhandler.TermHandler), which owns the transport and detects whether the
// process is nested inside a parent formation.  When Logs is nil, output goes
// through the writelog text pipeline into slog.Default().
func (l *Formation) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)

	base := slog.Default()
	if l.Logs != nil {
		base = l.Logs.Logger()
	}
	// The formation's own lifecycle records are tagged "procman" (a display
	// tag, not an attribute namespace) so the renderer prefixes them.
	procmanLog := writelog.WithTag(base, "procman")

	// Create one child channel per process per stream.  The LogSink owns the
	// transport: it may render text to a terminal or frame records up to a
	// parent formation.  Without a LogSink, process output falls back to the
	// writelog text pipeline in Process.run.
	type childIO struct {
		stdout *os.File
		stderr *os.File
	}
	ios := make([]childIO, len(l.Processes))
	if l.Logs != nil {
		for i, p := range l.Processes {
			stdout, stderr, err := l.Logs.ChildFDs(p.Tag, p.Index, p.resolver(l.LogLevels))
			if err != nil {
				for j := range i {
					ios[j].stdout.Close()
					ios[j].stderr.Close()
				}
				return err
			}
			ios[i] = childIO{stdout: stdout, stderr: stderr}
		}
	}

	for i, p := range l.Processes {
		ch := ios[i]
		p := p
		eg.Go(func() error {
			if l.Logs != nil {
				defer ch.stdout.Close()
				defer ch.stderr.Close()
			}
			procmanLog.Warn(fmt.Sprintf("starting %s", p.Tag))
			logger := base
			if l.Logs == nil {
				logger = p.loggerFor(base, l.LogLevels)
			}
			err := p.run(ctx,
				withStdoutFile(ch.stdout),
				withStderrFile(ch.stderr),
				withLogger(logger),
			)
			if err != nil {
				procmanLog.Warn(err.Error())
			}
			return err
		})
	}

	return eg.Wait()
}

type runOptions struct {
	logger     *slog.Logger
	extraFiles []*os.File
	envAdd     []string
	stdoutFile *os.File // when set, used as the child's stdout (instead of writelog.Stream)
	stderrFile *os.File // when set, used as the child's stderr (instead of writelog.Stream)
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

func withExtraFiles(files []*os.File) runOption {
	return func(o *runOptions) {
		o.extraFiles = files
	}
}

func withEnvAdd(entries []string) runOption {
	return func(o *runOptions) {
		o.envAdd = entries
	}
}

func withStdoutFile(f *os.File) runOption {
	return func(o *runOptions) {
		o.stdoutFile = f
	}
}

func withStderrFile(f *os.File) runOption {
	return func(o *runOptions) {
		o.stderrFile = f
	}
}

func (p *Process) run(ctx context.Context, opt ...runOption) error {
	o := &runOptions{
		logger: slog.Default(),
	}
	o.Apply(opt...)

	if !validTag(p.Tag) {
		return fmt.Errorf("%s: tag must be lowercase alphanumeric (may contain dashes)", p.Tag)
	}
	if len(p.CmdArgs) == 0 {
		// Defensive: Formation.Load already rejects empty commands, but a
		// library caller can construct a Process directly.
		return fmt.Errorf("%s: no command", p.Tag)
	}

	c := exec.CommandContext(ctx, p.CmdArgs[0], p.CmdArgs[1:]...)
	// Detach the child from the controlling terminal.  Stdin is the null
	// device (never the terminal), and Setsid gives the child its own session
	// and process group so terminal-generated signals (SIGINT from Ctrl-C,
	// SIGTSTP, SIGQUIT) only ever reach procman, which decides when to tear
	// the group down via Cancel.  The group kill still works because a session
	// leader's pgid is its own pid.
	c.Stdin = nil

	// Process output is logged.  When a stdout/stderr socket is provided
	// (Formation.Run through a LogSink), the fds are wired directly so procman
	// children can use their framers and non-procman children write text into
	// the socket for the relay to dispatch.  Without one, output goes through
	// the regular writelog.Stream text pipeline.
	if o.stdoutFile != nil {
		c.Stdout = o.stdoutFile
	} else {
		stdout := writelog.Stream(o.logger, p.Tag, slog.LevelInfo, writelog.StreamConfig{})
		defer stdout.Close()
		c.Stdout = stdout
	}
	if o.stderrFile != nil {
		c.Stderr = o.stderrFile
	} else {
		stderr := writelog.Stream(o.logger, p.Tag, slog.LevelInfo, writelog.StreamConfig{})
		defer stderr.Close()
		c.Stderr = stderr
	}
	c.WaitDelay = 1 * time.Second
	c.Dir = p.Workdir
	c.ExtraFiles = o.extraFiles
	c.Env = baseEnv(append(p.Environ, o.envAdd...)...)
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
		// New session + group: detaches the controlling terminal and keeps
		// the group-wide kill in Cancel working (pgid == child pid).
		Setsid: true,
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

// Exec calls os.exec, replacing this process.
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
