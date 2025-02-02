package procman

import (
	"bytes"
	"io"
	"log/slog"
	"sync"
)

type stream struct {
	mu   sync.Mutex
	sink *slog.Logger
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
		s.sink.LogAttrs(nil, slog.LevelInfo, string(got))
	}
	return len(p), nil
}

func Stream(sink *slog.Logger, tag string) io.Writer {
	return &stream{
		buf:  bytes.NewBuffer(make([]byte, 0, 256)),
		sink: sink.WithGroup(tag),
	}
}
