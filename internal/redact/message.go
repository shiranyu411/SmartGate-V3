package redact

import (
	"regexp"
	"strings"
)

var (
	messageURLRE    = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	messageSecretRE = regexp.MustCompile(`(?i)(password|passwd|token|secret|credential|authorization|uuid|private[_-]?key|psk)=([^\s,;]+)`)
	messagePathRE   = regexp.MustCompile(`(^|[\s:=(])((?:/[A-Za-z0-9._@%+=:,~-]+)+)`)
)

// Message removes URL credentials, paths, queries, and key=value secrets while
// retaining enough URL identity for diagnostics.
func Message(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return message
	}
	message = messageURLRE.ReplaceAllStringFunc(message, func(raw string) string {
		parts := strings.SplitN(raw, "://", 2)
		if len(parts) != 2 {
			return "[REDACTED_URL]"
		}
		host := parts[1]
		if slash := strings.IndexAny(host, "/?#"); slash >= 0 {
			host = host[:slash]
		}
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return parts[0] + "://" + host
	})
	return messageSecretRE.ReplaceAllString(message, "$1=[REDACTED]")
}

// MessageWithPaths additionally removes absolute filesystem paths. URL
// redaction runs first so the scheme and host that Message retains stay intact.
func MessageWithPaths(message string) string {
	return messagePathRE.ReplaceAllString(Message(message), "$1[REDACTED_PATH]")
}
