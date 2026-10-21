package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"smartgate/internal/commitlane"
	"smartgate/internal/rescue"
	"smartgate/internal/version"
)

func registerHandlers(mux *http.ServeMux, statusProvider func() Status, rescueMgr *rescue.Manager, recovery *commitlane.RecoveryOrchestrator) {
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeAPIData(w, http.StatusOK, version.Current())
	})
	mux.HandleFunc("/api/runtime/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeAPIData(w, http.StatusOK, statusProvider())
	})
	mux.HandleFunc("/api/runtime/rescue/enter", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := rescueMgr.Enter(r.Context(), rescue.ReasonManual); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAPIData(w, http.StatusAccepted, map[string]string{"status": "rescue entered"})
	})
	mux.HandleFunc("/api/runtime/rescue/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		transitionCtx, transitionCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
		defer transitionCancel()
		recoverPending := func() error {
			recoveryCtx, cancel := context.WithTimeout(transitionCtx, 30*time.Second)
			defer cancel()
			if recovery == nil {
				return commitlane.ErrRecoveryRequired
			}
			return recovery.RecoverForRescueClear(recoveryCtx)
		}
		if err := rescueMgr.ClearWithRecovery(transitionCtx, recoverPending); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAPIData(w, http.StatusAccepted, map[string]string{"status": "rescue cleared"})
	})
	mux.HandleFunc("/api/runtime/recover", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if recovery == nil {
			writeAPIError(w, http.StatusInternalServerError, "recovery orchestrator is not configured")
			return
		}
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if err := recovery.Recover(recoveryCtx); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAPIData(w, http.StatusOK, map[string]string{"status": "recovered"})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
