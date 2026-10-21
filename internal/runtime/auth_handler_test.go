package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smartgate/internal/auth"
)

func TestAuthHandlersSetupLoginAndMiddleware(t *testing.T) {
	authStore := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	server := NewServer(Config{
		ListenAddr:   "127.0.0.1:3000",
		Auth:         authStore,
		AuthRequired: true,
	})

	checkReq := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	checkResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(checkResp, checkReq)
	if checkResp.Code != http.StatusOK {
		t.Fatalf("check status = %d body=%s", checkResp.Code, checkResp.Body.String())
	}
	checkData := decodeEnvelopeData(t, checkResp.Body.Bytes())
	if checkData["needs_setup"] != true {
		t.Fatalf("expected needs_setup, got %+v", checkData)
	}

	protectedReq := httptest.NewRequest(http.MethodGet, "/api/runtime/locks", nil)
	protectedResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(protectedResp, protectedReq)
	if protectedResp.Code != http.StatusUnauthorized {
		t.Fatalf("protected status = %d body=%s", protectedResp.Code, protectedResp.Body.String())
	}

	setupReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", bytes.NewReader(mustJSON(t, map[string]string{"password": "test-password"})))
	setupResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(setupResp, setupReq)
	if setupResp.Code != http.StatusOK {
		t.Fatalf("setup status = %d body=%s", setupResp.Code, setupResp.Body.String())
	}
	setupData := decodeEnvelopeData(t, setupResp.Body.Bytes())
	token, ok := setupData["token"].(string)
	if !ok || token == "" {
		t.Fatalf("setup token missing: %+v", setupData)
	}

	protectedReq = httptest.NewRequest(http.MethodGet, "/api/runtime/locks", nil)
	protectedReq.Header.Set("Authorization", "Bearer "+token)
	protectedResp = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(protectedResp, protectedReq)
	if protectedResp.Code != http.StatusOK {
		t.Fatalf("authorized protected status = %d body=%s", protectedResp.Code, protectedResp.Body.String())
	}
}

func TestPasswordChangeKeepsCurrentSession(t *testing.T) {
	authStore := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	server := NewServer(Config{
		ListenAddr:   "127.0.0.1:3000",
		Auth:         authStore,
		AuthRequired: true,
	})

	setupReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", bytes.NewReader(mustJSON(t, map[string]string{"password": "test-password"})))
	setupResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(setupResp, setupReq)
	if setupResp.Code != http.StatusOK {
		t.Fatalf("setup status = %d body=%s", setupResp.Code, setupResp.Body.String())
	}
	token := decodeEnvelopeData(t, setupResp.Body.Bytes())["token"].(string)

	changeReq := httptest.NewRequest(http.MethodPut, "/api/auth/password", bytes.NewReader(mustJSON(t, map[string]string{
		"old_password": "test-password",
		"new_password": "76545468",
	})))
	changeReq.Header.Set("Authorization", "Bearer "+token)
	changeResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(changeResp, changeReq)
	if changeResp.Code != http.StatusOK {
		t.Fatalf("password change status = %d body=%s", changeResp.Code, changeResp.Body.String())
	}

	checkReq := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	checkReq.Header.Set("Authorization", "Bearer "+token)
	checkResp := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(checkResp, checkReq)
	if checkResp.Code != http.StatusOK {
		t.Fatalf("check status = %d body=%s", checkResp.Code, checkResp.Body.String())
	}
	checkData := decodeEnvelopeData(t, checkResp.Body.Bytes())
	if checkData["authenticated"] != true {
		t.Fatalf("current session should remain authenticated after password change: %+v", checkData)
	}
}

func TestAuthMiddlewareOnlyAllowsQueryTokenForEvents(t *testing.T) {
	authStore := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	server := NewServer(Config{ListenAddr: "127.0.0.1:3000", Auth: authStore, AuthRequired: true})
	setup := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(setup, httptest.NewRequest(http.MethodPost, "/api/auth/setup", bytes.NewReader(mustJSON(t, map[string]string{"password": "test-password"}))))
	token := decodeEnvelopeData(t, setup.Body.Bytes())["token"].(string)

	rejected := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/api/runtime/status?token="+token, nil))
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("query token on ordinary API status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(rejected.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	payload, ok := envelope.Error.(map[string]any)
	if !ok || payload["code"] != "query_token_not_allowed" {
		t.Fatalf("unexpected query-token rejection: %+v", envelope.Error)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventsReq := httptest.NewRequest(http.MethodGet, "/api/events?token="+token, nil).WithContext(ctx)
	recorder := newFlushRecorder()
	done := make(chan struct{})
	go func() {
		server.http.Handler.ServeHTTP(recorder, eventsReq)
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for !strings.Contains(recorder.String(), "event: status") {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("SSE query token was not accepted: %q", recorder.String())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	<-done
}

func decodeEnvelopeData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, string(body))
	}
	if !env.OK {
		t.Fatalf("envelope error: %+v", env)
	}
	data, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("envelope data is %T: %+v", env.Data, env.Data)
	}
	return data
}
