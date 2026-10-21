package runtime

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"smartgate/internal/state"
)

func TestEventsHandlerStreamsStatusEvent(t *testing.T) {
	server := NewServer(Config{
		ListenAddr: "127.0.0.1:3000",
		Status:     NewStatus("127.0.0.1:3000", state.RuntimeState{ConfigEpoch: 7}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := newFlushRecorder()
	done := make(chan struct{})
	go func() {
		server.http.Handler.ServeHTTP(resp, req)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		body := resp.String()
		if strings.Contains(body, "event: status") && strings.Contains(body, `"config_epoch":7`) {
			cancel()
			<-done
			if got := resp.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Fatalf("content-type = %q", got)
			}
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("status event not streamed: %q", resp.String())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

type flushRecorder struct {
	header http.Header
	body   bytes.Buffer
	mu     sync.Mutex
	code   int
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: http.Header{}}
}

func (r *flushRecorder) Header() http.Header {
	return r.header
}

func (r *flushRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = code
	}
}

func (r *flushRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}

func (r *flushRecorder) Flush() {}

func (r *flushRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}
