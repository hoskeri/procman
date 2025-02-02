package procman

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
)

func TestStreams(t *testing.T) {
	testOutput := &bytes.Buffer{}
	jh := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{})
	lg := slog.New(jh)
	s := Stream(lg, "web")
	s.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrst" + "\n" + "a00002ghijklmno")))
	s.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
	t.Logf("\n%s", testOutput.String())
}

func BenchmarkStreams(b *testing.B) {
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	jh := slog.NewJSONHandler(devNull, &slog.HandlerOptions{})
	lg := slog.New(jh)
	s := Stream(lg, "web")
	s2 := Stream(lg, "web2")
	for range b.N {
		s.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrstaaaaaaasdsdffdsdsdsdsdsdsd" + "\n" + "a00002ghijklmno")))
		s.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
		s2.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrst" + "\n" + "a00002ghijklmno")))
		s2.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
	}
}
