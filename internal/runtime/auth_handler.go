package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"smartgate/internal/auth"
)

type authAPI struct {
	store *auth.Store
}

type passwordPayload struct {
	Password    string `json:"password"`
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func registerAuthHandlers(mux *http.ServeMux, store *auth.Store) {
	api := authAPI{store: store}
	mux.HandleFunc("/api/auth/check", api.check)
	mux.HandleFunc("/api/auth/setup", api.setup)
	mux.HandleFunc("/api/auth/login", api.login)
	mux.HandleFunc("/api/auth/logout", api.logout)
	mux.HandleFunc("/api/auth/password", api.password)
}

func (api authAPI) check(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	result, err := api.store.Check(bearerToken(r))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]any{
		"needs_setup":   result.NeedsSetup,
		"authenticated": result.Authenticated,
	})
}

func (api authAPI) setup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	payload, ok := decodePasswordPayload(w, r)
	if !ok {
		return
	}
	token, err := api.store.Setup(payload.Password)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	writeAPIData(w, http.StatusOK, map[string]string{"token": token})
}

func (api authAPI) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	payload, ok := decodePasswordPayload(w, r)
	if !ok {
		return
	}
	token, err := api.store.Login(payload.Password)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	writeAPIData(w, http.StatusOK, map[string]string{"token": token})
}

func (api authAPI) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := api.store.Logout(bearerToken(r)); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIData(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func (api authAPI) password(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	check, err := api.store.Check(bearerToken(r))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !check.Authenticated {
		writeAPIError(w, http.StatusUnauthorized, "invalid or expired login credential")
		return
	}
	var payload passwordPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := api.store.ChangePassword(payload.OldPassword, payload.NewPassword, bearerToken(r)); err != nil {
		writeAuthErr(w, err)
		return
	}
	writeAPIData(w, http.StatusOK, map[string]string{"status": "password changed"})
}

func decodePasswordPayload(w http.ResponseWriter, r *http.Request) (passwordPayload, bool) {
	var payload passwordPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return passwordPayload{}, false
	}
	return payload, true
}

func authMiddleware(next http.Handler, store *auth.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/api/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		if !isAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// Query-string tokens are reserved for the browser EventSource SSE
		// endpoint. Keeping this exception here prevents ordinary API URLs from
		// becoming bearer-token carriers that leak through history, proxies, or
		// access logs.
		if r.URL.Query().Get("token") != "" && r.URL.Path != "/api/events" {
			writeAPIErrorCode(w, http.StatusUnauthorized, "query_token_not_allowed", "query-string tokens are only allowed for /api/events", nil)
			return
		}
		check, err := store.Check(bearerToken(r))
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if check.NeedsSetup || !check.Authenticated {
			writeAPIError(w, http.StatusUnauthorized, "invalid or expired login credential")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authz) > 7 && strings.EqualFold(authz[:7], "Bearer ") {
		return strings.TrimSpace(authz[7:])
	}
	if r.URL.Path == "/api/events" {
		return strings.TrimSpace(r.URL.Query().Get("token"))
	}
	return ""
}

func writeAuthErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrTooManyAttempts):
		writeAPIError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
	case errors.Is(err, auth.ErrInvalidCredential):
		writeAPIError(w, http.StatusUnauthorized, "invalid password")
	case errors.Is(err, auth.ErrNeedsSetup):
		writeAPIError(w, http.StatusPreconditionRequired, "auth setup required")
	case errors.Is(err, auth.ErrAlreadySetup):
		writeAPIError(w, http.StatusConflict, "auth already setup")
	default:
		writeAPIError(w, http.StatusBadRequest, err.Error())
	}
}
