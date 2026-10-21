package runtime

import (
	"encoding/json"
	"net/http"
	"strings"

	"smartgate/internal/redact"
)

type apiEnvelope struct {
	OK    bool `json:"ok"`
	Data  any  `json:"data,omitempty"`
	Error any  `json:"error,omitempty"`
}

type apiErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func writeAPIData(w http.ResponseWriter, code int, data any) {
	writeJSON(w, code, apiEnvelope{OK: true, Data: data})
}

func writeAPIError(w http.ResponseWriter, code int, msg string) {
	writeAPIErrorCode(w, code, apiErrorCodeForStatus(code), sanitizeAPIMessage(msg), nil)
}

func writeAPIErrorData(w http.ResponseWriter, code int, msg string, data any) {
	writeAPIErrorCode(w, code, apiErrorCodeForStatus(code), sanitizeAPIMessage(msg), data)
}

func writeAPIErrorCode(w http.ResponseWriter, status int, code, message string, details any) {
	if strings.TrimSpace(code) == "" {
		code = apiErrorCodeForStatus(status)
	}
	writeJSON(w, status, apiEnvelope{OK: false, Error: apiErrorPayload{Code: code, Message: sanitizeAPIMessage(message), Details: sanitizeAPIDetails(details)}})
}

// sanitizeAPIDetails is a final serialization boundary for error details.
// Handler-specific DTOs may still provide richer, already-safe summaries.
func sanitizeAPIDetails(value any) any {
	switch item := value.(type) {
	case nil:
		return nil
	case string:
		return sanitizeAPIMessage(item)
	case bool, float64, float32, int, int64, int32, uint, uint64, uint32, json.Number:
		return item
	case []string:
		out := make([]string, len(item))
		for i, entry := range item {
			out[i] = sanitizeAPIMessage(entry)
		}
		return out
	case []any:
		out := make([]any, len(item))
		for i, entry := range item {
			out[i] = sanitizeAPIDetails(entry)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, entry := range item {
			if sensitiveDisplayPath(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = sanitizeAPIDetails(entry)
		}
		return out
	default:
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil
		}
		return sanitizeAPIDetails(decoded)
	}
}

func apiErrorCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "validation_failed"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusLocked:
		return "locked"
	case http.StatusPreconditionRequired:
		return "precondition_required"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "upstream_failed"
	case http.StatusInsufficientStorage:
		return "insufficient_storage"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "request_failed"
	}
}

func sanitizeAPIMessage(message string) string {
	return redact.Message(message)
}
