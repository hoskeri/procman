package procman

import (
	"bytes"
	"io"
)

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
