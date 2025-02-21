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

func main() {
	var procfile string
	var dotenv string
	flag.StringVar(&procfile, "procfile", "Procfile", "path to Procfile")
	flag.StringVar(&dotenv, "env", "", "path to dotenv style env file")
	flag.Parse()

	proclogger := slog.New(termhandler.New(os.Stdout, &termhandler.Options{
		Level: slog.LevelInfo,
	}))

	fm := process.Formation{
		WorkDir: filepath.Dir(procfile),
		Sink:    proclogger,
	}

	if err := fm.LoadFile(procfile); err != nil {
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
