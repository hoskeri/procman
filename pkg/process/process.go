package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-shellwords"
	"golang.org/x/sync/errgroup"

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
	defer src.Close()
	ps := []*Process{}
	lineNum := 0

	sc := bufio.NewScanner(src)
	for sc.Scan() {
		lineNum += 1

		line := sc.Text()

		if len(line) == 0 {
			continue
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		name, cmd, found := strings.Cut(line, ":")
		if !found {
			return errors.New("invalid line")
		}

		cmdArgs, err := shellwords.Parse(cmd)
		if err != nil {
			return err
		}

		ps = append(ps, &Process{
			Tag:     name,
			CmdArgs: cmdArgs,
			Workdir: l.Workdir,
		})
	}
	if err := sc.Err(); err != nil {
		return err
	}

	l.Processes = ps
	return nil
}

func (l *Formation) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)
	for _, p := range l.Processes {
		// copying loop var not needed.
		eg.Go(func() error {
			logger := l.Sink.WithGroup("procman")
			logger.Warn(fmt.Sprintf("starting %s", p.Tag))
			err := p.run(ctx, withLogger(l.Sink))
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

func baseEnv(e ...string) (ret []string) {
	for _, e := range []string{
		"PATH",
		"HOME",
		"USERNAME",
		"LOGNAME",
		"SHELL",
		"TERM",
		"LANG",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
	} {
		v := os.Getenv(e)
		if v != "" {
			ret = append(ret, e+"="+v)
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

	c := exec.CommandContext(ctx, p.CmdArgs[0], p.CmdArgs[1:]...)
	stdout := writelog.Stream(o.logger, p.Tag, p.LogLevel)
	stderr := writelog.Stream(o.logger, p.Tag, p.LogLevel)
	defer stdout.Close()
	defer stderr.Close()
	c.Stdin = nil
	c.Stdout = stdout
	c.Stderr = stderr
	c.WaitDelay = 1 * time.Second
	c.Env = baseEnv(p.Environ...)
	c.Dir = p.Workdir
	c.Cancel = func() error {
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	slog.Debug("c.run", "c.args", p.CmdArgs)
	err := c.Run()
	slog.Debug("c.exit", "err", err)
	return pf(p.Tag, err)
}

func pf(tag string, err error) error {
	switch ee := err.(type) {
	case *exec.ExitError:
		if ee.ExitCode() == -1 {
			ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus)
			if !ok {
				return fmt.Errorf("%s killed", tag)
			}
			return fmt.Errorf("%s signalled, %s", tag, ws.Signal().String())
		}
		return fmt.Errorf("%s exited, exit code %d", tag, ee.ExitCode())
	case nil:
		// we return an error here so that any process exiting
		// also causes the other processes to stop too.
		return fmt.Errorf("%s exited", tag)
	}

	return err
}

func (p *Process) Exec(ctx context.Context, opt ...runOption) error {
	slog.Debug("p.exec", "args", p.CmdArgs)
	e, err := exec.LookPath(p.CmdArgs[0])
	if err != nil {
		return err
	}
	return syscall.Exec(e, p.CmdArgs, p.Environ)
}
