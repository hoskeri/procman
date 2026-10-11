package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hoskeri/procman/pkg/loglevels"
	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/status"
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
	Name      string
	Debug     bool
	// LogLevel is the parsed --log-level policy; nil until AddFlags runs.
	LogLevel *loglevels.Value
}

func (p *procFlags) AddFlags(fs *pflag.FlagSet) {
	fs.StringVarP(&p.Procfile, "procfile", "f", "Procfile", "path to Procfile")
	fs.StringVarP(&p.Workdir, "workdir", "w", "", "path to initial working dir, defaults to location of Procfile")
	fs.StringVarP(&p.Dotenv, "env", "e", "", "path to dotenv style env file")
	fs.StringVar(&p.Formation, "formation", "", "optional map of process type=replica-count")
	fs.StringVar(&p.Output, "output", "auto", "output mode: auto,term")
	fs.IntVar(&p.Columns, "columns", 0, "truncate log lines to this many characters (0 = terminal width, <0 = no truncation)")
	fs.StringVar(&p.Name, "name", "procman", "application name used to tag the formation's own log lines")
	fs.BoolVar(&p.Debug, "debug", false, "enable debug logging")
	p.LogLevel = loglevels.NewValue(slog.LevelInfo)
	fs.Var(p.LogLevel, "log-level", "log level policy: a level, or level plus tag=level overrides (e.g. info,api=debug)")
}

func main() {
	p := &procFlags{}
	p.AddFlags(pflag.CommandLine)
	pflag.CommandLine.SortFlags = true
	pflag.Parse()

	if p.Debug && !p.LogLevel.Changed() {
		// --debug is a convenience for --log-level debug; an explicit
		// --log-level (even one carrying only tag overrides) wins.
		p.LogLevel.SetDefault(slog.LevelDebug)
	}
	ll := p.LogLevel.Levels().Default

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
		Name:      p.Name,
		Workdir:   p.Workdir,
		Logs:      logs,
		LogLevels: p.LogLevel.Levels(),
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
		if pe, ok := errors.AsType[*process.ExitError](err); ok {
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
		// A fatal formation status: the formation's own Status policy decided
		// no component was worth waiting on any longer.
		if se, ok := errors.AsType[*status.Error](err); ok && se.Fatal() {
			slog.Error("fm.Run", "err", err, "status", se.State.String())
			logs.Close()
			os.Exit(1)
		}
		slog.Error("fm.Run", "err", err)
		logs.Close()
		os.Exit(1)
	}
}
