package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case data := <-channel:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
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
	result, err := a.db.ExecContext(r.Context(), `UPDATE generation_tasks SET status='cancelled', completed_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('pending','running')`, id)
	if err != nil {
		writeError(w, 500, "cancel task failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, 409, "task is not cancellable")
		return
	}
	a.publish(id, map[string]any{"task_id": id, "status": "cancelled"})
	writeJSON(w, 200, map[string]any{"id": id, "status": "cancelled"})
}

func (a *app) retryTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid task id")
		return
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE generation_tasks SET status='pending', failed_count=0, completed_at=NULL WHERE id=? AND status='failed'`, id)
	if err != nil {
		writeError(w, 500, "retry task failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, 409, "only failed tasks can be retried")
		return
	}
	a.jobs <- id
	writeJSON(w, 202, map[string]any{"id": id, "status": "pending"})
}
