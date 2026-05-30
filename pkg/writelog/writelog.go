package writelog

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
)

type stream struct {
	mu   sync.Mutex
	sink *slog.Logger
	lvl  slog.Level
	buf  *bytes.Buffer
}

func (s *stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.buf.Write(p)
	if err != nil {
		return 0, err
	}

	for {
		got, err := s.buf.ReadBytes('\n')
		if err != nil {
			s.buf.Write(got)
			break
		}
		s.sink.LogAttrs(context.Background(), s.lvl, string(got))
	}
	return len(p), nil
}

// Close flushes any remaining buffered bytes that were not terminated by a
// newline (e.g. the last line of output from a process that exits without one).
func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.buf.Len() > 0 {
		s.sink.LogAttrs(context.Background(), s.lvl, s.buf.String())
		s.buf.Reset()
	}
	return nil
}

// Stream returns an io.WriteCloser that forwards each newline-delimited line
// of subprocess output to sink as a structured log record tagged with tag.
// The caller should Close() the writer after the subprocess exits to flush any
// final partial line.
func Stream(sink *slog.Logger, tag string, lvl slog.Level) io.WriteCloser {
	return &stream{
		buf:  bytes.NewBuffer(make([]byte, 0, 256)),
		sink: sink.WithGroup(tag).With(slog.Attr{Key: "tag", Value: slog.StringValue(tag)}),
		lvl:  lvl,
	}
}
