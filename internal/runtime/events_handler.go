package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultEventsInterval = 2 * time.Second

type eventsAPI struct {
	statusProvider func() Status
	interval       time.Duration
}

func registerEventHandlers(mux *http.ServeMux, statusProvider func() Status) {
	api := eventsAPI{statusProvider: statusProvider, interval: defaultEventsInterval}
	mux.HandleFunc("/api/events", api.events)
}

func (api eventsAPI) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	interval := api.interval
	if interval <= 0 {
		interval = defaultEventsInterval
	}
	if err := writeSSE(w, "status", api.eventPayload()); err != nil {
		return
	}
	flusher.Flush()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := writeSSE(w, "status", api.eventPayload()); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (api eventsAPI) eventPayload() map[string]any {
	return map[string]any{
		"time":    time.Now().UTC(),
		"runtime": api.statusProvider(),
	}
}

func writeSSE(w http.ResponseWriter, event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
		return err
	}
	return nil
}
