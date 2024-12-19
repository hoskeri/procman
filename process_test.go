package procman

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestProcess(t *testing.T) {
	tw := bytes.NewBuffer([]byte{})
	wantOutput := "hello      | stderr\nhello      | stdout\n"

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
		t.Fatalf("diff (+got, -want): %v", diff)
	}
}

func TestFormation(t *testing.T) {
	testCases := []struct {
		data string
		want []Process
		err  error
	}{
		{
			data: "web: ./webserver \"hello world\"\ndb: ./mysql 'a b c'",
			want: []Process{
				{Name: "web", CmdArgs: []string{"./webserver", "hello world"}},
				{Name: "db", CmdArgs: []string{"./mysql", "a b c"}},
			},
		},
	}

	for _, tc := range testCases {
		frm := &Formation{}

		pf, err := frm.Load(strings.NewReader(tc.data))
		if err != nil {
			t.Fatalf("%v", err)
		}
		if diff := cmp.Diff(pf, tc.want); diff != "" {
			t.Fatalf("unexpected diff, (-want, +got) %s", diff)
		}
	}
}
