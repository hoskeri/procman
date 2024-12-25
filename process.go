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
	"strings"
	"syscall"

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
	// Actual command line, include executable.
	CmdArgs []string
	// Working directory
	Workdir string
}

type Formation struct {
	WorkDir   string
	Env       func(string) string
	Processes []*Process
}

func New() (*Formation, error) {
	return &Formation{}, nil
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
			return p.run(ctx)
		})
	}

	<-ctx.Done()
	return ctx.Err()
}

type runOptions struct {
	output io.Writer
}

func (ro *runOptions) Apply(os ...Option) {
	for _, o := range os {
		o(ro)
	}
}

type Option func(o *runOptions)

func WithOutput(output io.Writer) Option {
	return func(o *runOptions) {
		o.output = output
	}
}

func (p *Process) run(ctx context.Context, opt ...Option) error {
	o := &runOptions{
		output: os.Stdout,
	}

	o.Apply(opt...)

	stdOutWriter := newPrefixWriter(o.output, fmt.Sprintf("%-10s | ", p.Tag))
	stdErrWriter := newPrefixWriter(o.output, fmt.Sprintf("%-10s | ", p.Tag))

	c := exec.CommandContext(ctx, p.CmdArgs[0], p.CmdArgs[1:]...)
	c.Stdout = stdOutWriter
	c.Stderr = stdErrWriter
	c.Env = p.Environ
	c.Dir = p.Workdir
	c.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Setsid:  true,
	}

	return c.Run()
}
