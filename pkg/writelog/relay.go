package writelog

import (
	"context"
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
// path (split on newlines and logged as text under childTag).  channelStream
// is the stream the socket belongs to (stdout/stderr); it is used as the
// fallback when a decoded frame does not carry its own stream.
//
// The gate is a single-byte check: if the first byte of a message equals
// frameVersion, it is assumed to be a binary frame; otherwise it's text.
// This avoids any frame-decode overhead on text messages.
//
// wg is optional; when non-nil, Add(1) is called before the loop and Done
// after it exits.
func DualRelay(recv *net.UnixConn, parentSink *slog.Logger, childTag string, channelStream StreamKind, wg *sync.WaitGroup) {
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		dualRelayLoop(recv, parentSink, childTag, channelStream)
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

func dualRelayLoop(recv *net.UnixConn, parentSink *slog.Logger, childTag string, channelStream StreamKind) {
	buf := make([]byte, MaxFrameSize+1024)
	var tb textBuf

	// Pre-allocate a logger for text lines (display tag only).  Text has no
	// frame, so its stream is known from the channel it was read on.
	textLogger := TaggedSink(parentSink, childTag).With(
		slog.String("stream", channelStream.String()),
	)

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
			emitRelayedFrame(parentSink, childTag, channelStream, frame)
		} else {
			// Text from a non-procman child.
			tb.feed(string(msg), func(line string) {
				textLogger.LogAttrs(context.Background(), slog.LevelInfo, line)
			})
		}
	}
}

// emitRelayedFrame re-emits a decoded frame into the parent sink.  The
// process tag (childTag) and the frame's component tag path become the display
// tag path, while frame.Groups become attr namespaces.  The frame's own Stream
// is preserved; when it is absent (an older/foreign sender) channelStream is
// used instead.  The stream is attached as a "stream" attribute so
// stream-aware rendering can use it later; the terminal renderer filters it.
func emitRelayedFrame(parentSink *slog.Logger, childTag string, channelStream StreamKind, frame Frame) {
	if frame.Message == "" {
		return
	}
	if frame.Stream == StreamUnset {
		frame.Stream = channelStream
	}

	tagCapable := TagCapable(parentSink)

	// Display tag path: process tag + the frame's component path.
	logger := TaggedSink(parentSink, childTag)
	if frame.Tag != "" {
		for _, t := range strings.Split(frame.Tag, "/") {
			if t != "" {
				logger = WithTag(logger, t)
			}
		}
	}
	// Attr namespaces (not display tags).
	for _, g := range frame.Groups {
		logger = logger.WithGroup(g)
	}

	// Reconstruct attrs from the encoded section (usually nil in practice).
	var attrs []slog.Attr
	if len(frame.Attrs) > 0 {
		attrs, _ = unmarshalAttrs(frame.Attrs)
	}
	// Plain (non-tag-capable) sinks have no prefix, so keep the component tag
	// visible as an attribute, matching the historical behavior.
	if !tagCapable && frame.Tag != "" {
		attrs = append(attrs, slog.String("tag", frame.Tag))
	}
	attrs = append(attrs, slog.String("stream", frame.Stream.String()))

	logger.LogAttrs(context.Background(), slog.Level(frame.Level), frame.Message, attrs...)
}

// --- Child-side autodetection ---

// ChildSinks probes the child's standard streams (fd 1 and fd 2) to
// determine whether they are connected to a parent formation's
// SOCK_SEQPACKET log sockets.  Each fd that is a SEQPACKET socket is made
// non-blocking and wrapped in a framer stamped with its stream; fds that are
// not sockets yield nil.
//
// A nested procman formation uses the stdout sink for its structured records
// and the stderr sink for its own error/default output.  A non-nested process
// (pipe, terminal, file) returns (nil, nil) and keeps its own sinks.
func ChildSinks() (stdout, stderr *slog.Logger) {
	stdout = childSink(1, StreamStdout)
	stderr = childSink(2, StreamStderr)
	return stdout, stderr
}

func childSink(fd int, stream StreamKind) *slog.Logger {
	typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || typ != syscall.SOCK_SEQPACKET {
		return nil
	}
	// Make the socket non-blocking so the framer can drop on EAGAIN instead
	// of blocking the child.  This is safe because the framer is the sole
	// writer to that fd once the sink is replaced.
	_ = syscall.SetNonblock(fd, true)
	return slog.New(NewFramer(fd, stream, slog.LevelDebug))
}
