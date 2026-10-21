package redact

import (
	"strings"
	"testing"
)

func TestMessageRedactsURLsAndSecrets(t *testing.T) {
	message := Message(` request https://user:pass@example.com/private?token=plain password=hunter2 `)
	for _, secret := range []string{"user:pass", "/private?token=plain", "hunter2"} {
		if strings.Contains(message, secret) {
			t.Fatalf("message leaked %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "https://example.com") || !strings.Contains(message, "password=[REDACTED]") {
		t.Fatalf("safe URL identity or redaction marker missing: %s", message)
	}
}

func TestMessageWithPathsRedactsFilesystemPathsWithoutDamagingURLs(t *testing.T) {
	message := MessageWithPaths(`open /etc/smartgate/state.json: fetch https://example.com/sub`)
	if strings.Contains(message, "/etc/smartgate/state.json") || !strings.Contains(message, "[REDACTED_PATH]") {
		t.Fatalf("filesystem path was not redacted: %s", message)
	}
	if !strings.Contains(message, "https://example.com") {
		t.Fatalf("safe URL identity was damaged: %s", message)
	}
}
