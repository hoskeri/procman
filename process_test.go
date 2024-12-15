package procman

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestProcess(t *testing.T) {
	tw := bytes.NewBuffer([]byte{})
	wantOutput := "hello      E | stderr\nhello      O | stdout\n"

	p := &Process{
		Name: "hello",
		CmdArgs: []string{
			"/usr/bin/sh", "-c",
			">&2 echo stderr; echo stdout",
		},
	}

	err := p.Run(context.Background(), WithOutput(tw))
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if diff := cmp.Diff(string(tw.Bytes()), wantOutput); diff != "" {
		t.Fatalf("diff (-want, +got): %v", diff)
	}
}
