package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/hoskeri/procman"
)

func main() {
	log.SetFlags(log.Lmsgprefix | log.Lshortfile | log.LstdFlags)
	fm, err := procman.New("Procfile")
	if err != nil {
		log.Fatalf("%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx, _ = signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	if err := fm.Run(ctx); err != nil {
		log.Fatalf("error: %v", err)
	}
}
