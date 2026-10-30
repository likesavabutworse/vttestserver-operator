package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot"
	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

func run(t *testing.T, st snapshot.Store, args ...string) (string, error) {
	t.Helper()
	newStore = func(context.Context, snapshot.S3Options) (snapshot.Store, error) { return st, nil }
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func readTermination(t *testing.T, p string) manifest.TerminationMessage {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.TerminationMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSnapshotThenRestore(t *testing.T) {
	st := snapshot.NewMemStore()
	src, dst, tl := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "term")
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, st, "snapshot", "--bucket", "b", "--data-dir", src, "--name", "g1", "--tag", "latest",
		"--keyspace", "commerce:1", "--keyspace", "customer:2", "--termination-log", tl); err != nil {
		t.Fatal(err)
	}
	snap := readTermination(t, tl)
	if snap.Name != "g1" || snap.SHA256 == "" || snap.SizeBytes == 0 {
		t.Fatalf("snapshot message: %+v", snap)
	}

	// The fingerprint comes from a file here, in another order.
	fpFile := filepath.Join(t.TempDir(), "fp.json")
	_ = os.WriteFile(fpFile, []byte(`{"keyspaces":[{"name":"customer","shards":2},{"name":"commerce","shards":1}]}`), 0o600)
	if _, err := run(t, st, "restore", "--bucket", "b", "--data-dir", dst, "--tag", "latest",
		"--fingerprint-file", fpFile, "--termination-log", tl); err != nil {
		t.Fatal(err)
	}
	if got := readTermination(t, tl); got.Name != "g1" || got.SHA256 != snap.SHA256 {
		t.Fatalf("restore message: %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "data" {
		t.Fatalf("restored file: %q", b)
	}
}

func TestRestoreMismatchWritesErrorToTerminationLog(t *testing.T) {
	st := snapshot.NewMemStore()
	src, tl := t.TempDir(), filepath.Join(t.TempDir(), "term")
	if _, err := run(t, st, "snapshot", "--bucket", "b", "--data-dir", src, "--name", "g1", "--tag", "latest",
		"--keyspace", "commerce:1", "--termination-log", tl); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, st, "restore", "--bucket", "b", "--data-dir", t.TempDir(), "--tag", "latest",
		"--keyspace", "commerce:3", "--termination-log", tl)
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("want fingerprint mismatch, got %v", err)
	}
	if got := readTermination(t, tl); !strings.Contains(got.Error, "fingerprint mismatch") {
		t.Fatalf("termination message: %+v", got)
	}
}

func TestPinFileWinsOverTag(t *testing.T) {
	st := snapshot.NewMemStore()
	src, tl := t.TempDir(), filepath.Join(t.TempDir(), "term")
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("one"), 0o600)
	snapArgs := func(name string) []string {
		return []string{"snapshot", "--bucket", "b", "--data-dir", src, "--name", name, "--tag", "latest",
			"--keyspace", "commerce:1", "--termination-log", tl}
	}
	if _, err := run(t, st, snapArgs("g1")...); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("two"), 0o600)
	if _, err := run(t, st, snapArgs("g2")...); err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(t.TempDir(), "name")
	_ = os.WriteFile(pin, []byte("g1\n"), 0o600)

	dst := t.TempDir()
	if _, err := run(t, st, "restore", "--bucket", "b", "--data-dir", dst, "--tag", "latest", "--pin-file", pin,
		"--keyspace", "commerce:1", "--termination-log", tl); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "one" {
		t.Fatalf("the pin (g1) must win over latest (g2), got %q", b)
	}
	// A missing pin file falls back to the tag.
	dst2 := t.TempDir()
	if _, err := run(t, st, "restore", "--bucket", "b", "--data-dir", dst2, "--tag", "latest", "--pin-file", pin+".none",
		"--keyspace", "commerce:1", "--termination-log", tl); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst2, "f")); string(b) != "two" {
		t.Fatalf("want latest (g2), got %q", b)
	}
}

func TestFlagRules(t *testing.T) {
	st := snapshot.NewMemStore()
	base := []string{"restore", "--bucket", "b", "--data-dir", t.TempDir(), "--termination-log", ""}
	cases := map[string][]string{
		"name and tag":             {"--name", "a", "--tag", "b", "--keyspace", "k:1"},
		"neither name nor tag":     {"--keyspace", "k:1"},
		"keyspace and file":        {"--tag", "a", "--keyspace", "k:1", "--fingerprint-file", "/x"},
		"no fingerprint":           {"--tag", "a"},
		"bad keyspace":             {"--tag", "a", "--keyspace", "k"},
		"missing fingerprint file": {"--tag", "a", "--fingerprint-file", "/does/not/exist"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := run(t, st, append(append([]string{}, base...), extra...)...); err == nil {
				t.Fatal("want error")
			}
		})
	}
	if _, err := run(t, st, "restore", "--data-dir", t.TempDir(), "--tag", "a", "--keyspace", "k:1"); err == nil {
		t.Fatal("bucket is required")
	}
}

func TestApplyMyCnf(t *testing.T) {
	dir := t.TempDir()
	cnf := filepath.Join(dir, "vt_0000000001", "my.cnf")
	if err := os.MkdirAll(filepath.Dir(cnf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cnf, []byte("[mysqld]\nmax_connections = 500\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(t.TempDir(), "extra.cnf")
	if err := os.WriteFile(extra, []byte("[mysqld]\nmax_connections = 88\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, snapshot.NewMemStore(), "apply-my-cnf", "--data-dir", dir, "--file", extra)
	if err != nil || !strings.Contains(out, "changed: 1") {
		t.Fatalf("%q %v", out, err)
	}
	b, _ := os.ReadFile(cnf)
	if !strings.Contains(string(b), "max_connections = 88") {
		t.Fatalf("settings missing:\n%s", b)
	}
	if out, err = run(t, snapshot.NewMemStore(), "apply-my-cnf", "--data-dir", dir, "--file", extra); err != nil || !strings.Contains(out, "changed: 0") {
		t.Fatalf("second run must change nothing: %q %v", out, err)
	}
	if _, err := run(t, snapshot.NewMemStore(), "apply-my-cnf", "--data-dir", dir, "--file", "/does/not/exist"); err == nil {
		t.Fatal("a missing settings file must be an error")
	}
}

func TestDelete(t *testing.T) {
	st := snapshot.NewMemStore()
	src := t.TempDir()
	if _, err := run(t, st, "snapshot", "--bucket", "b", "--data-dir", src, "--name", "g1", "--tag", "latest",
		"--keyspace", "commerce:1", "--termination-log", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, st, "delete", "--bucket", "b", "--name", "g1", "--termination-log", ""); err != nil {
		t.Fatal(err)
	}
	if len(st.Data)+len(st.Manifests)+len(st.Tags) != 0 {
		t.Fatalf("store not empty: %v %v %v", st.Data, st.Manifests, st.Tags)
	}
}
