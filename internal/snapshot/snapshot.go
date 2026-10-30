package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// TakeOptions are the inputs of Take.
type TakeOptions struct {
	DataDir     string
	Name        string
	Tags        []string
	Fingerprint manifest.Fingerprint
	Vitess      string
	// Now is the clock. It is time.Now when nil.
	Now func() time.Time
}

// Take archives the data dir and writes it to the store. The order is data, then
// manifest, then tags, so a reader that sees a tag always finds a complete snapshot.
// The caller must make sure mysqld is stopped.
func Take(ctx context.Context, st Store, o TakeOptions) (manifest.Manifest, error) {
	if err := manifest.ValidateName(o.Name); err != nil {
		return manifest.Manifest{}, fmt.Errorf("snapshot name: %w", err)
	}
	for _, t := range o.Tags {
		if err := manifest.ValidateName(t); err != nil {
			return manifest.Manifest{}, fmt.Errorf("tag: %w", err)
		}
	}
	if err := o.Fingerprint.Validate(); err != nil {
		return manifest.Manifest{}, err
	}
	if _, err := st.Manifest(ctx, o.Name); err == nil {
		return manifest.Manifest{}, fmt.Errorf("snapshot %q already exists", o.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return manifest.Manifest{}, err
	}
	if _, err := os.Stat(o.DataDir); err != nil {
		return manifest.Manifest{}, err
	}

	pr, pw := io.Pipe()
	type result struct {
		d   Digest
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, err := WriteArchive(pw, o.DataDir)
		pw.CloseWithError(err)
		done <- result{d, err}
	}()
	putErr := st.PutData(ctx, o.Name, pr)
	// Unblock the writer if the store stopped reading early.
	pr.CloseWithError(errors.New("upload ended"))
	res := <-done
	if res.err != nil {
		return manifest.Manifest{}, res.err
	}
	if putErr != nil {
		return manifest.Manifest{}, fmt.Errorf("upload data: %w", putErr)
	}

	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	m := manifest.Manifest{
		FormatVersion: manifest.FormatVersion,
		Name:          o.Name,
		SHA256:        res.d.SHA256,
		SizeBytes:     res.d.SizeBytes,
		CreatedAt:     now().UTC(),
		Vitess:        o.Vitess,
		Tags:          o.Tags,
		Fingerprint:   o.Fingerprint,
	}
	if err := st.PutManifest(ctx, m); err != nil {
		return manifest.Manifest{}, fmt.Errorf("upload manifest: %w", err)
	}
	for _, t := range o.Tags {
		if err := st.PutTag(ctx, t, o.Name); err != nil {
			return manifest.Manifest{}, fmt.Errorf("upload tag %s: %w", t, err)
		}
	}
	return m, nil
}

// Ref selects a snapshot by name or by tag. Exactly one must be set.
type Ref struct {
	Name string
	Tag  string
}

// RestoreResult is the outcome of Restore.
type RestoreResult struct {
	Name   string
	SHA256 string
	// Skipped is true when the marker file showed an earlier finished restore.
	Skipped bool
}

type marker struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Restore puts a snapshot into the data dir. The marker file is the commit point:
//
//   - marker present: nothing to do, the earlier result is returned.
//   - marker absent: the data dir is cleared (except lost+found), the archive is
//     extracted and checked against the manifest, and the marker is written last.
//
// So a restore that died halfway is redone, and mysqld never starts on a partial
// data dir. The fingerprint is checked before anything is written.
func Restore(ctx context.Context, st Store, dir string, ref Ref, expected manifest.Fingerprint) (RestoreResult, error) {
	if (ref.Name == "") == (ref.Tag == "") {
		return RestoreResult{}, errors.New("set exactly one of snapshot name or tag")
	}
	if err := expected.Validate(); err != nil {
		return RestoreResult{}, err
	}
	markerPath := filepath.Join(dir, MarkerFile)
	if b, err := os.ReadFile(markerPath); err == nil {
		var mk marker
		if err := json.Unmarshal(b, &mk); err != nil {
			return RestoreResult{}, fmt.Errorf("marker %s is not valid: %w", markerPath, err)
		}
		return RestoreResult{Name: mk.Name, SHA256: mk.SHA256, Skipped: true}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreResult{}, err
	}

	name := ref.Name
	if name == "" {
		var err error
		if name, err = st.Resolve(ctx, ref.Tag); err != nil {
			return RestoreResult{}, fmt.Errorf("resolve tag %q: %w", ref.Tag, err)
		}
	}
	if err := manifest.ValidateName(name); err != nil {
		return RestoreResult{}, fmt.Errorf("snapshot name: %w", err)
	}
	m, err := st.Manifest(ctx, name)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("read manifest of %q: %w", name, err)
	}
	if err := m.Fingerprint.Check(expected); err != nil {
		return RestoreResult{}, err
	}

	if err := ClearDir(dir); err != nil {
		return RestoreResult{}, err
	}
	body, err := st.Fetch(ctx, name)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("fetch data of %q: %w", name, err)
	}
	defer func() { _ = body.Close() }()
	d, err := ExtractArchive(body, dir)
	if err != nil {
		return RestoreResult{}, err
	}
	if d.SHA256 != m.SHA256 || d.SizeBytes != m.SizeBytes {
		return RestoreResult{}, fmt.Errorf("snapshot %q is damaged: sha256 %s, size %d, manifest says sha256 %s, size %d",
			name, d.SHA256, d.SizeBytes, m.SHA256, m.SizeBytes)
	}
	b, _ := json.Marshal(marker{Name: name, SHA256: m.SHA256})
	tmp := markerPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return RestoreResult{}, err
	}
	if err := os.Rename(tmp, markerPath); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Name: name, SHA256: m.SHA256}, nil
}
