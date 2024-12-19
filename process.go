package procman

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/mattn/go-shellwords"
)

type Formation struct {
	Workdir string
	Getenv  func(string) string
}

type Process struct {
	Name    string
	CmdArgs []string
}

func (l *Formation) Load(src io.Reader) ([]Process, error) {
	sc := bufio.NewScanner(src)
	ps := []Process{}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}

		name, cmd, found := strings.Cut(line, ":")
		if !found {
			return nil, errors.New("invalid line")
		}

		cmdArgs, err := shellwords.Parse(cmd)
		if err != nil {
			return nil, err
		}

		ps = append(ps, Process{
			Name:    name,
			CmdArgs: cmdArgs,
		})
	}
	return ps, nil
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

	stdOutWriter := newPrefixWriter(o.output, fmt.Sprintf("%-10s | ", p.Name))
	stdErrWriter := newPrefixWriter(o.output, fmt.Sprintf("%-10s | ", p.Name))

	cmd := &exec.Cmd{
		Args:   p.CmdArgs,
		Path:   p.CmdArgs[0],
		Stdin:  nil,
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
