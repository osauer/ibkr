package ibkr

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// loadWireFixture reads a captured msgID-stripped (well, msgID-included as
// fields[0]) gateway frame from testdata/wire/<name>. Each line is one IBKR
// field; empty lines are empty fields; lines starting with '#' are comments.
//
// The dispatcher in connection.go passes msgID at index 0 to handlers, so
// fixtures preserve that — fields[0] is the msgID.
func loadWireFixture(t *testing.T, name string) []string {
	t.Helper()
	path := "testdata/wire/" + name
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open wire fixture %s: %v", path, err)
	}
	defer f.Close()
	var fields []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields = append(fields, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read wire fixture %s: %v", path, err)
	}
	return fields
}
