package writelog

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
)

// SetupRecvConn wraps a raw SOCK_SEQPACKET socket fd into a *net.UnixConn
// suitable for frame reads with deadline control.
func SetupRecvConn(fd int) *net.UnixConn {
	f := os.NewFile(uintptr(fd), "log-recv")
	conn, err := net.FileConn(f)
	f.Close() // net.FileConn dups the fd; close our reference.
	if err != nil {
		return nil
	}
	return conn.(*net.UnixConn)
}

// Relay spawns a reader goroutine that reads frames from recv (the recv
// side of a SOCK_SEQPACKET socketpair) and re-emits them into parentSink
// with group nesting: parentSink.WithGroup(childTag).WithGroup(frame groups...).
//
// The relay can be stopped gracefully by calling recv.SetReadDeadline with
// a past time (e.g. time.Now()); this unblocks the reader and the goroutine
// exits.  The recv *net.UnixConn should be closed with Close() after the
// relay has exited.
//
// wg is optional; when non-nil, Add(1) is called before the loop and Done
// after it exits, so the caller can wait for all relays to finish.
func Relay(recv *net.UnixConn, parentSink *slog.Logger, childTag string, wg *sync.WaitGroup) {
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		relayLoop(recv, parentSink, childTag)
	}()
}

// relayLoop is the inner read-decode-emit loop. It runs until recv returns
// an error (or EOF when all write ends close).
func relayLoop(recv *net.UnixConn, parentSink *slog.Logger, childTag string) {
	buf := make([]byte, MaxFrameSize+1024)
	for {
		n, err := recv.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			return
		}

		var frame Frame
		if err := frame.Unmarshal(buf[:n]); err != nil {
			// Malformed frame: skip and continue.
			continue
		}

		emitRelayedFrame(parentSink, childTag, frame)
	}
}

// emitRelayedFrame re-emits a decoded frame into the parent sink with
// proper group nesting.
func emitRelayedFrame(parentSink *slog.Logger, childTag string, frame Frame) {
	if frame.Message == "" {
		return
	}

	// Build the group chain: childTag + frame.Groups (the child's own
	// group path from its framer handler).
	logger := parentSink.WithGroup(childTag)
	for _, g := range frame.Groups {
		logger = logger.WithGroup(g)
	}

	// Reconstruct attrs.  JSON types map to Go types that slog.Any
	// can represent (string, bool, float64, map[string]any, []any).
	// Use slog.Any for flexibility.
	var attrs []slog.Attr
	if frame.Attrs != nil {
		attrs = make([]slog.Attr, 0, len(frame.Attrs))
		for k, v := range frame.Attrs {
			attrs = append(attrs, slog.Any(k, v))
		}
	}
	if frame.Tag != "" {
		attrs = append(attrs, slog.String("tag", frame.Tag))
	}

	logger.LogAttrs(context.Background(), slog.Level(frame.Level), frame.Message, attrs...)
}

// NewChildLogger creates a *slog.Logger that encodes records as frames and
// writes them to the fd identified by the PROCMAN_LOG_FD environment
// variable. Returns nil when the env var is not set (root mode).
func NewChildLogger() *slog.Logger {
	fdStr := os.Getenv("PROCMAN_LOG_FD")
	if fdStr == "" {
		return nil
	}
	var fd int
	if _, err := fmt.Sscanf(fdStr, "%d", &fd); err != nil || fd < 0 {
		return nil
	}
	f := SetupSendSocket(fd)
	return slog.New(NewFramer(f, slog.LevelDebug))
}
