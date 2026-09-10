package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type directTaskInput struct {
	WorkflowID     int64          `json:"workflow_id"`
	PositivePrompt string         `json:"positive_prompt"`
	NegativePrompt string         `json:"negative_prompt"`
	Parameters     map[string]any `json:"parameters"`
}

func (a *app) createDirectTask(w http.ResponseWriter, r *http.Request) {
	var input directTaskInput
	if err := decodeJSON(r, &input); err != nil || input.WorkflowID <= 0 || strings.TrimSpace(input.PositivePrompt) == "" {
		writeError(w, http.StatusBadRequest, "workflow_id and positive_prompt are required")
		return
	}
	var workflowExists int
	if err := a.db.QueryRowContext(r.Context(), "SELECT 1 FROM workflows WHERE id=? AND enabled=1", input.WorkflowID).Scan(&workflowExists); err != nil {
		writeError(w, http.StatusBadRequest, "workflow not found or disabled")
		return
	}
	comfyURL, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read ComfyUI URL failed")
		return
	}
	parameters, _ := json.Marshal(map[string]any{"positive_prompt": input.PositivePrompt, "negative_prompt": input.NegativePrompt, "parameters": input.Parameters})
	now := time.Now()
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create task failed")
		return
	}
	result, err := tx.ExecContext(r.Context(), `INSERT INTO generation_tasks(source_type, workflow_id, comfyui_url, parameters_json, created_at) VALUES('direct',?,?,?,?)`, input.WorkflowID, comfyURL, string(parameters), now)
	if err == nil {
		var taskID int64
		taskID, err = result.LastInsertId()
		if err == nil {
			_, err = tx.ExecContext(r.Context(), `INSERT INTO generation_items(task_id, positive_prompt, negative_prompt) VALUES(?,?,?)`, taskID, input.PositivePrompt, input.NegativePrompt)
		}
	}
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "create task failed")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "commit task failed")
		return
	}
	taskID := resultID(result)
	a.jobs <- taskID
	writeJSON(w, http.StatusAccepted, map[string]any{"id": taskID, "status": "pending", "comfyui_url": comfyURL})
}

func resultID(result interface{ LastInsertId() (int64, error) }) int64 {
	id, _ := result.LastInsertId()
	return id
}

func (a *app) listTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, source_type, workflow_id, comfyui_url, status, total_count, success_count, failed_count, created_at FROM generation_tasks ORDER BY id DESC LIMIT 100`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query tasks failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, workflowID, total, success, failed int
		var source, comfyURL, status, created string
		if err := rows.Scan(&id, &source, &workflowID, &comfyURL, &status, &total, &success, &failed, &created); err != nil {
			writeError(w, http.StatusInternalServerError, "read task failed")
			return
		}
		items = append(items, map[string]any{"id": id, "source_type": source, "workflow_id": workflowID, "comfyui_url": comfyURL, "status": status, "total_count": total, "success_count": success, "failed_count": failed, "created_at": created})
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) getTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	var source, comfyURL, parameters, status, created string
	var workflowID, total, success, failed int
	err = a.db.QueryRowContext(r.Context(), `SELECT source_type, workflow_id, comfyui_url, parameters_json, status, total_count, success_count, failed_count, created_at FROM generation_tasks WHERE id=?`, id).Scan(&source, &workflowID, &comfyURL, &parameters, &status, &total, &success, &failed, &created)
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "source_type": source, "workflow_id": workflowID, "comfyui_url": comfyURL, "parameters": json.RawMessage(parameters), "status": status, "total_count": total, "success_count": success, "failed_count": failed, "created_at": created})
}
