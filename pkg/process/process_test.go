package process

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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

	err := p.run(context.Background(), withLogger(lg))
	if err != nil {
		t.Fatalf("expected nil error for clean exit, got: %v", err)
	}
}

func TestFormation(t *testing.T) {
	testCases := []struct {
		name string
		data string
		want []*Process
		err  error
	}{
		{
			name: "basic quoted args",
			data: "web: ./webserver \"hello world\"\ndb: ./mysql 'a b c'",
			want: []*Process{
				{Tag: "web", CmdArgs: []string{"./webserver", "hello world"}},
				{Tag: "db", CmdArgs: []string{"./mysql", "a b c"}},
			},
		},
		{
			name: "comment and blank lines are skipped",
			data: "# this is a comment\n\nweb: ./server",
			want: []*Process{
				{Tag: "web", CmdArgs: []string{"./server"}},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			frm := &Formation{}
			err := frm.Load(io.NopCloser(strings.NewReader(tc.data)))
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if diff := cmp.Diff(tc.want, frm.Processes, cmpopts.IgnoreFields(Process{}, "Workdir")); diff != "" {
				t.Fatalf("unexpected processes (-want, +got):\n%s", diff)
			}
		})
	}
}
