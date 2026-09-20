package writelog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
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

// --- Dual relay: frame or text ---

// DualRelay reads messages from recv and dispatches each to either the frame
// path (parsed and re-emitted into parentSink with group nesting) or the text
// path (split on newlines and logged as text under childTag).
//
// The gate is a single-byte check: if the first byte of a message equals
// frameVersion, it is assumed to be a binary frame; otherwise it's text.
// This avoids any JSON parse overhead on text messages.
//
// wg is optional; when non-nil, Add(1) is called before the loop and Done
// after it exits.
func DualRelay(recv *net.UnixConn, parentSink *slog.Logger, childTag string, wg *sync.WaitGroup) {
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		dualRelayLoop(recv, parentSink, childTag)
	}()
}

// textBuf accumulates partial (unterminated) lines across successive
// SEQPACKET messages from a non-procman child writing text to stdout.
type textBuf struct {
	partial string
}

// feed processes one chunk of text, emitting complete newline-terminated
// lines via emit.  Any incomplete trailing text is stored in the buffer
// and prepended to the next feed call.
func (tb *textBuf) feed(data string, emit func(line string)) {
	s := tb.partial + data
	tb.partial = ""
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			tb.partial = s
			return
		}
		emit(s[:i+1])
		s = s[i+1:]
	}
}

// flush emits any remaining partial line (data without trailing newline) and
// resets the buffer.  Must be called after the child exits to capture the
// final output.
func (tb *textBuf) flush(emit func(line string)) {
	if tb.partial != "" {
		emit(tb.partial)
		tb.partial = ""
	}
}

func dualRelayLoop(recv *net.UnixConn, parentSink *slog.Logger, childTag string) {
	buf := make([]byte, MaxFrameSize+1024)
	var tb textBuf

	// Pre-allocate a logger for text lines (childTag group only).
	textLogger := parentSink.WithGroup(childTag).With(slog.String("tag", childTag))

	for {
		n, err := recv.Read(buf)
		if err != nil {
			// Flush any remaining partial text line before exiting.
			tb.flush(func(line string) {
				textLogger.LogAttrs(context.Background(), slog.LevelInfo, line)
			})
			return
		}
		if n == 0 {
			tb.flush(func(line string) {
				textLogger.LogAttrs(context.Background(), slog.LevelInfo, line)
			})
			return
		}

		msg := buf[:n]

		// Fast gate: check if this is a binary frame.
		if IsFramePrefix(msg) {
			var frame Frame
			if err := frame.UnmarshalBinary(msg); err != nil {
				// Malformed frame: treat as text.
				tb.feed(string(msg), func(line string) {
					textLogger.LogAttrs(context.Background(), slog.LevelInfo, line)
				})
				continue
			}
			emitRelayedFrame(parentSink, childTag, frame)
		} else {
			// Text from a non-procman child.
			tb.feed(string(msg), func(line string) {
				textLogger.LogAttrs(context.Background(), slog.LevelInfo, line)
			})
		}
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

	// Reconstruct attrs from JSON bytes (usually nil in practice).
	var attrs []slog.Attr
	if len(frame.AttrsJSON) > 0 {
		var m map[string]any
		d := json.NewDecoder(bytes.NewReader(frame.AttrsJSON))
		d.UseNumber()
		if d.Decode(&m) == nil {
			attrs = make([]slog.Attr, 0, len(m))
			for k, v := range m {
				attrs = append(attrs, slog.Any(k, v))
			}
		}
	}
	if frame.Tag != "" {
		attrs = append(attrs, slog.String("tag", frame.Tag))
	}

	logger.LogAttrs(context.Background(), slog.Level(frame.Level), frame.Message, attrs...)
}

// --- Child-side autodetection ---

// NewChildFramer probes the child's stdout (fd 1) to determine whether it is
// connected to a parent formation's SOCK_SEQPACKET log socket.  If so, it
// sets O_NONBLOCK on stdout and returns a *slog.Logger backed by a
// FramerHandler writing to fd 1.  If stdout is not a SEQPACKET socket (pipe,
// terminal, file) it returns nil — the caller should use its own sink (root
// mode).
func NewChildFramer() *slog.Logger {
	typ, err := syscall.GetsockoptInt(1, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || typ != syscall.SOCK_SEQPACKET {
		return nil
	}
	// Make stdout non-blocking so the framer can drop on EAGAIN instead of
	// blocking the child process.  This is safe because the framer is the
	// sole writer to stdout once the formation sink is replaced.
	_ = syscall.SetNonblock(1, true)
	return slog.New(NewFramer(1, slog.LevelDebug))
}
