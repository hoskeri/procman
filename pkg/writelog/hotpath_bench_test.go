package writelog_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hoskeri/procman/pkg/termhandler"
	"github.com/hoskeri/procman/pkg/writelog"
)

// cheapSink is a slog.Handler that does no work (no I/O, no attrs) so the
// writelog hot path (Write -> split -> push -> drain -> LogAttrs) is measured
// in isolation from sink cost.
type cheapSink struct{}

func (cheapSink) Enabled(context.Context, slog.Level) bool  { return true }
func (cheapSink) Handle(context.Context, slog.Record) error { return nil }
func (cheapSink) WithAttrs([]slog.Attr) slog.Handler        { return cheapSink{} }
func (cheapSink) WithGroup(string) slog.Handler             { return cheapSink{} }

// termSink is the real production sink: a color-forced termhandler writing to
// /dev/null. This measures the exact path used when --output auto|term.
func newTermSink() slog.Handler {
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	return termhandler.New(devNull, &termhandler.Options{Colors: true, Columns: 0})
}

var line = []byte("2026-09-13 12:00:00 web | info: processing batch=42 item=a-long-payload-here-987\n")

// BenchmarkWritelogCheapSink measures writelog.Write/drain in isolation (no
// sink cost), isolating the parser + queue machinery allocation churn.
func BenchmarkWritelogCheapSink(b *testing.B) {
	s := writelog.Stream(slog.New(cheapSink{}), "web", slog.LevelInfo, writelog.StreamConfig{})
	defer s.Close()
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for i := 0; i < b.N; i++ {
		s.Write(line)
	}
}

// BenchmarkTermhandlerHandle measures the termhandler.Handle cost alone (prefix
// concat, truncation, mutex, single Write syscall) per line.
func BenchmarkTermhandlerHandle(b *testing.B) {
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	h := termhandler.New(devNull, &termhandler.Options{Colors: true, Columns: 0})
	hg := h.WithGroup("web").(slog.Handler)
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, string(line[:len(line)-1]), 0)
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for i := 0; i < b.N; i++ {
		hg.Handle(context.Background(), rec)
	}
}

// BenchmarkTermPathModern is the modern production path end-to-end: writelog
// -> color termhandler -> /dev/null. This is what trebuchet exercises.
func BenchmarkTermPathModern(b *testing.B) {
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	h := termhandler.New(devNull, &termhandler.Options{Colors: true, Columns: 0})
	lg := slog.New(h)
	s := writelog.Stream(lg, "web", slog.LevelInfo, writelog.StreamConfig{})
	defer s.Close()
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for i := 0; i < b.N; i++ {
		s.Write(line)
	}
}

// BenchmarkContention drives many streams (one per "process") sharing a single
// termhandler, each writing concurrently from its own goroutine. This exposes
// cross-process lock contention on the shared termhandler mutex and the per
// stream drain goroutine scheduling.
func BenchmarkContention(b *testing.B) {
	const nproc = 16
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	h := termhandler.New(devNull, &termhandler.Options{Colors: true, Columns: 0})
	lg := slog.New(h)

	streams := make([]struct {
		s  interface{ Write([]byte) (int, error) }
		wg *sync.WaitGroup
	}, nproc)
	start := make(chan struct{})
	wgs := make([]*sync.WaitGroup, nproc)
	for i := 0; i < nproc; i++ {
		w := writelog.Stream(lg, fmt.Sprintf("p%d", i), slog.LevelInfo, writelog.StreamConfig{})
		defer w.Close()
		wgs[i] = &sync.WaitGroup{}
		wgs[i].Add(1)
		streams[i].s = w
		streams[i].wg = wgs[i]
		go func(s interface{ Write([]byte) (int, error) }, wg *sync.WaitGroup) {
			defer wg.Done()
			<-start
			for j := 0; j < b.N; j++ {
				s.Write(line)
			}
		}(streams[i].s, wgs[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	close(start)
	for _, wg := range wgs {
		wg.Wait()
	}
	b.SetBytes(int64(len(line)))
}
