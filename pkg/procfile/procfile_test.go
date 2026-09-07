package procfile

import (
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParse(t *testing.T) {
	testCases := []struct {
		name    string
		data    string
		want    []Record
		wantErr bool
		errMsg  string // required substring of the error, when wantErr
	}{
		{
			name: "basic quoted args",
			data: "web: ./webserver \"hello world\"\ndb: ./mysql 'a b c'",
			want: []Record{
				{Tag: "web", CmdArgs: []string{"./webserver", "hello world"}},
				{Tag: "db", CmdArgs: []string{"./mysql", "a b c"}},
			},
		},
		{
			name: "comment and blank lines are skipped",
			data: "# this is a comment\n\nweb: ./server",
			want: []Record{
				{Tag: "web", CmdArgs: []string{"./server"}},
			},
		},
		{
			name: "malformed line reports its number",
			data: "web: ./server\ndb ./mysql",
			wantErr: true,
			errMsg:  "invalid line 2",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(io.NopCloser(strings.NewReader(tc.data)))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got records: %v", got)
				}
				if !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("expected error containing %q, got: %v", tc.errMsg, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("unexpected records (-want, +got):\n%s", diff)
			}
		})
	}
}