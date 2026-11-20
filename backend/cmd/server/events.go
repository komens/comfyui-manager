package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (a *app) taskEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid task id")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}
	channel := make(chan []byte, 8)
	a.eventsMu.Lock()
	if a.events[id] == nil {
		a.events[id] = make(map[chan []byte]struct{})
	}
	a.events[id][channel] = struct{}{}
	a.eventsMu.Unlock()
	defer func() {
		a.eventsMu.Lock()
		delete(a.events[id], channel)
		if len(a.events[id]) == 0 {
			delete(a.events, id)
		}
		a.eventsMu.Unlock()
		close(channel)
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case data := <-channel:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *app) publish(taskID int64, event any) {
	data, _ := json.Marshal(event)
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	for channel := range a.events[taskID] {
		select {
		case channel <- data:
		default:
		}
	}
}

func (a *app) cancelTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid task id")
		return
	}
	if err := a.cancelTaskByID(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, errTaskNotFound):
			writeError(w, 404, "task not found")
		case errors.Is(err, errTaskNotCancellable):
			writeError(w, 409, "task is not cancellable")
		default:
			writeError(w, 500, "cancel task failed")
		}
		return
	}
	var warning string
	_ = a.db.QueryRowContext(r.Context(), `SELECT error_message FROM generation_items
		WHERE task_id=? AND status='cancelled' AND error_message!='' LIMIT 1`, id).Scan(&warning)
	writeJSON(w, 200, map[string]any{"id": id, "status": "cancelled", "remote_warning": warning})
}

func (a *app) retryTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid task id")
		return
	}
	if err := a.retryTaskByID(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, errTaskNotFound):
			writeError(w, 404, "task not found")
		case errors.Is(err, errTaskNotFailed):
			writeError(w, 409, "task has no retryable failed items")
		case errors.Is(err, errTaskWorkflowGone):
			writeError(w, 409, "工作流已删除，无法重试该任务")
		default:
			writeError(w, 500, "retry task failed")
		}
		return
	}
	writeJSON(w, 202, map[string]any{"id": id, "status": "pending"})
}
