// Package manifest holds the snapshot manifest, the fingerprint and the
// termination message. It has no third party imports, so the operator and
// datactl can share it without pulling in the S3 SDK or Kubernetes types.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// FormatVersion is the manifest format this code writes and can read.
const FormatVersion = 1

// MaxTerminationMessageBytes is the Kubernetes limit for a termination message.
const MaxTerminationMessageBytes = 4096

// Keyspace is one keyspace and its shard count. It is a separate type from the
// CRD type on purpose: datactl must not depend on Kubernetes packages.
type Keyspace struct {
	Name   string `json:"name"`
	Shards int    `json:"shards"`
}

// Fingerprint is the topology a snapshot was taken with.
type Fingerprint struct {
	Keyspaces []Keyspace `json:"keyspaces"`
}

// Manifest describes one snapshot in the store.
type Manifest struct {
	FormatVersion int         `json:"formatVersion"`
	Name          string      `json:"name"`
	SHA256        string      `json:"sha256"`
	SizeBytes     int64       `json:"sizeBytes"`
	CreatedAt     time.Time   `json:"createdAt"`
	Vitess        string      `json:"vitess,omitempty"`
	Tags          []string    `json:"tags,omitempty"`
	Fingerprint   Fingerprint `json:"fingerprint"`
}

// TerminationMessage is what datactl writes for the operator to read.
type TerminationMessage struct {
	Name      string `json:"name,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ParseKeyspace parses the "name:shards" flag form.
func ParseKeyspace(s string) (Keyspace, error) {
	name, shards, ok := strings.Cut(s, ":")
	if !ok {
		return Keyspace{}, fmt.Errorf("keyspace %q: want name:shards", s)
	}
	n, err := strconv.Atoi(shards)
	if err != nil {
		return Keyspace{}, fmt.Errorf("keyspace %q: shards is not a number", s)
	}
	return Keyspace{Name: name, Shards: n}, nil
}

// NewFingerprint builds a validated fingerprint from "name:shards" values.
func NewFingerprint(specs []string) (Fingerprint, error) {
	fp := Fingerprint{}
	for _, s := range specs {
		ks, err := ParseKeyspace(s)
		if err != nil {
			return Fingerprint{}, err
		}
		fp.Keyspaces = append(fp.Keyspaces, ks)
	}
	if err := fp.Validate(); err != nil {
		return Fingerprint{}, err
	}
	return fp, nil
}

// ParseFingerprint reads a fingerprint in the same JSON shape as the manifest
// "fingerprint" block, and validates it.
func ParseFingerprint(r io.Reader) (Fingerprint, error) {
	var fp Fingerprint
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fp); err != nil {
		return Fingerprint{}, fmt.Errorf("parse fingerprint: %w", err)
	}
	if err := fp.Validate(); err != nil {
		return Fingerprint{}, err
	}
	return fp, nil
}

// Validate checks that the fingerprint has at least one keyspace, that names
// are valid and unique, and that every shard count is at least 1.
func (f Fingerprint) Validate() error {
	if len(f.Keyspaces) == 0 {
		return fmt.Errorf("fingerprint has no keyspaces")
	}
	seen := make(map[string]bool, len(f.Keyspaces))
	for _, ks := range f.Keyspaces {
		if err := ValidateName(ks.Name); err != nil {
			return fmt.Errorf("keyspace name: %w", err)
		}
		if ks.Shards < 1 {
			return fmt.Errorf("keyspace %q: shards must be at least 1, got %d", ks.Name, ks.Shards)
		}
		if seen[ks.Name] {
			return fmt.Errorf("keyspace %q is listed twice", ks.Name)
		}
		seen[ks.Name] = true
	}
	return nil
}

// Check compares the fingerprint of a snapshot with the expected one. Order does
// not matter. The error lists every difference.
func (f Fingerprint) Check(expected Fingerprint) error {
	have := shardsByName(f)
	want := shardsByName(expected)
	var diffs []string
	for name, w := range want {
		h, ok := have[name]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("keyspace %q is missing in the snapshot", name))
		case h != w:
			diffs = append(diffs, fmt.Sprintf("keyspace %q has %d shards in the snapshot, spec wants %d", name, h, w))
		}
	}
	for name := range have {
		if _, ok := want[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("keyspace %q is in the snapshot but not in the spec", name))
		}
	}
	if len(diffs) == 0 {
		return nil
	}
	slices.Sort(diffs)
	return fmt.Errorf("fingerprint mismatch: %s", strings.Join(diffs, "; "))
}

func shardsByName(f Fingerprint) map[string]int {
	m := make(map[string]int, len(f.Keyspaces))
	for _, ks := range f.Keyspaces {
		m[ks.Name] = ks.Shards
	}
	return m
}

// ValidateName checks a snapshot name, tag or keyspace name. These become S3
// key parts, so path separators and dot segments are not allowed.
func ValidateName(s string) error {
	if s == "" {
		return fmt.Errorf("name is empty")
	}
	if len(s) > 128 {
		return fmt.Errorf("name %q is longer than 128 characters", s)
	}
	if s == "." || s == ".." {
		return fmt.Errorf("name %q is not allowed", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("name %q has the character %q: use letters, digits, '-', '_' and '.'", s, r)
		}
	}
	return nil
}

// Marshal writes the manifest as indented JSON.
func (m Manifest) Marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// Parse reads a manifest. It rejects a format version that is newer than this
// code understands.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if m.FormatVersion < 1 || m.FormatVersion > FormatVersion {
		return Manifest{}, fmt.Errorf("manifest formatVersion %d is not supported (this build reads up to %d)", m.FormatVersion, FormatVersion)
	}
	if err := ValidateName(m.Name); err != nil {
		return Manifest{}, fmt.Errorf("manifest name: %w", err)
	}
	if err := m.Fingerprint.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// Marshal writes the termination message. If it is larger than the Kubernetes
// limit, it cuts the error text.
func (t TerminationMessage) Marshal() []byte {
	b, _ := json.Marshal(t)
	for len(b) > MaxTerminationMessageBytes && len(t.Error) > 0 {
		cut := max(len(t.Error)-(len(b)-MaxTerminationMessageBytes)-3, 0)
		t.Error = strings.ToValidUTF8(t.Error[:cut], "") + "..."
		b, _ = json.Marshal(t)
		if cut == 0 {
			break
		}
	}
	return b
}
