package procman

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
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-shellwords"
	"golang.org/x/sync/errgroup"
)

// Process represents a running process
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
}

type Formation struct {
	WorkDir   string
	Env       func(string) string
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

	l.WorkDir = path.Dir(fpath)

	return l.Load(src)
}

func (l *Formation) Load(src io.Reader) error {
	ps := []*Process{}
	lineNum := 0

	sc := bufio.NewScanner(src)
	for sc.Scan() {
		lineNum += 1
		if err := sc.Err(); err != nil {
			return err
		}

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
			Environ: baseEnv(),
		})
	}

	l.Processes = ps
	return nil
}

func (l *Formation) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)

	for _, p := range l.Processes {
		eg.Go(func() error {
			logger := l.Sink.WithGroup("procman")
			logger.Info(fmt.Sprintf("%s starting\n", p.Tag))
			err := p.run(ctx, WithLogger(l.Sink))
			logger.Info(fmt.Sprintf("%s exited\n", p.Tag))
			return err
		})
	}

	<-ctx.Done()
	egErr := eg.Wait()
	return egErr
}

type runOptions struct {
	logger *slog.Logger
}

func (ro *runOptions) Apply(os ...Option) {
	for _, o := range os {
		o(ro)
	}
}

func baseEnv() (ret []string) {
	for _, e := range []string{"PATH", "HOME", "USERNAME", "LOGNAME"} {
		ret = append(ret, e+"="+os.Getenv(e))
	}
	return ret
}

type Option func(o *runOptions)

func WithLogger(l *slog.Logger) Option {
	return func(o *runOptions) {
		o.logger = l
	}
}

func (p *Process) run(ctx context.Context, opt ...Option) error {
	o := &runOptions{
		logger: slog.Default(),
	}
	o.Apply(opt...)

	c := exec.CommandContext(ctx, p.CmdArgs[0], p.CmdArgs[1:]...)
	c.Stdout = Stream(o.logger, p.Tag)
	c.Stderr = Stream(o.logger, p.Tag)
	c.WaitDelay = 10 * time.Second
	c.Env = baseEnv()
	if len(p.Environ) > 0 {
		c.Env = p.Environ
	}
	c.Dir = p.Workdir
	c.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	err := c.Run()
	return err
}

func (p *Process) Exec(ctx context.Context, opt ...Option) error {
	slog.Debug("p.exec", "args", p.CmdArgs)
	e, err := exec.LookPath(p.CmdArgs[0])
	if err != nil {
		return err
	}
	return syscall.Exec(e, p.CmdArgs, p.Environ)
}
