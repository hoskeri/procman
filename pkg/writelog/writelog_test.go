package writelog

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestStreams(t *testing.T) {
	testOutput := &bytes.Buffer{}
	jh := slog.NewJSONHandler(testOutput, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, s slog.Attr) slog.Attr {
			if s.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return s
		},
	})
	lg := slog.New(jh)

	s := Stream(lg, "op0", slog.LevelInfo)
	// Two writes that together form two complete lines and one partial line.
	s.Write([]byte("line1\nline2\npartial"))
	// Close should flush the partial line.
	s.Close()

	t.Logf("\n%s\n", testOutput.String())

	got := testOutput.String()
	for _, want := range []string{
		`"msg":"line1\n"`,
		`"msg":"line2\n"`,
		`"msg":"partial"`,
		`"op0":{"tag":"op0"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

func TestStreamLevelRespected(t *testing.T) {
	testOutput := &bytes.Buffer{}
	jh := slog.NewJSONHandler(testOutput, &slog.HandlerOptions{
		Level: slog.LevelWarn,
		ReplaceAttr: func(_ []string, s slog.Attr) slog.Attr {
			if s.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return s
		},
	})
	lg := slog.New(jh)

	// Stream at INFO level — handler is set to WARN, so nothing should appear.
	s := Stream(lg, "proc", slog.LevelInfo)
	s.Write([]byte("should not appear\n"))
	s.Close()

	if testOutput.Len() != 0 {
		t.Errorf("expected no output at INFO level with WARN handler, got: %s", testOutput.String())
	}

	// Stream at WARN level — should appear.
	testOutput.Reset()
	s2 := Stream(lg, "proc", slog.LevelWarn)
	s2.Write([]byte("should appear\n"))
	s2.Close()

	if !strings.Contains(testOutput.String(), "should appear") {
		t.Errorf("expected output at WARN level, got: %s", testOutput.String())
	}
}

func BenchmarkStreams(b *testing.B) {
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	jh := slog.NewJSONHandler(devNull, &slog.HandlerOptions{})
	lg := slog.New(jh)
	s := Stream(lg, "web", slog.LevelInfo)
	s2 := Stream(lg, "web2", slog.LevelInfo)
	for range b.N {
		s.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrstaaaaaaasdsdffdsdsdsdsdsdsd" + "\n" + "a00002ghijklmno")))
		s.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
		s2.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrst" + "\n" + "a00002ghijklmno")))
		s2.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
	}
}
