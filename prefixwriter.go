package procman

import (
	"bytes"
	"context"
	"io"
	"os"
)

type Logger struct {
	o     io.Writer
	queue <-chan []byte
}

func NewLogger() *Logger {
	return &Logger{
		o:     os.Stdout,
		queue: make(chan []byte, 128),
	}
}

func (l *Logger) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case line := <-l.queue:
			if _, err := l.o.Write(line); err != nil {
				return err
			}
		}
	}
}

type prefixWriter struct {
	writer  io.Writer
	prefix  []byte
	newLine bool
}

func newPrefixWriter(writer io.Writer, prefix string) *prefixWriter {
	return &prefixWriter{
		writer:  writer,
		prefix:  []byte(prefix),
		newLine: true,
	}
}

func (pw *prefixWriter) Write(p []byte) (n int, err error) {
	var buf bytes.Buffer

	for _, b := range p {
		if pw.newLine {
			buf.Write(pw.prefix)
			pw.newLine = false
		}

		buf.WriteByte(b)

		if b == '\n' {
			pw.newLine = true
		}
	}

	_, err = pw.writer.Write(buf.Bytes())
	return len(p), err
}
