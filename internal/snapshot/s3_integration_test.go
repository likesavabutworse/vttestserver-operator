package snapshot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

// These tests use an in-process fake S3 server by default. To run them against real S3 compatible
// storage (for example Silo on localhost:19000 with the Tilt setup), set VTTEST_S3_ENDPOINT:
//
//	VTTEST_S3_ENDPOINT=http://localhost:19000 VTTEST_S3_BUCKET=vttest-snapshots \
//	AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... go test ./internal/snapshot -run S3 -v
//
// Every run uses its own key prefix and removes it at the end.
func newTestS3Store(t *testing.T) *S3Store {
	t.Helper()
	endpoint := os.Getenv("VTTEST_S3_ENDPOINT")
	bucket := os.Getenv("VTTEST_S3_BUCKET")
	if bucket == "" {
		bucket = "vttest-snapshots"
	}
	if endpoint == "" {
		endpoint = startFakeS3(t, bucket)
	}
	st, err := NewS3Store(context.Background(), S3Options{
		Bucket:         bucket,
		Prefix:         fmt.Sprintf("it-%d", time.Now().UnixNano()),
		Region:         "us-east-1",
		Endpoint:       endpoint,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func startFakeS3(t *testing.T, bucket string) string {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(bucket); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	return srv.URL
}

func randomFile(t *testing.T, path string, size int) {
	t.Helper()
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestS3StoreRoundTrip(t *testing.T) {
	st := newTestS3Store(t)
	ctx := context.Background()
	src := t.TempDir()
	// Random data does not compress, so the archive is larger than the 16 MiB multipart threshold.
	randomFile(t, filepath.Join(src, "vt_commerce", "big.ibd"), 20<<20)
	randomFile(t, filepath.Join(src, "topo", "small"), 100)
	fp := manifest.Fingerprint{Keyspaces: []manifest.Keyspace{{Name: "commerce", Shards: 1}}}

	m, err := Take(ctx, st, TakeOptions{DataDir: src, Name: "g1", Tags: []string{"latest", "golden"}, Fingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Delete(ctx, "g1") })
	if m.SizeBytes < 20<<20 {
		t.Fatalf("archive is %d bytes, want at least 20 MiB", m.SizeBytes)
	}

	got, err := st.Manifest(ctx, "g1")
	if err != nil || got.SHA256 != m.SHA256 || got.SizeBytes != m.SizeBytes {
		t.Fatalf("manifest %+v, err %v, want sha %s", got, err, m.SHA256)
	}
	for _, tag := range []string{"latest", "golden"} {
		if name, err := st.Resolve(ctx, tag); err != nil || name != "g1" {
			t.Fatalf("tag %s: %q %v", tag, name, err)
		}
	}

	dst := t.TempDir()
	res, err := Restore(ctx, st, dst, Ref{Tag: "latest"}, fp)
	if err != nil || res.Skipped || res.SHA256 != m.SHA256 {
		t.Fatalf("restore %+v: %v", res, err)
	}
	a, _ := os.ReadFile(filepath.Join(src, "vt_commerce", "big.ibd"))
	b, _ := os.ReadFile(filepath.Join(dst, "vt_commerce", "big.ibd"))
	if string(a) != string(b) {
		t.Fatal("restored file differs")
	}

	if _, err := Take(ctx, st, TakeOptions{DataDir: src, Name: "g1", Fingerprint: fp}); err == nil {
		t.Fatal("a second snapshot with the same name must fail")
	}
}

func TestS3StoreNotFound(t *testing.T) {
	st := newTestS3Store(t)
	ctx := context.Background()
	if _, err := st.Resolve(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := st.Manifest(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Manifest: %v", err)
	}
	if _, err := st.Fetch(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Fetch: %v", err)
	}
	if err := st.Delete(ctx, "nope"); err != nil {
		t.Fatalf("deleting a missing snapshot is not an error: %v", err)
	}
}

func TestS3StoreDeleteKeepsOtherSnapshots(t *testing.T) {
	st := newTestS3Store(t)
	ctx := context.Background()
	src := t.TempDir()
	randomFile(t, filepath.Join(src, "f"), 1000)
	fp := manifest.Fingerprint{Keyspaces: []manifest.Keyspace{{Name: "commerce", Shards: 1}}}
	for name, tags := range map[string][]string{"keep": {"keep", "shared"}, "drop": {"drop"}} {
		if _, err := Take(ctx, st, TakeOptions{DataDir: src, Name: name, Tags: tags, Fingerprint: fp}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = st.Delete(ctx, "keep"); _ = st.Delete(ctx, "drop") })

	if err := st.Delete(ctx, "drop"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Manifest(ctx, "drop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("drop must be gone: %v", err)
	}
	if _, err := st.Resolve(ctx, "drop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("its tag must be gone: %v", err)
	}
	if _, err := st.Manifest(ctx, "keep"); err != nil {
		t.Fatalf("keep must stay: %v", err)
	}
	if name, err := st.Resolve(ctx, "shared"); err != nil || name != "keep" {
		t.Fatalf("the tag of keep must stay: %q %v", name, err)
	}
}

func TestS3StoreNeedsBucket(t *testing.T) {
	if _, err := NewS3Store(context.Background(), S3Options{}); err == nil {
		t.Fatal("want error without a bucket")
	}
}
