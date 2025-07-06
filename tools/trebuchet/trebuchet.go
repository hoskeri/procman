package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{AddSource: false})))

	maxBlockDuration := flag.Duration("max-block", 15000*time.Microsecond, "max duration output is allowed to be blocked")
	countMessages := flag.Uint("count", 1000, "message count")
	exitCode := flag.Int("exit-code", 0, "exit with this code once done")
	flag.Parse()

	src := rand.New(rand.NewSource(0))
	var blocked time.Duration

	for s := range *countMessages {
		time.Sleep(10 * time.Millisecond)

		begin := time.Now()
		ll := src.Intn(12) + rand.Intn(128) + rand.Intn(128)
		buf := make([]byte, ll, ll)
		_, _ = src.Read(buf)
		slog.Info("trebuchet", "blocked", blocked.String(), "s", s, "buf", base64.URLEncoding.EncodeToString(buf))
		blocked = time.Now().Sub(begin)
		if blocked.Microseconds() > maxBlockDuration.Microseconds() {
			fmt.Printf("blocked for %s\n", blocked.String())
			os.Exit(1)
		}
	}
	os.Exit(*exitCode)
}
