package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sync"
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

// levelSetter is implemented by slog.Handlers that can produce per-group
// handlers with an overridden minimum log level (e.g. termhandler.TermHandler).
// It is matched structurally to avoid a hard package dependency.
type levelSetter interface {
	WithOverride(name string, lvl slog.Leveler) slog.Handler
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
// Nested (child) formations detect their parent by probing stdout (fd 1):
// if it is a SOCK_SEQPACKET socket, the formation's sink is replaced by a
// framer that encodes each record as a structured binary frame and sends it
// up the socket.  Otherwise the formation runs in root mode with its own
// sink.
func (l *Formation) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)

	// --------------------------------------------------------------------
	// 1. Nesting detection: probe stdout (fd 1). If stdout is a
	//    SOCK_SEQPACKET socket (parent formation), replace the sink with
	//    a framer that writes binary frames to fd 1.
	// --------------------------------------------------------------------
	if childFramer := writelog.NewChildFramer(); childFramer != nil {
		l.Sink = childFramer
	}

	// --------------------------------------------------------------------
	// 2. Create one SOCK_SEQPACKET socketpair per child. The send side
	//    becomes the child's stdout; the recv side stays in this process
	//    and is read by a DualRelay dispatcher that handles both frames
	//    (procman children) and text (regular binaries).
	// --------------------------------------------------------------------
	type childChan struct {
		recvConn *net.UnixConn
		sendFile *os.File // send side of the socketpair → child's stdout
		tag      string
	}
	var chans []childChan

	for _, p := range l.Processes {
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
		if err != nil {
			return fmt.Errorf("stdout socketpair for %s: %w", p.Tag, err)
		}
		// The send side is left blocking intentionally: non-procman children
		// expect a blocking stdout.  Procman children self-set O_NONBLOCK
		// after detecting the socket type via SO_TYPE.
		recvConn := writelog.SetupRecvConn(fds[0])
		if recvConn == nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
			return fmt.Errorf("SetupRecvConn for %s failed", p.Tag)
		}
		chans = append(chans, childChan{
			recvConn: recvConn,
			sendFile: os.NewFile(uintptr(fds[1]), fmt.Sprintf("stdout-%s", p.Tag)),
			tag:      p.Tag,
		})
	}

	// --------------------------------------------------------------------
	// 3. Launch one DualRelay goroutine per child socket. Each relay reads
	//    messages from the recv side and dispatches to either the structured
	//    frame path or the text-splitting path based on the message header.
	// --------------------------------------------------------------------
	var relayWg sync.WaitGroup
	for _, ch := range chans {
		writelog.DualRelay(ch.recvConn, l.Sink, ch.tag, &relayWg)
	}

	// --------------------------------------------------------------------
	// 4. Spawn child processes in the errgroup. Each child's stdout is the
	//    send side of its socketpair (fd 1).  Stderr goes through the
	//    regular writelog.Stream text pipeline.
	// --------------------------------------------------------------------
	for i, p := range l.Processes {
		ch := chans[i]
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
		p := p
		eg.Go(func() error {
			defer ch.sendFile.Close()
			logger := l.Sink.WithGroup("procman")
			logger.Warn(fmt.Sprintf("starting %s", p.Tag))
			err := p.run(ctx, withLogger(procLogger), withStdoutFile(ch.sendFile))
			if err != nil {
				logger.Warn(err.Error())
			}
			return err
		})
	}

	err := eg.Wait()

	// --------------------------------------------------------------------
	// 5. Signal all relays to stop by setting a past deadline on their
	//    recv sockets (unblocking the reader). Close the recv sockets and
	//    wait for the relay goroutines to finish.
	// --------------------------------------------------------------------
	stopTime := time.Now().Add(-1 * time.Second)
	for _, ch := range chans {
		ch.recvConn.SetReadDeadline(stopTime)
	}
	for _, ch := range chans {
		ch.recvConn.Close()
	}
	relayWg.Wait()

	return err
}

type runOptions struct {
	logger     *slog.Logger
	extraFiles []*os.File
	envAdd     []string
	stdoutFile *os.File // when set, used as the child's stdout (instead of writelog.Stream)
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
	// Process output is logged.  Stdout, when a stdoutFile is provided
	// (socketpair from Formation.Run), is wired directly so procman children
	// can use the framer and non-procman children write text into the socket
	// for the DualRelay to dispatch.  Stderr always goes through the regular
	// writelog.Stream text pipeline.
	stderr := writelog.Stream(o.logger, p.Tag, slog.LevelInfo, writelog.StreamConfig{})
	defer stderr.Close()
	c.Stdin = nil
	if o.stdoutFile != nil {
		// The send side of a SOCK_SEQPACKET socketpair — child's stdout
		// is a socket, not a pipe.  DualRelay on the recv side handles
		// both binary frames (procman children) and plain text.
		c.Stdout = o.stdoutFile
	} else {
		// Root-mode or direct Process.run caller: text pipe (legacy path).
		stdout := writelog.Stream(o.logger, p.Tag, slog.LevelInfo, writelog.StreamConfig{})
		defer stdout.Close()
		c.Stdout = stdout
	}
	c.Stderr = stderr
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
