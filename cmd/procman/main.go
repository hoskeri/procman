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
	fs.IntVar(&p.Columns, "columns", 0, "truncate log lines to this many characters (0 = off)")
	fs.BoolVar(&p.Debug, "debug", false, "enable debug logging")
}

// proclogger builds the process output sink according to --output:
//
//	auto - termhandler (colored, prefixed) when stdout is a terminal, a plain
//	       text handler otherwise (e.g. when piped);
//	term - always the termhandler, forcing color even when piped.
//
// Unknown values behave like auto. In either termhandler case --columns N
// truncates each log line to N bytes.
func proclogger(output string, columns int) *slog.Logger {
	forceColor := output == "term"
	if forceColor || termhandler.IsTerminal(os.Stdout) {
		return slog.New(termhandler.New(os.Stdout, &termhandler.Options{
			Level:   slog.LevelDebug,
			Columns: columns,
			Colors:  forceColor,
		}))
	}
	// Piped output: no color, no per-process prefixes.
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
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
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{AddSource: false, Level: ll})))
	plogger := proclogger(p.Output, p.Columns)

	if p.Workdir == "" {
		w := filepath.Dir(p.Procfile)
		p.Workdir = w
	}

	fm := process.Formation{
		Workdir: p.Workdir,
		Sink:    plogger,
	}

	if err := fm.LoadFile(p.Procfile); err != nil {
		slog.Error("fm.LoadFile", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

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
			os.Exit(pe.Code)
		}
		slog.Error("fm.Run", "err", err)
		os.Exit(1)
	}
}
