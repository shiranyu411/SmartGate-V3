package opid

import (
	"strings"
	"testing"
)

func TestCleanAcceptsStableFileSafeIDs(t *testing.T) {
	for _, id := range []string{"apply-1", "provider_2026.06.24", "ABC123"} {
		got, err := Clean(id)
		if err != nil {
			t.Fatalf("clean %q: %v", id, err)
		}
		if got != id {
			t.Fatalf("clean %q = %q", id, got)
		}
	}
}

func TestCleanRejectsUnsafeIDs(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../escape", "nested/path", "has space", " padded", strings.Repeat("x", MaxLength+1)} {
		if _, err := Clean(id); err == nil {
			t.Fatalf("expected %q to be rejected", id)
		}
	}
}
