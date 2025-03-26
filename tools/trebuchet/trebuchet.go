package main

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"time"
)

func main() {
	h := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: false,
	}))
	slog.SetDefault(h)

	src := rand.New(rand.NewSource(0))
	var blocked time.Duration

	for {
		begin := time.Now()
		ll := src.Intn(12) + rand.Intn(128) + rand.Intn(128)
		buf := make([]byte, ll, ll)
		_, _ = src.Read(buf)
		slog.Info("trebuchet", "blocked", blocked.String(), "buf", base64.URLEncoding.EncodeToString(buf))
		time.Sleep(10 * time.Millisecond)
		blocked = time.Now().Sub(begin)
		if blocked.Microseconds() > 15000 {
			fmt.Printf("blocked for %s\n", blocked.String())
			os.Exit(1)
		}
	}
}
