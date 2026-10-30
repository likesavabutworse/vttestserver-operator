package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

var fp = manifest.Fingerprint{Keyspaces: []manifest.Keyspace{{Name: "commerce", Shards: 1}, {Name: "customer", Shards: 2}}}

func seed(t *testing.T) string {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vt_commerce", "t.ibd"), "rows", 0o600)
	writeFile(t, filepath.Join(dir, "topo", "x"), "topo", 0o600)
	return dir
}

func take(t *testing.T, st Store, dir, name string, tags ...string) manifest.Manifest {
	t.Helper()
	m, err := Take(context.Background(), st, TakeOptions{
		DataDir: dir, Name: name, Tags: tags, Fingerprint: fp, Vitess: "25.0.0",
		Now: func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTakeWriteOrder(t *testing.T) {
	st := NewMemStore()
	take(t, st, seed(t), "g1", "latest", "golden")
	want := []string{"data", "manifest", "tag", "tag"}
	if !reflect.DeepEqual(st.Order, want) {
		t.Fatalf("order %v, want %v", st.Order, want)
	}
	if st.Tags["latest"] != "g1" || st.Tags["golden"] != "g1" {
		t.Fatalf("tags: %v", st.Tags)
	}
}

func TestTakeRefusesDuplicateAndBadNames(t *testing.T) {
	st := NewMemStore()
	dir := seed(t)
	take(t, st, dir, "g1")
	ctx := context.Background()
	if _, err := Take(ctx, st, TakeOptions{DataDir: dir, Name: "g1", Fingerprint: fp}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already exists, got %v", err)
	}
	if _, err := Take(ctx, st, TakeOptions{DataDir: dir, Name: "../x", Fingerprint: fp}); err == nil {
		t.Fatal("want error for a bad name")
	}
	if _, err := Take(ctx, st, TakeOptions{DataDir: dir, Name: "g2", Tags: []string{"a/b"}, Fingerprint: fp}); err == nil {
		t.Fatal("want error for a bad tag")
	}
	if _, err := Take(ctx, st, TakeOptions{DataDir: dir, Name: "g2", Fingerprint: manifest.Fingerprint{}}); err == nil {
		t.Fatal("want error for an empty fingerprint")
	}
}

func TestRestoreByTagAndMarker(t *testing.T) {
	st := NewMemStore()
	m := take(t, st, seed(t), "g1", "latest")
	dst := t.TempDir()
	ctx := context.Background()

	res, err := Restore(ctx, st, dst, Ref{Tag: "latest"}, fp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "g1" || res.SHA256 != m.SHA256 || res.Skipped {
		t.Fatalf("result: %+v", res)
	}
	if readFile(t, filepath.Join(dst, "vt_commerce", "t.ibd")) != "rows" {
		t.Fatal("data missing")
	}

	// A newer "latest" must not change a finished restore.
	take(t, st, seed(t), "g2", "latest")
	res, err = Restore(ctx, st, dst, Ref{Tag: "latest"}, fp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || res.Name != "g1" {
		t.Fatalf("second run must skip and report g1, got %+v", res)
	}
}

func TestRestoreRedoesAnUnfinishedRestore(t *testing.T) {
	st := NewMemStore()
	take(t, st, seed(t), "g1", "latest")
	dst := t.TempDir()
	// A dead init container left partial data and lost+found, but no marker.
	writeFile(t, filepath.Join(dst, "vt_commerce", "partial.ibd"), "half", 0o600)
	writeFile(t, filepath.Join(dst, lostAndFound, "keep"), "keep", 0o600)

	res, err := Restore(context.Background(), st, dst, Ref{Name: "g1"}, fp)
	if err != nil || res.Skipped {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "vt_commerce", "partial.ibd")); err == nil {
		t.Fatal("partial data must be removed")
	}
	if readFile(t, filepath.Join(dst, lostAndFound, "keep")) != "keep" {
		t.Fatal("lost+found must stay")
	}
}

func TestRestoreFingerprintMismatchWritesNothing(t *testing.T) {
	st := NewMemStore()
	take(t, st, seed(t), "g1", "latest")
	dst := t.TempDir()
	writeFile(t, filepath.Join(dst, "existing"), "x", 0o600)
	wrong := manifest.Fingerprint{Keyspaces: []manifest.Keyspace{{Name: "commerce", Shards: 1}, {Name: "customer", Shards: 3}}}

	_, err := Restore(context.Background(), st, dst, Ref{Tag: "latest"}, wrong)
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("want fingerprint mismatch, got %v", err)
	}
	if readFile(t, filepath.Join(dst, "existing")) != "x" {
		t.Fatal("a failed check must not touch the data dir")
	}
	if _, err := os.Stat(filepath.Join(dst, MarkerFile)); err == nil {
		t.Fatal("no marker after a failure")
	}
}

func TestRestoreDetectsDamagedData(t *testing.T) {
	st := NewMemStore()
	take(t, st, seed(t), "g1")
	b := st.Data["g1"]
	b[len(b)/2] ^= 0xff
	dst := t.TempDir()
	if _, err := Restore(context.Background(), st, dst, Ref{Name: "g1"}, fp); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(filepath.Join(dst, MarkerFile)); err == nil {
		t.Fatal("no marker after a failure")
	}
}

func TestRestoreSHAMismatchWithManifest(t *testing.T) {
	st := NewMemStore()
	take(t, st, seed(t), "g1")
	// Replace the data with a valid archive of different content.
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "x"), "other", 0o600)
	st2 := NewMemStore()
	take(t, st2, other, "g1")
	st.Data["g1"] = st2.Data["g1"]

	dst := t.TempDir()
	_, err := Restore(context.Background(), st, dst, Ref{Name: "g1"}, fp)
	if err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("want damaged error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, MarkerFile)); err == nil {
		t.Fatal("no marker after a failure")
	}
}

func TestRestoreRefAndMissing(t *testing.T) {
	st := NewMemStore()
	ctx := context.Background()
	if _, err := Restore(ctx, st, t.TempDir(), Ref{}, fp); err == nil {
		t.Fatal("want error without a ref")
	}
	if _, err := Restore(ctx, st, t.TempDir(), Ref{Name: "a", Tag: "b"}, fp); err == nil {
		t.Fatal("want error with both name and tag")
	}
	if _, err := Restore(ctx, st, t.TempDir(), Ref{Tag: "nope"}, fp); err == nil {
		t.Fatal("want error for a missing tag")
	}
	if _, err := Restore(ctx, st, t.TempDir(), Ref{Name: "../x"}, fp); err == nil {
		t.Fatal("want error for a bad name")
	}
}
