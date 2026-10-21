package runtime

import (
	"net/http"
	"strings"
	"testing"

	"smartgate/internal/coredesc"
	"smartgate/internal/smartnode"
	"smartgate/internal/worker"
)

func TestWorkerHandlersExposeKnownStoppedWorkersAndRejectUnknown(t *testing.T) {
	primaryWorker := runtimeTestWorkerDescriptors[runtimeCoreA].WorkerID(1)
	secondaryWorker := runtimeTestWorkerDescriptors[runtimeCoreB].WorkerID(1)
	manager, err := worker.NewManager(
		&workerHandlerAdapter{core: runtimeCoreA, workerID: primaryWorker},
		&workerHandlerAdapter{core: runtimeCoreB, workerID: secondaryWorker},
	)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{ListenAddr: "127.0.0.1:3000", Workers: manager})
	response := serve(t, server, http.MethodGet, "/api/workers", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("workers status=%d body=%s", response.Code, response.Body.String())
	}
	data := decodeEnvelopeData(t, response.Body.Bytes())
	workers, ok := data["workers"].([]any)
	if !ok || len(workers) != 2 {
		t.Fatalf("expected both known workers: %+v", data)
	}
	for _, rawWorker := range workers {
		status, ok := rawWorker.(map[string]any)
		if !ok {
			t.Fatalf("worker status is not an object: %+v", rawWorker)
		}
		if executors, ok := status["executors"].([]any); !ok || len(executors) != 0 {
			t.Fatalf("stopped worker executors must be an empty array: %+v", status)
		}
		if pools, ok := status["pools"].([]any); !ok || len(pools) != 0 {
			t.Fatalf("stopped worker pools must be an empty array: %+v", status)
		}
	}
	events := serve(t, server, http.MethodGet, "/api/workers/"+secondaryWorker+"/events", nil)
	if events.Code != http.StatusOK {
		t.Fatalf("worker events status=%d body=%s", events.Code, events.Body.String())
	}
	eventData := decodeEnvelopeData(t, events.Body.Bytes())
	if items, ok := eventData["events"].([]any); !ok || len(items) != 0 {
		t.Fatalf("worker events must be an empty array: %+v", eventData)
	}
	missing := serve(t, server, http.MethodGet, "/api/workers/missing/events", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing worker status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestWorkerResponseSanitizesEventMessages(t *testing.T) {
	primaryWorker := runtimeTestWorkerDescriptors[runtimeCoreA].WorkerID(1)
	statuses := []worker.Status{{
		ID: primaryWorker, Core: runtimeCoreA, State: worker.StateFailed,
		LastEvent: &worker.Event{Worker: primaryWorker, Kind: "install_failed", State: worker.StateFailed, Message: "fetch https://user:pass@example.com/sub?token=plain password=hunter2"},
	}}
	safeStatuses := sanitizeWorkerStatuses(statuses)
	encoded := string(mustJSON(t, safeStatuses))
	for _, secret := range []string{"user:pass", "/sub?token=plain", "hunter2"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("worker status leaked %q: %s", secret, encoded)
		}
	}
	if safeStatuses[0].LastEvent == nil || safeStatuses[0].LastEvent.Message != "Worker 安装失败" || safeStatuses[0].LastEvent.Diagnostic == "" {
		t.Fatalf("worker failure did not expose Chinese text plus a diagnostic: %+v", safeStatuses)
	}

	events := sanitizeWorkerEvents([]worker.Event{{Kind: "future_event", Message: "token=abc123"}})
	if strings.Contains(string(mustJSON(t, events)), "abc123") {
		t.Fatalf("worker event leaked token: %+v", events)
	}
	if events[0].Message != "Worker 状态已更新" {
		t.Fatalf("unknown worker event exposed its machine value: %+v", events[0])
	}
}

type workerHandlerAdapter struct {
	worker.Adapter
	core     smartnode.CoreID
	workerID string
}

func (adapter *workerHandlerAdapter) Core() smartnode.CoreID { return adapter.core }

func (adapter *workerHandlerAdapter) Descriptor() coredesc.Descriptor {
	return coredesc.Descriptor{ID: adapter.core, Naming: coredesc.Naming{LegacyWorkerAlias: adapter.workerID}}
}
