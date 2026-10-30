package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const golden = "[mysqld]\nmax_connections = 500\ninnodb_buffer_pool_size = 134217728\n\n[client]\nport = 3306\n"

func cnfAt(t *testing.T, dir, tablet, content string) string {
	t.Helper()
	p := filepath.Join(dir, tablet, "my.cnf")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyMyCnfAddsBlockAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	p := cnfAt(t, dir, "vt_0000000001", golden)
	n, err := ApplyMyCnf(dir, []byte("[mysqld]\nmax_connections = 88\n"))
	if err != nil || n != 1 {
		t.Fatalf("changed %d, err %v", n, err)
	}
	got, _ := os.ReadFile(p)
	s := string(got)
	if !strings.HasPrefix(s, golden) {
		t.Fatalf("the original content must stay first:\n%s", s)
	}
	if !strings.HasSuffix(s, "# BEGIN vttestserver-operator\n[mysqld]\nmax_connections = 88\n# END vttestserver-operator\n") {
		t.Fatalf("block missing or in the wrong place:\n%s", s)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v", st.Mode().Perm())
	}
}

func TestApplyMyCnfIsIdempotentAndReplaces(t *testing.T) {
	dir := t.TempDir()
	p := cnfAt(t, dir, "vt_0000000001", golden)
	extra := []byte("[mysqld]\nmax_connections = 88\n")
	if _, err := ApplyMyCnf(dir, extra); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(p)

	n, err := ApplyMyCnf(dir, extra)
	if err != nil || n != 0 {
		t.Fatalf("a second run with the same input must change nothing: %d %v", n, err)
	}

	if n, err := ApplyMyCnf(dir, []byte("[mysqld]\nmax_connections = 99\n")); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := os.ReadFile(p)
	if strings.Count(string(got), "# BEGIN") != 1 || strings.Contains(string(got), "= 88") || !strings.Contains(string(got), "= 99") {
		t.Fatalf("the old block must be replaced:\n%s", got)
	}
	if !strings.HasPrefix(string(got), golden) || string(first) == string(got) {
		t.Fatalf("unexpected result:\n%s", got)
	}
}

func TestApplyMyCnfEmptyRemovesTheBlock(t *testing.T) {
	dir := t.TempDir()
	p := cnfAt(t, dir, "vt_0000000001", golden)
	if _, err := ApplyMyCnf(dir, []byte("[mysqld]\nmax_connections = 88\n")); err != nil {
		t.Fatal(err)
	}
	if n, err := ApplyMyCnf(dir, nil); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != golden {
		t.Fatalf("the file must be back to the original:\n%q", got)
	}
}

func TestApplyMyCnfHandlesNoFilesManyFilesAndNoTrailingNewline(t *testing.T) {
	empty := t.TempDir()
	if n, err := ApplyMyCnf(empty, []byte("[mysqld]\nx = 1\n")); err != nil || n != 0 {
		t.Fatalf("a fresh data dir is not an error: %d %v", n, err)
	}
	if n, err := ApplyMyCnf(filepath.Join(empty, "missing"), []byte("[mysqld]\nx = 1\n")); err != nil || n != 0 {
		t.Fatalf("a missing dir is not an error: %d %v", n, err)
	}

	dir := t.TempDir()
	a := cnfAt(t, dir, "vt_0000000001", "[mysqld]\na = 1")
	b := cnfAt(t, dir, "vt_0000000002", golden)
	other := cnfAt(t, dir, "mysql", golden) // not a tablet dir
	if n, err := ApplyMyCnf(dir, []byte("[mysqld]\nx = 1\n")); err != nil || n != 2 {
		t.Fatalf("want 2 files changed, got %d, %v", n, err)
	}
	got, _ := os.ReadFile(a)
	if !strings.HasPrefix(string(got), "[mysqld]\na = 1\n# BEGIN") {
		t.Fatalf("a missing final newline must be added:\n%q", got)
	}
	if g, _ := os.ReadFile(b); !strings.Contains(string(g), "x = 1") {
		t.Fatal("second tablet dir not changed")
	}
	if g, _ := os.ReadFile(other); string(g) != golden {
		t.Fatal("a directory that is not vt_* must stay as it is")
	}
}

func TestStripMyCnfBlockWithUnclosedBlock(t *testing.T) {
	in := []byte("[mysqld]\na = 1\n\n# BEGIN vttestserver-operator\n[mysqld]\nx = 1\n")
	if got := string(stripMyCnfBlock(in)); got != "[mysqld]\na = 1" {
		t.Fatalf("got %q", got)
	}
}
