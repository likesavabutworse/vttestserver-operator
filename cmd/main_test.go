package main

import "testing"

func TestNamespaceConfig(t *testing.T) {
	if got := namespaceConfig(""); got != nil {
		t.Fatalf("empty value must watch all namespaces, got %v", got)
	}
	if got := namespaceConfig(" , "); got != nil {
		t.Fatalf("blank entries must be ignored, got %v", got)
	}
	got := namespaceConfig("a, b,,a")
	if len(got) != 2 {
		t.Fatalf("want namespaces a and b, got %v", got)
	}
	for _, ns := range []string{"a", "b"} {
		if _, ok := got[ns]; !ok {
			t.Fatalf("missing %s in %v", ns, got)
		}
	}
}
