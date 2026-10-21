package netgate

import (
	"fmt"
	"strings"
)

type OwnerRegistry struct {
	prefix string
}

func NewOwnerRegistry(prefix string) *OwnerRegistry {
	return &OwnerRegistry{prefix: prefix}
}

func (r *OwnerRegistry) Tag(kind, id string) string {
	kind = normalize(kind)
	id = normalize(id)
	return fmt.Sprintf("%s:%s:%s", r.prefix, kind, id)
}

func (r *OwnerRegistry) Comment(kind, id string) string {
	return "owner=" + r.Tag(kind, id)
}

func normalize(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "unknown"
	}
	return strings.NewReplacer(" ", "-", "/", "-", ":", "-").Replace(s)
}
