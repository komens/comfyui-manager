package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type promptInput struct {
	Title          string `json:"title"`
	Description    string `json:"description"`
	PositivePrompt string `json:"positive_prompt"`
	GroupName      string `json:"group_name"`
}

func (a *app) listPrompts(w http.ResponseWriter, r *http.Request) {
	where := ` WHERE 1=1`
	args := []any{}
	if group := r.URL.Query().Get("group"); group != "" {
		where += ` AND p.group_name=?`
		args = append(args, group)
	}
	if status := r.URL.Query().Get("status"); status != "" {
		where += ` AND p.status=?`
		args = append(args, status)
	}
	if search := r.URL.Query().Get("q"); search != "" {
		where += ` AND (p.title LIKE ? OR p.positive_prompt LIKE ?)`
		like := "%" + search + "%"
		args = append(args, like, like)
	}
	if r.URL.Query().Get("favorite") == "1" {
		where += ` AND p.is_favorite=1`
	}
	// 统计总数
	var total int
	countArgs := make([]any, len(args))
	copy(countArgs, args)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM prompts p`+where, countArgs...).Scan(&total)

	// 分页参数
	page, pageSize, offset := parsePagination(r)

	query := `SELECT p.id, p.title, p.description, p.positive_prompt, p.group_name, p.group_id, p.status, p.is_favorite, p.completed_at, p.created_at,
		(SELECT COUNT(*) FROM generation_items gi WHERE gi.prompt_id=p.id) AS run_count,
		(SELECT COUNT(*) FROM images img JOIN generation_items gi ON img.generation_item_id=gi.id WHERE gi.prompt_id=p.id) AS image_count
		FROM prompts p` + where + ` ORDER BY p.id DESC LIMIT ? OFFSET ?`
	queryArgs := append(args, pageSize, offset)
	rows, err := a.db.QueryContext(r.Context(), query, queryArgs...)
	if err != nil {
		writeError(w, 500, "query prompts failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, runCount, imageCount, isFav int
		var title, desc, positive, groupName, groupID, status string
		var completedAt sql.NullString
		var createdAt string
		if err := rows.Scan(&id, &title, &desc, &positive, &groupName, &groupID, &status, &isFav, &completedAt, &createdAt, &runCount, &imageCount); err != nil {
			writeError(w, 500, "read prompt failed")
			return
		}
		items = append(items, map[string]any{
			"id": id, "title": title, "description": desc, "positive_prompt": positive,
			"group_name": groupName, "group_id": groupID, "status": status,
			"is_favorite":  isFav == 1,
			"completed_at": completedAt.String, "created_at": createdAt,
			"run_count": runCount, "image_count": imageCount,
		})
	}
	writeJSON(w, 200, map[string]any{
		"items": items, "total": total, "page": page, "page_size": pageSize,
	})
}

func (a *app) createPrompt(w http.ResponseWriter, r *http.Request) {
	var input promptInput
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.PositivePrompt) == "" {
		writeError(w, 400, "positive_prompt is required")
		return
	}
	if input.Title == "" {
		// 必须用 rune-safe 的 truncate：直接切片是按字节切，切在多字节字符中间
		// 会产出非法 UTF-8，入库即乱码（createDirectTask / uploadJSON 都是这么做的）
		input.Title = truncate(input.PositivePrompt, 50)
	}
	groupID := ""
	if input.GroupName != "" {
		groupID = slugify(input.GroupName)
	}
	now := time.Now()
	result, err := a.db.ExecContext(r.Context(),
		`INSERT INTO prompts(title, description, positive_prompt, group_name, group_id, status, created_at, updated_at) VALUES(?,?,?,?,?,'pending',?,?)`,
		input.Title, input.Description, input.PositivePrompt, input.GroupName, groupID, now, now)
	if err != nil {
		writeError(w, 500, "create prompt failed")
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, 201, map[string]any{"id": id, "title": input.Title})
}

func (a *app) getPrompt(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid prompt id")
		return
	}
	var title, desc, positive, groupName, groupID, status string
	var completedAt sql.NullString
	var createdAt, updatedAt string
	err = a.db.QueryRowContext(r.Context(),
		`SELECT title, description, positive_prompt, group_name, group_id, status, completed_at, created_at, updated_at FROM prompts WHERE id=?`, id).
		Scan(&title, &desc, &positive, &groupName, &groupID, &status, &completedAt, &createdAt, &updatedAt)
	if err != nil {
		writeError(w, 404, "prompt not found")
		return
	}
	// Get generation history with images
	rows, _ := a.db.QueryContext(r.Context(),
		`SELECT gi.id, gi.task_id, gi.status, gi.comfy_prompt_id, gi.error_message, gt.workflow_id, w.name
		 FROM generation_items gi
		 JOIN generation_tasks gt ON gi.task_id=gt.id
		 JOIN workflows w ON gt.workflow_id=w.id
		 WHERE gi.prompt_id=? ORDER BY gi.id DESC`, id)
	runs := make([]map[string]any, 0)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var itemID, taskID int
			var itemStatus string
			var promptID, errorMsg string
			var workflowID int
			var workflowName string
			_ = rows.Scan(&itemID, &taskID, &itemStatus, &promptID, &errorMsg, &workflowID, &workflowName)
			// Get images for this item
			imgRows, _ := a.db.QueryContext(r.Context(), `SELECT id, filename FROM images WHERE generation_item_id=?`, itemID)
			images := make([]map[string]any, 0)
			if imgRows != nil {
				for imgRows.Next() {
					var imgID int
					var filename string
					_ = imgRows.Scan(&imgID, &filename)
					images = append(images, map[string]any{"id": imgID, "filename": filename})
				}
				imgRows.Close()
			}
			runs = append(runs, map[string]any{
				"item_id": itemID, "task_id": taskID, "status": itemStatus,
				"comfy_prompt_id": promptID, "error_message": errorMsg,
				"workflow_id": workflowID, "workflow_name": workflowName, "images": images,
			})
		}
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "title": title, "description": desc, "positive_prompt": positive,
		"group_name": groupName, "group_id": groupID, "status": status,
		"completed_at": completedAt.String, "created_at": createdAt, "updated_at": updatedAt, "runs": runs,
	})
}

func (a *app) updatePrompt(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid prompt id")
		return
	}
	var input promptInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, 400, "invalid JSON body")
		return
	}
	var oldTitle, oldDesc, oldPositive, oldGroupName, oldGroupID string
	err = a.db.QueryRowContext(r.Context(), `SELECT title, description, positive_prompt, group_name, group_id FROM prompts WHERE id=?`, id).Scan(&oldTitle, &oldDesc, &oldPositive, &oldGroupName, &oldGroupID)
	if err != nil {
		writeError(w, 404, "prompt not found")
		return
	}
	title := input.Title
	if title == "" {
		title = oldTitle
	}
	desc := input.Description
	if desc == "" {
		desc = oldDesc
	}
	positive := input.PositivePrompt
	if positive == "" {
		positive = oldPositive
	}
	groupName := input.GroupName
	if groupName == "" {
		groupName = oldGroupName
	}
	// 分组名没变就保留原 group_id（中文分组名会被 slugify 过滤成空串，
	// 无条件重算会让 "manual" 之类的既有 id 漂移成随机值）
	groupID := oldGroupID
	if groupName != oldGroupName {
		groupID = ""
		if groupName != "" {
			groupID = slugify(groupName)
		}
	}
	_, err = a.db.ExecContext(r.Context(),
		`UPDATE prompts SET title=?, description=?, positive_prompt=?, group_name=?, group_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		title, desc, positive, groupName, groupID, id)
	if err != nil {
		writeError(w, 500, "update prompt failed")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "updated": true})
}

func (a *app) deletePrompt(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid prompt id")
		return
	}
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM prompts WHERE id=?`, id)
	if err != nil {
		writeError(w, 500, "delete prompt failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, 404, "prompt not found")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "deleted": true})
}

func (a *app) togglePromptFavorite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid prompt id")
		return
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE prompts SET is_favorite = 1 - is_favorite, updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "toggle favorite failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "prompt not found")
		return
	}
	var fav int
	_ = a.db.QueryRowContext(r.Context(), `SELECT is_favorite FROM prompts WHERE id=?`, id).Scan(&fav)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "is_favorite": fav == 1})
}

func (a *app) runPrompt(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid prompt id")
		return
	}
	var input struct {
		WorkflowID int64          `json:"workflow_id"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := decodeJSON(r, &input); err != nil || input.WorkflowID <= 0 {
		writeError(w, 400, "workflow_id is required")
		return
	}
	var positive string
	if err := a.db.QueryRowContext(r.Context(), `SELECT positive_prompt FROM prompts WHERE id=?`, id).Scan(&positive); err != nil {
		writeError(w, 404, "prompt not found")
		return
	}
	comfyURL, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, 500, "read ComfyUI URL failed")
		return
	}
	parameters, _ := json.Marshal(map[string]any{"positive_prompt": positive, "parameters": input.Parameters})
	now := time.Now()
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, 500, "create task failed")
		return
	}
	result, err := tx.ExecContext(r.Context(),
		`INSERT INTO generation_tasks(source_type, workflow_id, comfyui_url, parameters_json, created_at) VALUES('prompt',?,?,?,?)`,
		input.WorkflowID, comfyURL, string(parameters), now)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, 500, "create task failed")
		return
	}
	taskID, _ := result.LastInsertId()
	_, err = tx.ExecContext(r.Context(),
		`INSERT INTO generation_items(task_id, prompt_id, positive_prompt, status) VALUES(?,?,?,'pending')`, taskID, id, positive)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, 500, "create task item failed")
		return
	}
	_, _ = tx.ExecContext(r.Context(), `UPDATE prompts SET status='running', updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
	if err := tx.Commit(); err != nil {
		writeError(w, 500, "commit task failed")
		return
	}
	writeJSON(w, 202, map[string]any{"task_id": taskID, "prompt_id": id, "status": "pending"})
}

func (a *app) listPromptGroups(w http.ResponseWriter, r *http.Request) {
	// 「手动提交」固定排在最前（界面上仅次于始终第一的「全部提示词」），
	// 其余按最近创建时间倒序。用 group_id='manual' 判定，同时兼容手建同名分组的情况。
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT group_name, group_id, COUNT(*) as count FROM prompts WHERE group_name!=''
		 GROUP BY group_name
		 ORDER BY CASE WHEN group_id='manual' OR group_name='手动提交' THEN 0 ELSE 1 END, MAX(created_at) DESC`)
	if err != nil {
		writeError(w, 500, "query groups failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var name, gid string
		var count int
		if err := rows.Scan(&name, &gid, &count); err != nil {
			writeError(w, 500, "read group failed")
			return
		}
		items = append(items, map[string]any{"group_name": name, "group_id": gid, "count": count})
	}
	writeJSON(w, 200, items)
}

// slugify 把分组名转成稳定的 ASCII id。纯中文等无法转换的输入返回空串，
// 不再伪造 group-<时间戳>——那会让同一分组的 id 每次调用都不同。
func slugify(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' {
			b.WriteByte('-')
		}
	}
	result := b.String()
	result = strings.Trim(result, "-")
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	return result
}

func (a *app) batchDeletePrompts(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeJSON(r, &input); err != nil || len(input.IDs) == 0 {
		writeError(w, 400, "ids is required")
		return
	}
	placeholders := make([]string, len(input.IDs))
	args := make([]any, len(input.IDs))
	for i, id := range input.IDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `DELETE FROM prompts WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	result, err := a.db.ExecContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, 500, "batch delete failed")
		return
	}
	n, _ := result.RowsAffected()
	writeJSON(w, 200, map[string]any{"deleted": n})
}

func (a *app) batchRunPrompts(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs        []int64        `json:"ids"`
		WorkflowID int64          `json:"workflow_id"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := decodeJSON(r, &input); err != nil || len(input.IDs) == 0 || input.WorkflowID <= 0 {
		writeError(w, 400, "ids and workflow_id are required")
		return
	}
	comfyURL, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, 500, "read ComfyUI URL failed")
		return
	}
	created := 0
	for _, id := range input.IDs {
		var positive string
		if err := a.db.QueryRowContext(r.Context(), `SELECT positive_prompt FROM prompts WHERE id=?`, id).Scan(&positive); err != nil {
			continue
		}
		parameters, _ := json.Marshal(map[string]any{"positive_prompt": positive, "parameters": input.Parameters})
		now := time.Now()
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			continue
		}
		result, err := tx.ExecContext(r.Context(),
			`INSERT INTO generation_tasks(source_type, workflow_id, comfyui_url, parameters_json, created_at) VALUES('prompt',?,?,?,?)`,
			input.WorkflowID, comfyURL, string(parameters), now)
		if err != nil {
			_ = tx.Rollback()
			continue
		}
		taskID, _ := result.LastInsertId()
		_, _ = tx.ExecContext(r.Context(),
			`INSERT INTO generation_items(task_id, prompt_id, positive_prompt, status) VALUES(?,?,?,'pending')`, taskID, id, positive)
		_, _ = tx.ExecContext(r.Context(), `UPDATE prompts SET status='running', updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
		_ = tx.Commit()
		created++
	}
	writeJSON(w, 200, map[string]any{"created": created})
}

func (a *app) groupRunPrompts(w http.ResponseWriter, r *http.Request) {
	var input struct {
		GroupName  string         `json:"group_name"`
		WorkflowID int64          `json:"workflow_id"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := decodeJSON(r, &input); err != nil || input.GroupName == "" || input.WorkflowID <= 0 {
		writeError(w, 400, "group_name and workflow_id are required")
		return
	}
	comfyURL, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, 500, "read ComfyUI URL failed")
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, positive_prompt FROM prompts WHERE group_name=? AND status IN ('pending','failed')`, input.GroupName)
	if err != nil {
		writeError(w, 500, "query group prompts failed")
		return
	}
	type item struct {
		id       int64
		positive string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.positive); err == nil {
			items = append(items, it)
		}
	}
	readErr := rows.Err()
	// 先把结果读干净、关掉读游标，再开写事务：游标未关就 BeginTx，
	// WAL 下能跑，但一旦 journal_mode 退回 delete 就会直接锁死。
	rows.Close()
	if readErr != nil {
		writeError(w, 500, "read group prompts failed")
		return
	}
	created := 0
	for _, it := range items {
		parameters, _ := json.Marshal(map[string]any{"positive_prompt": it.positive, "parameters": input.Parameters})
		now := time.Now()
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			continue
		}
		result, err := tx.ExecContext(r.Context(),
			`INSERT INTO generation_tasks(source_type, workflow_id, comfyui_url, parameters_json, created_at) VALUES('prompt',?,?,?,?)`,
			input.WorkflowID, comfyURL, string(parameters), now)
		if err != nil {
			_ = tx.Rollback()
			continue
		}
		taskID, _ := result.LastInsertId()
		_, _ = tx.ExecContext(r.Context(),
			`INSERT INTO generation_items(task_id, prompt_id, positive_prompt, status) VALUES(?,?,?,'pending')`, taskID, it.id, it.positive)
		_, _ = tx.ExecContext(r.Context(), `UPDATE prompts SET status='running', updated_at=CURRENT_TIMESTAMP WHERE id=?`, it.id)
		_ = tx.Commit()
		created++
	}
	writeJSON(w, 200, map[string]any{"created": created})
}
