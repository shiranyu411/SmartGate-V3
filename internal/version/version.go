package version

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuiltAt   = "unknown"
	GoVersion = runtime.Version()
)

type Info struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuiltAt   string `json:"built_at"`
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

func Current() Info {
	return Info{
		Name:      "smartgate",
		Version:   clean(Version, "dev"),
		Commit:    clean(Commit, "unknown"),
		BuiltAt:   cleanBuiltAt(BuiltAt),
		GoVersion: GoVersion,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
}

func Line() string {
	info := Current()
	return fmt.Sprintf("%s %s commit=%s built_at=%s %s/%s %s", info.Name, info.Version, info.Commit, info.BuiltAt, info.GOOS, info.GOARCH, info.GoVersion)
}

func JSON() ([]byte, error) {
	return json.MarshalIndent(Current(), "", "  ")
}

func clean(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func cleanBuiltAt(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "unknown") {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return ""
	}
	return value
}
