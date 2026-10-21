package runtime

import (
	"net/http"
	"strings"

	"smartgate/internal/worker"
)

var workerEventMessageText = map[string]string{
	"start":                "Worker 开始启动",
	"install_failed":       "Worker 安装失败",
	"route_prepare_failed": "Worker 路由准备失败",
	"ready":                "Worker 已就绪",
	"configured":           "Worker 配置已安装",
	"drain":                "Worker 开始排空",
	"drain_complete":       "Worker 排空完成",
	"stop_failed":          "Worker 停止失败",
	"stopped":              "Worker 已停止",
	"restore_failed":       "Worker 恢复失败",
	"restored":             "Worker 已恢复",
	"rescue_stop":          "正在为救援模式停止 Worker",
	"rescue_stop_failed":   "救援模式停止 Worker 失败",
	"cutover_start":        "正在为路径切换启动 Worker",
	"cutover_start_failed": "路径切换启动 Worker 失败",
	"cutover_ready":        "路径切换 Worker 已就绪",
	"exited":               "Worker 进程异常退出",
}

func registerWorkerHandlers(mux *http.ServeMux, manager *worker.Manager) {
	mux.HandleFunc("/api/workers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeAPIData(w, http.StatusOK, map[string]any{"workers": sanitizeWorkerStatuses(manager.Statuses())})
	})
	mux.HandleFunc("/api/workers/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/workers/"), "/")
		workerID, suffix, ok := strings.Cut(path, "/")
		if !ok || suffix != "events" || workerID == "" {
			writeAPIErrorCode(w, http.StatusNotFound, "worker_endpoint_not_found", "worker endpoint not found", nil)
			return
		}
		if _, exists := manager.Status(workerID); !exists {
			writeAPIErrorCode(w, http.StatusNotFound, "worker_not_found", "Worker not found", nil)
			return
		}
		writeAPIData(w, http.StatusOK, map[string]any{"worker_id": workerID, "events": sanitizeWorkerEvents(manager.Events(workerID))})
	})
}

func sanitizeWorkerStatuses(statuses []worker.Status) []worker.Status {
	for index := range statuses {
		if statuses[index].LastEvent != nil {
			event := localizedWorkerEvent(*statuses[index].LastEvent)
			statuses[index].LastEvent = &event
		}
	}
	return statuses
}

func sanitizeWorkerEvents(events []worker.Event) []worker.Event {
	for index := range events {
		events[index] = localizedWorkerEvent(events[index])
	}
	return events
}

func localizedWorkerEvent(event worker.Event) worker.Event {
	original := sanitizeAPIMessage(event.Message)
	message, known := workerEventMessageText[event.Kind]
	if !known {
		message = "Worker 状态已更新"
	}
	event.Message = message
	event.Diagnostic = ""
	if original != "" && original != message && (!known || strings.HasSuffix(event.Kind, "_failed") || event.Kind == "exited") {
		event.Diagnostic = original
	}
	return event
}
