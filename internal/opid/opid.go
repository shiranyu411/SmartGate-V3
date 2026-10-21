package opid

import (
	"fmt"
	"strings"
)

const MaxLength = 128

func Clean(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("op id is required")
	}
	if len(id) > MaxLength {
		return "", fmt.Errorf("op id too long: %d > %d", len(id), MaxLength)
	}
	if id == "." || id == ".." || strings.TrimSpace(id) != id {
		return "", fmt.Errorf("op id contains unsafe characters")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return "", fmt.Errorf("op id contains unsafe characters")
		}
	}
	return id, nil
}

func Validate(id string) error {
	_, err := Clean(id)
	return err
}
