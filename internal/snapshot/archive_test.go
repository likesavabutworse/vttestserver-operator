package snapshot

import (
	"archive/tar"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestArchiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "vt_ks", "t.ibd"), "table data", 0o640)
	writeFile(t, filepath.Join(src, "topo", "vschema.json"), `{"sharded":true}`, 0o600)
	writeFile(t, filepath.Join(src, MarkerFile), "old marker", 0o600)
	writeFile(t, filepath.Join(src, lostAndFound, "junk"), "junk", 0o600)
	if err := os.Symlink("topo/vschema.json", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	d1, err := WriteArchive(&buf, src)
	if err != nil {
		t.Fatal(err)
	}
	if d1.SizeBytes != int64(buf.Len()) {
		t.Fatalf("size %d, buffer %d", d1.SizeBytes, buf.Len())
	}

	dst := t.TempDir()
	d2, err := ExtractArchive(bytes.NewReader(buf.Bytes()), dst)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("digest differs: write %+v, extract %+v", d1, d2)
	}
	if got := readFile(t, filepath.Join(dst, "vt_ks", "t.ibd")); got != "table data" {
		t.Fatalf("t.ibd: %q", got)
	}
	st, err := os.Stat(filepath.Join(dst, "vt_ks", "t.ibd"))
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode: %v %v", st, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "link")); err != nil || l != "topo/vschema.json" {
		t.Fatalf("symlink: %q %v", l, err)
	}
	for _, skipped := range []string{MarkerFile, lostAndFound} {
		if _, err := os.Lstat(filepath.Join(dst, skipped)); err == nil {
			t.Errorf("%s must not be archived", skipped)
		}
	}
}

func TestArchiveSkipsSockets(t *testing.T) {
	// Unix socket paths are limited to about 104 bytes, and t.TempDir is long on macOS.
	src, err := os.MkdirTemp("/tmp", "vts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(src) })
	l, err := net.Listen("unix", filepath.Join(src, "mysql.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	writeFile(t, filepath.Join(src, "data"), "x", 0o600)

	var buf bytes.Buffer
	if _, err := WriteArchive(&buf, src); err != nil {
		t.Fatalf("a socket must not fail the archive: %v", err)
	}
	dst := t.TempDir()
	if _, err := ExtractArchive(&buf, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "mysql.sock")); err == nil {
		t.Fatal("socket must not be archived")
	}
	if readFile(t, filepath.Join(dst, "data")) != "x" {
		t.Fatal("regular file lost")
	}
}

func evilArchive(t *testing.T, hdrs ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = 1
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte("x"))
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func TestExtractRejectsEscapes(t *testing.T) {
	cases := map[string][]tar.Header{
		"dot dot":       {{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o600}},
		"nested dotdot": {{Name: "a/../../evil", Typeflag: tar.TypeReg, Mode: 0o600}},
		"absolute":      {{Name: "/tmp/evil", Typeflag: tar.TypeReg, Mode: 0o600}},
		"abs symlink":   {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}},
		"up symlink":    {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "../.."}},
		"through link": {
			{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "sub/.."},
			{Name: "l/../../x", Typeflag: tar.TypeReg, Mode: 0o600},
		},
		"device": {{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o600}},
	}
	for name, hdrs := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dst := filepath.Join(parent, "data")
			if _, err := ExtractArchive(bytes.NewReader(evilArchive(t, hdrs...)), dst); err == nil {
				t.Fatal("want error")
			}
			entries, _ := os.ReadDir(parent)
			for _, e := range entries {
				if e.Name() != "data" {
					t.Fatalf("file escaped the data dir: %s", e.Name())
				}
			}
		})
	}
}

func TestExtractDetectsCorruption(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a"), strings.Repeat("hello ", 1000), 0o600)
	var buf bytes.Buffer
	if _, err := WriteArchive(&buf, src); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	b[len(b)/2] ^= 0xff
	if _, err := ExtractArchive(bytes.NewReader(b), t.TempDir()); err == nil {
		t.Fatal("want error for a corrupt stream")
	}
}

func TestClearDirKeepsLostAndFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "b"), "x", 0o600)
	writeFile(t, filepath.Join(dir, "c"), "x", 0o600)
	writeFile(t, filepath.Join(dir, lostAndFound, "f"), "x", 0o600)
	if err := ClearDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != lostAndFound {
		t.Fatalf("want only lost+found, got %v", entries)
	}
	if err := ClearDir(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("missing dir is not an error: %v", err)
	}
}
