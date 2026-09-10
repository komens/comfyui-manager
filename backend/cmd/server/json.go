package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type jsonEntry struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Desc     string `json:"desc"`
	Positive string `json:"positive"`
	Negative string `json:"negative"`
	Status   string `json:"status"`
}
type jsonDocument struct {
	Entries []jsonEntry `json:"entries"`
}

type jsonTaskRequest struct {
	JSONFileID int64          `json:"json_file_id"`
	WorkflowID int64          `json:"workflow_id"`
	EntryIDs   []int64        `json:"entry_ids"`
	Mode       string         `json:"mode"`
	Parameters map[string]any `json:"parameters"`
}

func (a *app) uploadJSON(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		writeError(w, 400, "invalid multipart form")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, "file is required")
		return
	}
	defer file.Close()
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".json") {
		writeError(w, 400, "only JSON files are supported")
		return
	}
	content, err := io.ReadAll(io.LimitReader(file, 50<<20))
	if err != nil {
		writeError(w, 400, "read file failed")
		return
	}
	entries, err := parseEntries(content)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	name := safeFilename(strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename))) + ".json"
	path := filepath.Join(a.dataDir, "json", name)
	if err := writeAtomic(path, content); err != nil {
		writeError(w, 500, "save JSON failed")
		return
	}
	now := time.Now()
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, 500, "create JSON record failed")
		return
	}
	result, err := tx.ExecContext(r.Context(), `INSERT INTO json_files(filename, storage_path, total_count, created_at, updated_at) VALUES(?,?,?,?,?)`, name, path, len(entries), now, now)
	if err == nil {
		var fileID int64
		fileID, err = result.LastInsertId()
		if err == nil {
			for _, entry := range entries {
				status := entry.Status
				if status == "" {
					status = "pending"
				}
				_, err = tx.ExecContext(r.Context(), `INSERT INTO prompt_entries(json_file_id, entry_key, title, description, positive_prompt, negative_prompt, status) VALUES(?,?,?,?,?,?,?)`, fileID, entry.ID, entry.Title, entry.Desc, entry.Positive, entry.Negative, status)
				if err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		_ = tx.Rollback()
		writeError(w, 400, "duplicate entry id or invalid JSON data")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, 500, "save JSON record failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"filename": name, "count": len(entries)})
}

func parseEntries(content []byte) ([]jsonEntry, error) {
	var doc jsonDocument
	if err := json.Unmarshal(content, &doc); err == nil && doc.Entries != nil {
		return validateEntries(doc.Entries)
	}
	var entries []jsonEntry
	if err := json.Unmarshal(content, &entries); err != nil {
		return nil, fmt.Errorf("JSON must be an object with entries or an array")
	}
	return validateEntries(entries)
}
func validateEntries(entries []jsonEntry) ([]jsonEntry, error) {
	seen := map[string]bool{}
	for _, e := range entries {
		if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.Positive) == "" {
			return nil, fmt.Errorf("each entry requires id and positive")
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("duplicate entry id: %s", e.ID)
		}
		seen[e.ID] = true
	}
	return entries, nil
}

func (a *app) listJSONFiles(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, filename, total_count, completed_count, failed_count, updated_at FROM json_files ORDER BY id DESC`)
	if err != nil {
		writeError(w, 500, "query JSON files failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, total, done, failed int
		var name, updated string
		if err := rows.Scan(&id, &name, &total, &done, &failed, &updated); err != nil {
			writeError(w, 500, "read JSON file failed")
			return
		}
		items = append(items, map[string]any{"id": id, "filename": name, "total_count": total, "completed_count": done, "failed_count": failed, "updated_at": updated})
	}
	writeJSON(w, 200, items)
}

func (a *app) listEntries(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid JSON file id")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, entry_key, title, description, positive_prompt, negative_prompt, status, completed_at FROM prompt_entries WHERE json_file_id=? ORDER BY id`, id)
	if err != nil {
		writeError(w, 500, "query entries failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var entryID int
		var key, title, desc, positive, negative, status string
		var completed any
		if err := rows.Scan(&entryID, &key, &title, &desc, &positive, &negative, &status, &completed); err != nil {
			writeError(w, 500, "read entry failed")
			return
		}
		items = append(items, map[string]any{"id": entryID, "entry_key": key, "title": title, "description": desc, "positive_prompt": positive, "negative_prompt": negative, "status": status, "completed_at": completed})
	}
	writeJSON(w, 200, items)
}

func (a *app) createJSONTasks(w http.ResponseWriter, r *http.Request) {
	var input jsonTaskRequest
	if err := decodeJSON(r, &input); err != nil || input.JSONFileID <= 0 || input.WorkflowID <= 0 {
		writeError(w, 400, "json_file_id and workflow_id are required")
		return
	}
	query := `SELECT id, positive_prompt, negative_prompt FROM prompt_entries WHERE json_file_id=?`
	args := []any{input.JSONFileID}
	if input.Mode != "all" {
		query += ` AND status != 'success'`
	}
	if len(input.EntryIDs) > 0 {
		placeholders := make([]string, len(input.EntryIDs))
		for i, id := range input.EntryIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += ` AND id IN (` + strings.Join(placeholders, ",") + `)`
	}
	rows, err := a.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, 500, "query entries failed")
		return
	}
	defer rows.Close()
	urlValue, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, 500, "read ComfyUI URL failed")
		return
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var entryID int64
		var positive, negative string
		if err := rows.Scan(&entryID, &positive, &negative); err != nil {
			writeError(w, 500, "read entry failed")
			return
		}
		payload, _ := json.Marshal(map[string]any{"positive_prompt": positive, "negative_prompt": negative, "parameters": input.Parameters, "entry_id": entryID})
		result, err := a.db.ExecContext(r.Context(), `INSERT INTO generation_tasks(source_type, workflow_id, comfyui_url, parameters_json, created_at) VALUES('json',?,?,?,?)`, input.WorkflowID, urlValue, string(payload), time.Now())
		if err != nil {
			writeError(w, 500, "create task failed")
			return
		}
		taskID, _ := result.LastInsertId()
		if _, err := a.db.ExecContext(r.Context(), `INSERT INTO generation_items(task_id, positive_prompt, negative_prompt) VALUES(?,?,?)`, taskID, positive, negative); err != nil {
			writeError(w, 500, "create task item failed")
			return
		}
		ids = append(ids, taskID)
		a.jobs <- taskID
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task_ids": ids, "count": len(ids)})
}
