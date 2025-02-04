package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/termhandler"
)

type procFlags struct {
	Procfile  string
	Dotenv    string
	Formation string
	Output    string
	Workdir   string
	Debug     bool
}

func (p *procFlags) AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&p.Procfile, "procfile", "Procfile", "path to Procfile")
	fs.StringVar(&p.Workdir, "workdir", "", "path to initial working dir, defaults to location of Procfile")
	fs.StringVar(&p.Dotenv, "env", "", "path to dotenv style env file")
	fs.StringVar(&p.Formation, "formation", "", "optional map of process type=replica-count")
	fs.StringVar(&p.Output, "output", "auto", "output mode: auto,term")
	fs.BoolVar(&p.Debug, "debug", false, "enable debug logging")
}

func main() {
	p := &procFlags{}
	p.AddFlags(flag.CommandLine)
	flag.Parse()

	ll := slog.LevelInfo
	if p.Debug {
		ll = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{AddSource: false, Level: ll})))

	proclogger := slog.New(termhandler.New(os.Stdout, &termhandler.Options{
		Level: slog.LevelDebug,
	}))

	if p.Workdir == "" {
		w := filepath.Dir(p.Procfile)
		p.Workdir = w
	}

	fm := process.Formation{
		Workdir: p.Workdir,
		Sink:    proclogger,
	}

	if err := fm.LoadFile(p.Procfile); err != nil {
		slog.Debug("fm.LoadFile", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	if err := fm.Run(ctx); err != nil {
		slog.Debug("fm.Run", "err", err)
		os.Exit(1)
	}
}
