package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// errPromptNotFound 表示提示词不存在。收藏级联等写操作据此返回 404 而非 500。
var errPromptNotFound = errors.New("prompt not found")

type promptInput struct {
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	PositivePrompt string  `json:"positive_prompt"`
	NegativePrompt *string `json:"negative_prompt"`
	GroupName      string  `json:"group_name"`
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
	// 来源平台："local" 特指本平台提示词（来源为空），其余按平台标识精确匹配
	if platform := r.URL.Query().Get("platform"); platform != "" {
		if platform == platformLocal {
			where += ` AND p.source_platform=''`
		} else {
			where += ` AND p.source_platform=?`
			args = append(args, platform)
		}
	}
	// 统计总数
	var total int
	countArgs := make([]any, len(args))
	copy(countArgs, args)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM prompts p`+where, countArgs...).Scan(&total)

	// 分页参数
	page, pageSize, offset := parsePagination(r)

	// 图片统计要覆盖两条关联路径：本平台图走 generation_items，外部同步图走 images.prompt_id。
	// 只写旧的 JOIN 会让外部提示词永远显示「0 张图、无封面」。
	const imageOfPrompt = `(img.prompt_id=p.id OR img.generation_item_id IN (SELECT gi.id FROM generation_items gi WHERE gi.prompt_id=p.id))`
	query := `SELECT p.id, p.title, p.description, p.positive_prompt, p.group_name, p.group_id, p.status, p.is_favorite, p.completed_at, p.created_at, p.source_platform,
		(SELECT COUNT(*) FROM generation_items gi WHERE gi.prompt_id=p.id) AS run_count,
		(SELECT COUNT(*) FROM images img WHERE ` + imageOfPrompt + `) AS image_count,
		(SELECT img2.id FROM images img2 WHERE (img2.prompt_id=p.id OR img2.generation_item_id IN (SELECT gi2.id FROM generation_items gi2 WHERE gi2.prompt_id=p.id)) ORDER BY img2.id DESC LIMIT 1) AS cover_image_id
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
		var title, desc, positive, groupName, groupID, status, platform string
		var completedAt sql.NullString
		var createdAt string
		var coverImageID sql.NullInt64
		if err := rows.Scan(&id, &title, &desc, &positive, &groupName, &groupID, &status, &isFav, &completedAt, &createdAt, &platform, &runCount, &imageCount, &coverImageID); err != nil {
			writeError(w, 500, "read prompt failed")
			return
		}
		items = append(items, map[string]any{
			"id": id, "title": title, "description": desc, "positive_prompt": positive,
			"group_name": groupName, "group_id": groupID, "status": status,
			"is_favorite":  isFav == 1,
			"completed_at": completedAt.String, "created_at": createdAt,
			"run_count": runCount, "image_count": imageCount, "cover_image_id": coverImageID.Int64,
			"source_platform": platform,
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
	// 新建没有「原值」，nil（未提供）直接落空串，与旧行为一致。
	result, err := a.db.ExecContext(r.Context(),
		`INSERT INTO prompts(title, description, positive_prompt, negative_prompt, group_name, group_id, status, created_at, updated_at) VALUES(?,?,?,?,?,?,'pending',?,?)`,
		input.Title, input.Description, input.PositivePrompt, strOrFallback(input.NegativePrompt, ""), input.GroupName, groupID, now, now)
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
	var title, desc, positive, negative, groupName, groupID, status, platform string
	var isFav int
	var completedAt sql.NullString
	var createdAt, updatedAt string
	err = a.db.QueryRowContext(r.Context(),
		`SELECT title, description, positive_prompt, COALESCE(negative_prompt,''), group_name, group_id, status, is_favorite, completed_at, created_at, updated_at, source_platform FROM prompts WHERE id=?`, id).
		Scan(&title, &desc, &positive, &negative, &groupName, &groupID, &status, &isFav, &completedAt, &createdAt, &updatedAt, &platform)
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
			imgRows, _ := a.db.QueryContext(r.Context(), `SELECT id, filename, width, height FROM images WHERE generation_item_id=?`, itemID)
			images := make([]map[string]any, 0)
			if imgRows != nil {
				for imgRows.Next() {
					var imgID, imgW, imgH int
					var filename string
					_ = imgRows.Scan(&imgID, &filename, &imgW, &imgH)
					images = append(images, map[string]any{"id": imgID, "filename": filename, "width": imgW, "height": imgH})
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
	// 外部同步图不挂在 generation_items 上，runs 里取不到，单独出一份。
	// 前端拿它渲染「外部来源」区块，否则外部提示词详情页会显示「还没有生成记录」。
	externalImages := make([]map[string]any, 0)
	if imgRows, imgErr := a.db.QueryContext(r.Context(),
		`SELECT id, filename, width, height FROM images WHERE prompt_id=? ORDER BY id DESC`, id); imgErr == nil {
		defer imgRows.Close()
		for imgRows.Next() {
			var imgID, imgW, imgH int
			var filename string
			if err := imgRows.Scan(&imgID, &filename, &imgW, &imgH); err == nil {
				externalImages = append(externalImages, map[string]any{"id": imgID, "filename": filename, "width": imgW, "height": imgH})
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "title": title, "description": desc, "positive_prompt": positive, "negative_prompt": negative,
		"group_name": groupName, "group_id": groupID, "status": status, "is_favorite": isFav == 1,
		"completed_at": completedAt.String, "created_at": createdAt, "updated_at": updatedAt, "runs": runs,
		"source_platform": platform, "external_images": externalImages,
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
	var oldTitle, oldDesc, oldPositive, oldNegative, oldGroupName, oldGroupID string
	err = a.db.QueryRowContext(r.Context(), `SELECT title, description, positive_prompt, COALESCE(negative_prompt,''), group_name, group_id FROM prompts WHERE id=?`, id).Scan(&oldTitle, &oldDesc, &oldPositive, &oldNegative, &oldGroupName, &oldGroupID)
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
	// negative 用指针区分「没传这个字段」和「传了空串」：nil=保持原值，""=清空，有值=改。
	// 这与 workflows.go 的 negative_prompt 同一套约定（strOrFallback）。前端编辑表单
	// 恒定带 negative_prompt 键，所以「清空文本框再保存」能真正清掉——清成空串后，
	// 提交时 resolveEffectiveNegative 会回落到所属工作流的默认负面，语义自洽。
	negative := strOrFallback(input.NegativePrompt, oldNegative)
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
		`UPDATE prompts SET title=?, description=?, positive_prompt=?, negative_prompt=?, group_name=?, group_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		title, desc, positive, negative, groupName, groupID, id)
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
	var cur int
	if err := a.db.QueryRowContext(r.Context(), `SELECT is_favorite FROM prompts WHERE id=?`, id).Scan(&cur); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "prompt not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "toggle favorite failed")
		return
	}
	fav := 1 - cur
	// 收藏以提示词为主体，级联到其名下所有图片（图片↔提示词共享收藏态）
	if err := a.setPromptFavorite(r.Context(), id, fav); err != nil {
		if errors.Is(err, errPromptNotFound) {
			writeError(w, http.StatusNotFound, "prompt not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "toggle favorite failed")
		return
	}
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
	comfyURL, err := a.setting(r.Context(), "comfyui_url")
	if err != nil {
		writeError(w, 500, "read ComfyUI URL failed")
		return
	}
	tx, workflowName := a.beginTaskCreation(w, r, input.WorkflowID)
	if tx == nil {
		return
	}
	defer tx.Rollback()
	taskID, err := queuePromptTask(r.Context(), tx, id, input.WorkflowID, workflowName, comfyURL, input.Parameters)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "prompt not found")
		return
	}
	if err != nil || tx.Commit() != nil {
		writeError(w, 500, "create task failed")
		return
	}
	writeJSON(w, 202, map[string]any{"task_id": taskID, "prompt_id": id, "status": "pending"})
}

func queuePromptTask(ctx context.Context, tx *sql.Tx, promptID, workflowID int64, workflowName, comfyURL string, parameters map[string]any) (int64, error) {
	var positive, negative string
	if err := tx.QueryRowContext(ctx, "SELECT positive_prompt, COALESCE(negative_prompt,'') FROM prompts WHERE id=?", promptID).Scan(&positive, &negative); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(map[string]any{"positive_prompt": positive, "parameters": parameters})
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx,
		"INSERT INTO generation_tasks(source_type, workflow_id, comfyui_url, parameters_json, workflow_name, created_at) VALUES('prompt',?,?,?,?,?)",
		workflowID, comfyURL, string(payload), workflowName, time.Now())
	if err != nil {
		return 0, err
	}
	taskID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO generation_items(task_id, prompt_id, positive_prompt, negative_prompt, status) VALUES(?,?,?,?,'pending')",
		taskID, promptID, positive, negative); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE prompts SET status='running', updated_at=CURRENT_TIMESTAMP WHERE id=?", promptID)
	return taskID, err
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
	a.queuePromptBatch(w, r, input.WorkflowID, input.Parameters, input.IDs, "")
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
	a.queuePromptBatch(w, r, input.WorkflowID, input.Parameters, nil, input.GroupName)
}

func (a *app) queuePromptBatch(w http.ResponseWriter, r *http.Request, workflowID int64, parameters map[string]any, ids []int64, group string) {
	ctx := r.Context()
	comfyURL, err := a.setting(ctx, "comfyui_url")
	if err != nil {
		writeError(w, 500, "read ComfyUI URL failed")
		return
	}
	tx, workflowName := a.beginTaskCreation(w, r, workflowID)
	if tx == nil {
		return
	}
	defer tx.Rollback()
	if group != "" {
		ids, err = groupPromptIDs(ctx, tx, group)
		if err != nil {
			writeError(w, 500, "read group prompts failed")
			return
		}
	}
	created := 0
	for _, id := range ids {
		_, err := queuePromptTask(ctx, tx, id, workflowID, workflowName, comfyURL, parameters)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			writeError(w, 500, "create tasks failed")
			return
		}
		created++
	}
	if err := tx.Commit(); err != nil {
		writeError(w, 500, "commit tasks failed")
		return
	}
	writeJSON(w, 200, map[string]any{"created": created})
}

func groupPromptIDs(ctx context.Context, tx *sql.Tx, group string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM prompts WHERE group_name=? AND status IN ('pending','failed')", group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
