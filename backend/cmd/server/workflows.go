package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type workflowInput struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	WorkflowJSON json.RawMessage `json:"workflow_json"`
	Mapping      json.RawMessage `json:"mapping"`
}

func (a *app) listWorkflows(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, name, description, workflow_path, mapping_json, enabled, created_at, updated_at FROM workflows ORDER BY id DESC`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query workflows failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, enabled int
		var name, description, path, mapping, created, updated string
		if err := rows.Scan(&id, &name, &description, &path, &mapping, &enabled, &created, &updated); err != nil {
			writeError(w, http.StatusInternalServerError, "read workflow failed")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "description": description, "workflow_path": path, "mapping": json.RawMessage(mapping), "enabled": enabled == 1, "created_at": created, "updated_at": updated})
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) createWorkflow(w http.ResponseWriter, r *http.Request) {
	var input workflowInput
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.Name) == "" || len(input.WorkflowJSON) == 0 {
		writeError(w, http.StatusBadRequest, "name and workflow_json are required")
		return
	}
	var workflow any
	if err := json.Unmarshal(input.WorkflowJSON, &workflow); err != nil {
		writeError(w, http.StatusBadRequest, "workflow_json must be valid JSON")
		return
	}
	mapping := input.Mapping
	if len(mapping) == 0 {
		mapping = json.RawMessage(`{}`)
	}
	if !json.Valid(mapping) {
		writeError(w, http.StatusBadRequest, "mapping must be valid JSON")
		return
	}
	path := a.dataDir + "/workflows/" + safeFilename(input.Name) + ".json"
	if err := writeAtomic(path, input.WorkflowJSON); err != nil {
		writeError(w, http.StatusInternalServerError, "save workflow file failed")
		return
	}
	result, err := a.db.ExecContext(r.Context(), `INSERT INTO workflows(name, description, workflow_path, mapping_json, created_at, updated_at) VALUES(?,?,?,?,?,?)`, input.Name, input.Description, path, string(mapping), time.Now(), time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save workflow failed")
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": input.Name, "workflow_path": path, "mapping": json.RawMessage(mapping)})
}

func (a *app) getWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow id")
		return
	}
	var name, description, path, mapping, created, updated string
	var enabled int
	err = a.db.QueryRowContext(r.Context(), `SELECT name, description, workflow_path, mapping_json, enabled, created_at, updated_at FROM workflows WHERE id=?`, id).Scan(&name, &description, &path, &mapping, &enabled, &created, &updated)
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "description": description, "workflow_path": path, "mapping": json.RawMessage(mapping), "enabled": enabled == 1, "created_at": created, "updated_at": updated})
}

func safeFilename(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, value)
	if value == "" {
		return "workflow"
	}
	return value
}

func writeAtomic(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-workflow-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}
