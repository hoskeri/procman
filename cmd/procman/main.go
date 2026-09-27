package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/termhandler"
	"github.com/spf13/pflag"
)

type procFlags struct {
	Procfile  string
	Dotenv    string
	Formation string
	Output    string
	Columns   int
	Workdir   string
	Debug     bool
}

func (p *procFlags) AddFlags(fs *pflag.FlagSet) {
	fs.StringVarP(&p.Procfile, "procfile", "f", "Procfile", "path to Procfile")
	fs.StringVarP(&p.Workdir, "workdir", "w", "", "path to initial working dir, defaults to location of Procfile")
	fs.StringVarP(&p.Dotenv, "env", "e", "", "path to dotenv style env file")
	fs.StringVar(&p.Formation, "formation", "", "optional map of process type=replica-count")
	fs.StringVar(&p.Output, "output", "auto", "output mode: auto,term")
	fs.IntVar(&p.Columns, "columns", 0, "truncate log lines to this many characters (0 = terminal width, <0 = off)")
	fs.BoolVar(&p.Debug, "debug", false, "enable debug logging")
}

func main() {
	p := &procFlags{}
	p.AddFlags(pflag.CommandLine)
	pflag.CommandLine.SortFlags = true
	pflag.Parse()

	ll := slog.LevelInfo
	if p.Debug {
		ll = slog.LevelDebug
	}

	// The signal context is the formation's lifetime; the termhandler uses it
	// to restore terminal state and stop relay goroutines on shutdown.
	ctx := context.Background()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	// --output auto: plain text when stdout is not a terminal; the colored,
	// prefixed term renderer otherwise. --output term forces the term renderer
	// even when piped (and therefore color). Unknown values behave like auto.
	plain := p.Output != "term" && !termhandler.IsTerminal(os.Stdout)

	logs := termhandler.New(ctx, os.Stdin, os.Stdout, os.Stderr, &termhandler.Options{
		Level:   ll,
		Columns: p.Columns,
		Colors:  p.Output == "term",
		Plain:   plain,
	})
	defer logs.Close()

	// A nested procman formation frames its own records up the stderr socket;
	// in root mode the process-wide default logger stays a plain text handler
	// on stderr (the term renderer only prefixes tagged process output).
	if logs.Nested() {
		slog.SetDefault(logs.ErrLogger())
	} else {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{AddSource: false, Level: ll})))
	}

	if p.Workdir == "" {
		p.Workdir = filepath.Dir(p.Procfile)
	}

	fm := process.Formation{
		Workdir: p.Workdir,
		Logs:    logs,
	}

	if err := fm.LoadFile(p.Procfile); err != nil {
		slog.Error("fm.LoadFile", "err", err)
		logs.Close()
		os.Exit(1)
	}

	if err := fm.Run(ctx); err != nil {
		// A process that exited on its own determines the formation's status:
		// its exit code is the exit code of procman itself. A non-ExitError
		// (e.g. an executable that failed to launch) is a setup failure -> 1.
		var pe *process.ExitError
		if errors.As(err, &pe) {
			lv := slog.LevelError
			if pe.Code == 0 {
				// Clean self-exit: the formation came down on its own, not by
				// user request — informational, not an error.
				lv = slog.LevelWarn
			}
			slog.Log(ctx, lv, "fm.Run", "err", err)
			logs.Close()
			os.Exit(pe.Code)
		}
		slog.Error("fm.Run", "err", err)
		logs.Close()
		os.Exit(1)
	}
}
