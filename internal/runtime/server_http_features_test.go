package runtime

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"smartgate/internal/auth"
)

func TestCORSMiddlewareHandlesPreflightBeforeAuth(t *testing.T) {
	server := NewServer(Config{
		ListenAddr:   "127.0.0.1:3000",
		CORSOrigins:  []string{"http://localhost:5173"},
		Auth:         auth.NewStore(filepath.Join(t.TempDir(), "auth.json")),
		AuthRequired: true,
	})
	req := httptest.NewRequest(http.MethodOptions, "/api/runtime/status", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	resp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("allow-origin = %q", got)
	}
}

func TestVersionEndpointReturnsEnvelope(t *testing.T) {
	server := NewServer(Config{ListenAddr: "127.0.0.1:3000"})
	resp := serve(t, server, http.MethodGet, "/api/version", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("version status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if data["name"] != "smartgate" || data["version"] == "" {
		t.Fatalf("unexpected version data: %+v", data)
	}
	for _, legacyField := range []string{"core", "core_min"} {
		if _, exists := data[legacyField]; exists {
			t.Fatalf("version data contains retired field %q: %+v", legacyField, data)
		}
	}
}

func TestRuntimeStatusEndpointReturnsEnvelope(t *testing.T) {
	server := NewServer(Config{ListenAddr: "127.0.0.1:3000"})
	resp := serve(t, server, http.MethodGet, "/api/runtime/status", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("runtime status = %d body=%s", resp.Code, resp.Body.String())
	}
	data := decodeEnvelopeData(t, resp.Body.Bytes())
	if data["phase"] == "" || data["listen_addr"] != "127.0.0.1:3000" {
		t.Fatalf("unexpected runtime status data: %+v", data)
	}
}

func TestStaticHandlerServesIndexAndDoesNotShadowAPI(t *testing.T) {
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("smartgate ui"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "asset.txt"), []byte("asset"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		StaticDir:  staticDir,
	})

	resp := serve(t, server, http.MethodGet, "/asset.txt", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "asset" {
		t.Fatalf("asset response = %d %q", resp.Code, resp.Body.String())
	}
	resp = serve(t, server, http.MethodGet, "/app/settings", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "smartgate ui" {
		t.Fatalf("spa fallback response = %d %q", resp.Code, resp.Body.String())
	}
	resp = serve(t, server, http.MethodGet, "/api/runtime/status", nil)
	if resp.Code != http.StatusOK || resp.Body.String() == "smartgate ui" {
		t.Fatalf("api should not be shadowed by static handler: %d %q", resp.Code, resp.Body.String())
	}
	resp = serve(t, server, http.MethodGet, "/api", nil)
	if resp.Code != http.StatusNotFound || resp.Body.String() == "smartgate ui" {
		t.Fatalf("bare api should not be shadowed by static handler: %d %q", resp.Code, resp.Body.String())
	}
}
