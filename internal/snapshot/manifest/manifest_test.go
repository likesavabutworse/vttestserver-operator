package manifest

import (
	"strings"
	"testing"
	"time"
)

func TestNewFingerprint(t *testing.T) {
	fp, err := NewFingerprint([]string{"commerce:1", "customer:10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Keyspaces) != 2 || fp.Keyspaces[1] != (Keyspace{Name: "customer", Shards: 10}) {
		t.Fatalf("unexpected fingerprint: %+v", fp)
	}
}

func TestNewFingerprintErrors(t *testing.T) {
	cases := map[string][]string{
		"no colon":     {"commerce"},
		"not a number": {"commerce:x"},
		"zero shards":  {"commerce:0"},
		"duplicate":    {"commerce:1", "commerce:2"},
		"bad name":     {"a/b:1"},
		"empty list":   {},
		"empty name":   {":1"},
		"negative":     {"commerce:-1"},
		"dot dot name": {"..:1"},
	}
	for name, specs := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewFingerprint(specs); err == nil {
				t.Fatalf("want error for %v", specs)
			}
		})
	}
}

func TestParseFingerprintJSON(t *testing.T) {
	fp, err := ParseFingerprint(strings.NewReader(`{"keyspaces":[{"name":"commerce","shards":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if fp.Keyspaces[0].Name != "commerce" {
		t.Fatalf("unexpected: %+v", fp)
	}
	if _, err := ParseFingerprint(strings.NewReader(`{"keyspaces":[{"name":"commerce","shards":1}],"extra":1}`)); err == nil {
		t.Fatal("want error for unknown field")
	}
	if _, err := ParseFingerprint(strings.NewReader(`not json`)); err == nil {
		t.Fatal("want error for invalid JSON")
	}
}

func TestCheckIgnoresOrder(t *testing.T) {
	a := Fingerprint{Keyspaces: []Keyspace{{"commerce", 1}, {"customer", 10}}}
	b := Fingerprint{Keyspaces: []Keyspace{{"customer", 10}, {"commerce", 1}}}
	if err := a.Check(b); err != nil {
		t.Fatalf("order must not matter: %v", err)
	}
}

func TestCheckMismatch(t *testing.T) {
	snap := Fingerprint{Keyspaces: []Keyspace{{"commerce", 1}, {"customer", 2}, {"extra", 1}}}
	want := Fingerprint{Keyspaces: []Keyspace{{"commerce", 1}, {"customer", 3}, {"other", 1}}}
	err := snap.Check(want)
	if err == nil {
		t.Fatal("want mismatch")
	}
	for _, s := range []string{
		`"customer" has 2 shards in the snapshot, spec wants 3`,
		`"other" is missing in the snapshot`,
		`"extra" is in the snapshot but not in the spec`,
	} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q lacks %q", err, s)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"golden-20261024", "latest", "v1.2_x", "A"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a b", "../x", "ä", strings.Repeat("a", 129)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	in := Manifest{
		FormatVersion: FormatVersion,
		Name:          "golden-1",
		SHA256:        strings.Repeat("a", 64),
		SizeBytes:     42,
		CreatedAt:     time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		Tags:          []string{"latest"},
		Fingerprint:   Fingerprint{Keyspaces: []Keyspace{{"commerce", 1}}},
	}
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.SHA256 != in.SHA256 || !out.CreatedAt.Equal(in.CreatedAt) || out.Fingerprint.Keyspaces[0] != in.Fingerprint.Keyspaces[0] {
		t.Fatalf("round trip changed the manifest: %+v", out)
	}
}

func TestParseRejectsFutureFormat(t *testing.T) {
	_, err := Parse([]byte(`{"formatVersion":99,"name":"x","fingerprint":{"keyspaces":[{"name":"a","shards":1}]}}`))
	if err == nil || !strings.Contains(err.Error(), "formatVersion 99") {
		t.Fatalf("want formatVersion error, got %v", err)
	}
	if _, err := Parse([]byte(`{"name":"x"}`)); err == nil {
		t.Fatal("want error for missing formatVersion")
	}
}

func TestTerminationMessageTruncates(t *testing.T) {
	msg := TerminationMessage{Error: strings.Repeat("é", 5000)}.Marshal()
	if len(msg) > MaxTerminationMessageBytes {
		t.Fatalf("message is %d bytes, limit %d", len(msg), MaxTerminationMessageBytes)
	}
	small := TerminationMessage{Name: "n", SHA256: "s"}.Marshal()
	if string(small) != `{"name":"n","sha256":"s"}` {
		t.Fatalf("unexpected: %s", small)
	}
}
