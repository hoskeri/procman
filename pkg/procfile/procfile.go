package procfile

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/mattn/go-shellwords"
)

// Record is a single parsed Procfile entry: a process type Tag and the
// command line split into arguments.
type Record struct {
	Tag     string
	CmdArgs []string
}

// Parse parses Procfile data from src. Blank lines and lines starting with
// '#' are skipped. Every other line is split at the first ':' into a Tag and
// a Command; the command is parsed into arguments with shellwords. The
// caller is responsible for attaching process-level state (e.g. workdir) to
// the returned records.
func Parse(src io.ReadCloser) ([]Record, error) {
	defer src.Close()
	ps := []Record{}
	lineNum := 0

	sc := bufio.NewScanner(src)
	for sc.Scan() {
		lineNum++

		line := sc.Text()

		if len(line) == 0 {
			continue
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		tag, cmd, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("invalid line %d", lineNum)
		}

		cmdArgs, err := shellwords.Parse(cmd)
		if err != nil {
			return nil, err
		}

		ps = append(ps, Record{
			Tag:     tag,
			CmdArgs: cmdArgs,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	return ps, nil
}
