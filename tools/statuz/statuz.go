// Command statuz is a status-reporting test fixture: a procman child that
// publishes component status (through status.TaggedReporter) on a schedule and
// then keeps running, so a parent formation's log stream and status registry
// can be observed end to end.
//
// It stands in for a runkube-style component: a process that is alive, has
// sub-components with distinct tags, and moves between healthy, degraded,
// unhealthy and failed.  With -exit-when-failed it returns non-zero after
// reporting a fatal status, which lets an integration test assert that a
// reported status never masks an exit.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hoskeri/procman/pkg/status"
)

func main() {
	tag := flag.String("tag", "statuz", "component tag to report under")
	interval := flag.Duration("interval", 100*time.Millisecond, "time between reports")
	sequence := flag.String("sequence", "healthy,degraded,unhealthy,healthy", "comma-separated states to report in order (the last one repeats)")
	detail := flag.String("detail", "fixture", "detail text for each report")
	exitWhenFailed := flag.Bool("exit-when-failed", false, "exit non-zero once a failed state is reported")
	flag.Parse()

	states := strings.Split(*sequence, ",")
	if len(states) == 0 || states[0] == "" {
		fmt.Fprintln(os.Stderr, "statuz: empty sequence")
		os.Exit(2)
	}

	rep := status.TaggedReporter(*tag)

	var mu sync.Mutex
	step := 0
	for {
		name := states[step%len(states)]
		step++
		mu.Lock()
		switch strings.TrimSpace(name) {
		case "healthy":
			rep.OK("%s: serving", *detail)
		case "starting":
			rep.Starting("%s: waiting for dependencies", *detail)
		case "degraded":
			rep.Degraded(nil, "%s: slow", *detail)
		case "unhealthy":
			rep.Unhealthy(nil, "%s: dependency unreachable", *detail)
		case "failed":
			rep.Failed(nil, "%s: unusable", *detail)
		default:
			rep.OK("%s: %s", *detail, name)
		}
		failed := strings.TrimSpace(name) == "failed"
		mu.Unlock()

		if failed && *exitWhenFailed {
			fmt.Fprintf(os.Stderr, "statuz: exiting after a failed report\n")
			os.Exit(1)
		}
		time.Sleep(*interval)
	}
}
