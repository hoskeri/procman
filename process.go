package procman

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/mattn/go-shellwords"
	"golang.org/x/sync/errgroup"
)

var logger = slog.Default()

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

	src, err := os.Open("Procfile")
	if err != nil {
		return err
	}

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
			logger.Info("formation.run", "start", p)
			err := p.run(ctx, WithLogger(l.Sink))
			logger.Info("formation.run", "exit", p, "err", err)
			return err
		})
	}

	logger.Info("formation.run, process start complete, waiting")
	<-ctx.Done()
	egErr := eg.Wait()
	ctxErr := ctx.Err()
	logger.Info("formation.run, done waiting", "egErr", egErr, "ctxErr", ctxErr)
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
	c.Env = p.Environ
	c.Dir = p.Workdir
	c.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	logger.Info("process.run", "run", c)
	err := c.Run()
	logger.Info("process.run", "exit", c, "err", err)
	return err
}

func (p *Process) Exec(ctx context.Context, opt ...Option) error {
	slog.Info("p.exec", "args", p.CmdArgs)
	e, err := exec.LookPath(p.CmdArgs[0])
	if err != nil {
		return err
	}
	return syscall.Exec(e, p.CmdArgs, p.Environ)
}
