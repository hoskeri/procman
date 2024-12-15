package procman

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

type Process struct {
	Name    string
	Environ []string
	CmdArgs []string
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

func (p *Process) Run(ctx context.Context, opt ...Option) error {
	o := &runOptions{
		output: os.Stdout,
	}

	o.Apply(opt...)

	stdOutWriter := newPrefixWriter(o.output, fmt.Sprintf("%-10s O | ", p.Name))
	stdErrWriter := newPrefixWriter(o.output, fmt.Sprintf("%-10s E | ", p.Name))

	cmd := &exec.Cmd{
		Args:   p.CmdArgs,
		Path:   p.CmdArgs[0],
		Env:    p.Environ,
		Stdin:  os.Stdin,
		Stdout: stdOutWriter,
		Stderr: stdErrWriter,
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		return err
	}

	return nil
}
