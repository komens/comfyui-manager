package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type directTaskInput struct {
	WorkflowID     int64          `json:"workflow_id"`
	PositivePrompt string         `json:"positive_prompt"`
	Title          string         `json:"title"`
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
	title := input.Title
	if title == "" {
		title = truncate(input.PositivePrompt, 50)
	}
	now := time.Now()
	parameters, _ := json.Marshal(map[string]any{"positive_prompt": input.PositivePrompt, "parameters": input.Parameters})
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create task failed")
		return
	}
	// 单条提示词同样入库到 prompts 表（group_name 为手动提交）
	promptResult, err := tx.ExecContext(r.Context(),
		`INSERT INTO prompts(title, description, positive_prompt, group_name, group_id, status, created_at, updated_at) VALUES(?, '', ?, '手动提交', 'manual', 'running', ?, ?)`,
		title, input.PositivePrompt, now, now)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "create prompt failed")
		return
	}
	promptID, _ := promptResult.LastInsertId()
	result, err := tx.ExecContext(r.Context(),
		`INSERT INTO generation_tasks(source_type, workflow_id, comfyui_url, parameters_json, created_at) VALUES('direct',?,?,?,?)`,
		input.WorkflowID, comfyURL, string(parameters), now)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "create task failed")
		return
	}
	taskID, _ := result.LastInsertId()
	_, err = tx.ExecContext(r.Context(),
		`INSERT INTO generation_items(task_id, prompt_id, positive_prompt) VALUES(?,?,?)`,
		taskID, promptID, input.PositivePrompt)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "create task item failed")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "commit task failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": taskID, "prompt_id": promptID, "status": "pending", "comfyui_url": comfyURL,
	})
}

func (a *app) listTasks(w http.ResponseWriter, r *http.Request) {
	where := ` WHERE 1=1`
	args := []any{}
	if status := r.URL.Query().Get("status"); status != "" {
		where += ` AND t.status=?`
		args = append(args, status)
	}
	// 统计总数
	var total int
	countArgs := make([]any, len(args))
	copy(countArgs, args)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM generation_tasks t`+where, countArgs...).Scan(&total)
	// 分页
	page, pageSize, offset := parsePagination(r)
	queryArgs := append(args, pageSize, offset)
	rows, err := a.db.QueryContext(r.Context(), `SELECT t.id, t.source_type, t.workflow_id, w.name, t.status, t.total_count, t.success_count, t.failed_count, t.created_at
		FROM generation_tasks t LEFT JOIN workflows w ON w.id=t.workflow_id`+where+` ORDER BY t.id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query tasks failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, workflowID, totalC, success, failed int
		var source, status, created string
		var workflowName sql.NullString
		if err := rows.Scan(&id, &source, &workflowID, &workflowName, &status, &totalC, &success, &failed, &created); err != nil {
			writeError(w, http.StatusInternalServerError, "read task failed")
			return
		}
		items = append(items, map[string]any{"id": id, "source_type": source, "workflow_id": workflowID,
			"workflow_name": workflowName.String, "status": status, "total_count": totalC,
			"success_count": success, "failed_count": failed, "created_at": created})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize})
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
	items := make([]map[string]any, 0)
	rows, rowsErr := a.db.QueryContext(r.Context(), `SELECT id, prompt_id, positive_prompt, COALESCE(comfy_prompt_id,''), status, error_message FROM generation_items WHERE task_id=? ORDER BY id`, id)
	if rowsErr == nil {
		defer rows.Close()
		for rows.Next() {
			var itemID int
			var promptID sql.NullInt64
			var positive, promptIDStr, itemStatus, itemError string
			if err := rows.Scan(&itemID, &promptID, &positive, &promptIDStr, &itemStatus, &itemError); err == nil {
				items = append(items, map[string]any{"id": itemID, "prompt_id": promptID.Int64,
					"positive_prompt": positive, "comfy_prompt_id": promptIDStr,
					"status": itemStatus, "error_message": itemError})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "source_type": source, "workflow_id": workflowID,
		"comfyui_url": comfyURL, "parameters": json.RawMessage(parameters), "status": status,
		"total_count": total, "success_count": success, "failed_count": failed,
		"created_at": created, "items": items})
}

func (a *app) deleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid task id")
		return
	}
	// 只允许删除非 running 状态的任务
	var status string
	if err := a.db.QueryRow(`SELECT status FROM generation_tasks WHERE id=?`, id).Scan(&status); err != nil {
		writeError(w, 404, "task not found")
		return
	}
	if status == "running" {
		writeError(w, 409, "cannot delete running task, cancel it first")
		return
	}
	// 删除关联数据（使用事务）
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, 500, "delete task failed")
		return
	}
	_, _ = tx.Exec(`DELETE FROM images WHERE generation_item_id IN (SELECT id FROM generation_items WHERE task_id=?)`, id)
	_, _ = tx.Exec(`DELETE FROM generation_items WHERE task_id=?`, id)
	_, _ = tx.Exec(`DELETE FROM generation_tasks WHERE id=?`, id)
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		writeError(w, 500, "delete task failed")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "deleted"})
}
