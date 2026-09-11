package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
}

type jsonDocument struct {
	Entries []jsonEntry `json:"entries"`
}

// uploadJSON 上传 JSON 文件，解析后入库到 prompts 表，并备份源文件
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
	// 生成备份文件名（保留原始名 + 时间戳）
	originalName := strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename))
	groupName := originalName
	timestamp := time.Now().Format("20060102_150405")
	backupName := fmt.Sprintf("%s_%s.json", originalName, timestamp)
	backupPath := filepath.Join(a.dataDir, "json", backupName)
	if err := writeAtomic(backupPath, content); err != nil {
		writeError(w, 500, "save JSON backup failed")
		return
	}
	now := time.Now()
	// 记录文件（仅备份用途）
	_, err = a.db.ExecContext(r.Context(),
		`INSERT INTO json_files(filename, storage_path, created_at) VALUES(?,?,?)`,
		backupName, backupPath, now)
	if err != nil {
		writeError(w, 500, "save JSON record failed")
		return
	}
	// 去重：按 positive_prompt + group_name 检查
	groupID := slugify(groupName)
	inserted, skipped := 0, 0
	for _, entry := range entries {
		title := entry.Title
		if title == "" {
			title = truncate(entry.Positive, 50)
		}
		// 检查同组内是否已存在相同 positive_prompt
		var exists int
		_ = a.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM prompts WHERE group_name=? AND positive_prompt=?`, groupName, entry.Positive).Scan(&exists)
		if exists > 0 {
			skipped++
			continue
		}
		_, err := a.db.ExecContext(r.Context(),
			`INSERT INTO prompts(title, description, positive_prompt, group_name, group_id, status, created_at, updated_at) VALUES(?,?,?,?,?,'pending',?,?)`,
			title, entry.Desc, entry.Positive, groupName, groupID, now, now)
		if err != nil {
			continue
		}
		inserted++
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"filename": backupName, "total": len(entries),
		"inserted": inserted, "skipped_duplicates": skipped,
		"group_name": groupName,
	})
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
		if strings.TrimSpace(e.Positive) == "" {
			return nil, fmt.Errorf("each entry requires a positive prompt")
		}
		key := e.ID
		if key == "" {
			key = truncate(e.Positive, 32)
		}
		if seen[key] {
			// 允许文件内 id 重复，用 positive 内容去重
		}
		seen[key] = true
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no entries found in JSON")
	}
	return entries, nil
}

// listJSONFiles 仅列出备份文件
func (a *app) listJSONFiles(w http.ResponseWriter, r *http.Request) {
	// 统计总数
	var total int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM json_files`).Scan(&total)
	// 分页
	page, pageSize, offset := parsePagination(r)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, filename, storage_path, created_at FROM json_files ORDER BY created_at DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		writeError(w, 500, "query JSON files failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id int
		var name, path, created string
		if err := rows.Scan(&id, &name, &path, &created); err != nil {
			writeError(w, 500, "read JSON file failed")
			return
		}
		items = append(items, map[string]any{"id": id, "filename": name, "created_at": created})
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// getJSONFile 查看文件内容
func (a *app) getJSONFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid JSON file id")
		return
	}
	var name, path, created string
	err = a.db.QueryRowContext(r.Context(),
		`SELECT filename, storage_path, created_at FROM json_files WHERE id=?`, id).Scan(&name, &path, &created)
	if err != nil {
		writeError(w, 404, "JSON file not found")
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		writeError(w, 500, "read JSON file failed")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "filename": name, "content": string(content), "created_at": created})
}

// downloadJSON 下载源文件
func (a *app) downloadJSON(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid JSON file id")
		return
	}
	var name, path string
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT filename, storage_path FROM json_files WHERE id=?`, id).Scan(&name, &path); err != nil {
		writeError(w, 404, "JSON file not found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, `_`)+`"`)
	http.ServeFile(w, r, path)
}

// deleteJSONFile 删除文件记录和备份文件（不影响已入库的 prompts）
func (a *app) deleteJSONFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid JSON file id")
		return
	}
	var path string
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT storage_path FROM json_files WHERE id=?`, id).Scan(&path); err != nil {
		writeError(w, 404, "JSON file not found")
		return
	}
	_ = os.Remove(path)
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM json_files WHERE id=?`, id)
	if err != nil {
		writeError(w, 500, "delete JSON file failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, 404, "JSON file not found")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "deleted": true})
}

func truncate(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n])
}
