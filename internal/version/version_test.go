package version

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrentAndLineUseInjectedValues(t *testing.T) {
	oldVersion, oldCommit, oldBuiltAt := Version, Commit, BuiltAt
	Version, Commit, BuiltAt = "9.9.9", "abc1234", "2026-06-26T00:00:00Z"
	defer func() {
		Version, Commit, BuiltAt = oldVersion, oldCommit, oldBuiltAt
	}()

	info := Current()
	if info.Version != "9.9.9" || info.Commit != "abc1234" || info.BuiltAt != "2026-06-26T00:00:00Z" {
		t.Fatalf("unexpected version info: %+v", info)
	}
	line := Line()
	if !strings.Contains(line, "smartgate 9.9.9") || !strings.Contains(line, "commit=abc1234") {
		t.Fatalf("unexpected version line: %q", line)
	}
	body, err := JSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var decoded Info
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if decoded.Version != info.Version || decoded.Commit != info.Commit {
		t.Fatalf("json mismatch: %+v", decoded)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode json fields: %v", err)
	}
	for _, field := range []string{"core", "core_min"} {
		if _, ok := fields[field]; ok {
			t.Fatalf("version JSON contains adapter-specific field %q", field)
		}
	}
}

func TestCurrentOmitsUnknownOrInvalidBuiltAt(t *testing.T) {
	oldBuiltAt := BuiltAt
	defer func() { BuiltAt = oldBuiltAt }()

	for _, value := range []string{"", "unknown", "not-a-time"} {
		BuiltAt = value
		if got := Current().BuiltAt; got != "" {
			t.Fatalf("BuiltAt %q should be omitted, got %q", value, got)
		}
	}
}
