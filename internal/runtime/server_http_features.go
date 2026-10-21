package runtime

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"smartgate/internal/state"
)

func requestBodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, state.MaxControlStateJSONBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func corsMiddleware(next http.Handler, origins []string) http.Handler {
	allowed := normalizeCORSOrigins(origins)
	if len(allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && corsOriginAllowed(allowed, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeCORSOrigins(origins []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, raw := range origins {
		for _, item := range strings.Split(raw, ",") {
			origin := strings.TrimSpace(item)
			if origin == "" {
				continue
			}
			if _, ok := seen[origin]; ok {
				continue
			}
			seen[origin] = struct{}{}
			out = append(out, origin)
		}
	}
	return out
}

func corsOriginAllowed(allowed []string, origin string) bool {
	for _, item := range allowed {
		if item == "*" || item == origin {
			return true
		}
	}
	return false
}

func registerStaticHandler(mux *http.ServeMux, staticDir string) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) || r.URL.Path == "/healthz" {
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(filepath.Clean(r.URL.Path), string(filepath.Separator))
		if path == "." || path == "" {
			path = "index.html"
		}
		fullPath := filepath.Join(staticDir, path)
		if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
			http.ServeFile(w, r, fullPath)
			return
		}
		indexPath := filepath.Join(staticDir, "index.html")
		if _, err := os.Stat(indexPath); err == nil {
			http.ServeFile(w, r, indexPath)
			return
		}
		http.NotFound(w, r)
	})
}

func isAPIPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}
