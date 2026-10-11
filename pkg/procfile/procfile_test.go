package procfile

import (
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestValidTag(t *testing.T) {
	tests := []struct {
		tag string
		ok  bool
	}{
		{"web", true},
		{"web-1", true},
		{"node", true},
		{"node-1", true},
		{"a", true},
		{"0", true},
		{"", false},
		{"WEB", false},
		{"Web_1", false},
		{"web.server", false},
		{"web 1", false},
		{"-web", false},
		{"web-", false},
	}
	for _, tt := range tests {
		if got := ValidTag(tt.tag); got != tt.ok {
			t.Errorf("ValidTag(%q) = %v, want %v", tt.tag, got, tt.ok)
		}
	}
}

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
			name:    "malformed line reports its number",
			data:    "web: ./server\ndb ./mysql",
			wantErr: true,
			errMsg:  "invalid line 2",
		},
		{
			name:    "invalid tag rejected",
			data:    "web: ./server\nWeb: ./other",
			wantErr: true,
			errMsg:  `line 2: process tag "Web"`,
		},
		{
			name:    "empty tag rejected",
			data:    ": ./server",
			wantErr: true,
			errMsg:  "process tag",
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
