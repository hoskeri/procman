package procman

import (
	"bytes"
	"fmt"
	"log/slog"
	"testing"
)

func TestStreams(t *testing.T) {
	testOutput := &bytes.Buffer{}
	jh := slog.NewJSONHandler(testOutput, &slog.HandlerOptions{})
	lg := slog.New(jh)
	s := Stream(lg, "web", 1, 1)
	s.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrst" + "\n" + "a00002ghijklmno")))
	s.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
	t.Logf("\n%s", testOutput.String())
}
