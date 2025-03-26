package writelog

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
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
	s := Stream(lg, "op0")
	s2 := Stream(lg, "op1")
	s.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrst" + "\n" + "a00002ghijklmno")))
	s2.Write([]byte(fmt.Sprintf("a00001ghijklmnopqrst" + "\n" + "a00002ghijklmno")))
	s.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))
	s2.Write([]byte(fmt.Sprintf("pqrst" + "\n" + "a00003ghijklmnopqrst" + "\n")))

	t.Logf("\n%s\n", testOutput.String())

	wantOutput := `{"level":"INFO","msg":"a00001ghijklmnopqrst\n","op0":{"tag":"op0"}}
{"level":"INFO","msg":"a00001ghijklmnopqrst\n","op1":{"tag":"op1"}}
{"level":"INFO","msg":"a00002ghijklmnopqrst\n","op0":{"tag":"op0"}}
{"level":"INFO","msg":"a00003ghijklmnopqrst\n","op0":{"tag":"op0"}}
{"level":"INFO","msg":"a00002ghijklmnopqrst\n","op1":{"tag":"op1"}}
{"level":"INFO","msg":"a00003ghijklmnopqrst\n","op1":{"tag":"op1"}}
`

	if diff := cmp.Diff(testOutput.String(), wantOutput); diff != "" {
		t.Logf("unexpected output (-want, +got): \n%s\n", diff)
	}
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
