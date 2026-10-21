package runtime

import (
	"encoding/json"
	"errors"
	"net/http"

	"smartgate/internal/backup"
	"smartgate/internal/commitlane"
	"smartgate/internal/state"
)

type backupAPI struct {
	layout state.Layout
	txn    *commitlane.BackupTxn
}

type backupCreatePayload struct {
	OpID string `json:"op_id"`
	Kind string `json:"kind,omitempty"`
}

type backupRestorePayload struct {
	OpID string `json:"op_id"`
	Path string `json:"path"`
}

func registerBackupHandlers(mux *http.ServeMux, layout state.Layout, txn *commitlane.BackupTxn) {
	api := backupAPI{layout: layout, txn: txn}
	mux.HandleFunc("/api/backups/create", api.create)
	mux.HandleFunc("/api/backups/restore", api.restore)
}

func (api backupAPI) create(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload backupCreatePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if payload.OpID == "" {
		writeAPIError(w, http.StatusBadRequest, "op_id is required")
		return
	}
	record, err := api.txn.Create(r.Context(), payload.OpID, payload.Kind)
	if err != nil {
		if errors.Is(err, commitlane.ErrRescueActive) || errors.Is(err, commitlane.ErrRecoveryRequired) {
			writeAPIError(w, http.StatusLocked, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAPIData(w, http.StatusAccepted, record)
}

func (api backupAPI) restore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload backupRestorePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if payload.OpID == "" {
		writeAPIError(w, http.StatusBadRequest, "op_id is required")
		return
	}
	parsed, err := backup.Parse(r.Context(), api.layout, payload.Path, backup.DefaultMaxFileBytes)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := api.txn.Restore(r.Context(), commitlane.BackupRestoreRequest{OpID: payload.OpID, Payload: parsed})
	if err != nil {
		if errors.Is(err, commitlane.ErrRescueActive) || errors.Is(err, commitlane.ErrRecoveryRequired) {
			writeAPIError(w, http.StatusLocked, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAPIData(w, http.StatusAccepted, result)
}
