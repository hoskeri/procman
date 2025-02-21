package process

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestProcess(t *testing.T) {
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	jh := slog.NewJSONHandler(devNull, &slog.HandlerOptions{})
	lg := slog.New(jh)
	p := &Process{
		Tag: "hello",
		CmdArgs: []string{
			"/usr/bin/sh", "-c",
			">&2 echo stderr; echo stdout",
		},
	}

	err := p.run(context.Background(), WithLogger(lg))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestFormation(t *testing.T) {
	t.Skip()
	testCases := []struct {
		data string
		want []*Process
		err  error
	}{
		{
			data: "web: ./webserver \"hello world\"\ndb: ./mysql 'a b c'",
			want: []*Process{
				{Tag: "web", CmdArgs: []string{"./webserver", "hello world"}},
				{Tag: "db", CmdArgs: []string{"./mysql", "a b c"}},
			},
		},
	}

	for _, tc := range testCases {
		frm := &Formation{}

		err := frm.Load(strings.NewReader(tc.data))
		if err != nil {
			t.Fatalf("%v", err)
		}
		if diff := cmp.Diff(frm.Processes, tc.want); diff != "" {
			t.Fatalf("unexpected diff, (-want, +got) %s", diff)
		}
	}
}
