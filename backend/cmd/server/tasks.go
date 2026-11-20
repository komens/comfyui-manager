package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// 任务操作的统一错误语义：单任务接口据此映射 HTTP 状态码，批量接口据此计入 skipped。
var (
	errTaskNotFound       = errors.New("task not found")
	errTaskRunning        = errors.New("cannot delete running task, cancel it first")
	errTaskNotCancellable = errors.New("task is not cancellable")
	errTaskNotFailed      = errors.New("task has no retryable failed items")
	errTaskWorkflowGone   = errors.New("workflow has been deleted, task cannot be retried")
)

type directTaskInput struct {
	WorkflowID     int64          `json:"workflow_id"`
	PositivePrompt string         `json:"positive_prompt"`
	NegativePrompt string         `json:"negative_prompt"`
	Title          string         `json:"title"`
	Parameters     map[string]any `json:"parameters"`
}

type taskWorkflow struct {
	name, json, mapping, negative string
}

func (a *app) readTaskWorkflow(ctx context.Context, tx *sql.Tx, workflowID int64, requireEnabled bool) (taskWorkflow, error) {
	var workflow taskWorkflow
	var path string
	err := tx.QueryRowContext(ctx, `SELECT name, workflow_path, mapping_json, negative_prompt
		FROM workflows WHERE id=? AND (?=0 OR enabled=1)`, workflowID, requireEnabled).
		Scan(&workflow.name, &path, &workflow.mapping, &workflow.negative)
	if err != nil {
		return workflow, err
	}
	raw, err := readWorkflowFile(a.workflowFilePath(path))
	if err != nil || !json.Valid(raw) {
		// Keep the failure attached to this submission even if the workflow is later repaired.
		workflow.json = "null"
	} else {
		workflow.json = string(raw)
	}
	return workflow, nil
}

// Read the workflow revision under the same write transaction that queues the task.
func (a *app) beginTaskCreation(w http.ResponseWriter, r *http.Request, workflowID int64) (*sql.Tx, taskWorkflow) {
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, 500, "create task failed")
		return nil, taskWorkflow{}
	}
	workflow, err := a.readTaskWorkflow(ctx, tx, workflowID, true)
	if err == nil {
		return tx, workflow
	}
	_ = tx.Rollback()
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 400, "工作流不存在或已停用")
	} else {
		writeError(w, 500, "read workflow failed")
	}
	return nil, taskWorkflow{}
}

func (a *app) createDirectTask(w http.ResponseWriter, r *http.Request) {
	var input directTaskInput
	if err := decodeJSON(r, &input); err != nil || input.WorkflowID <= 0 || strings.TrimSpace(input.PositivePrompt) == "" {
		writeError(w, http.StatusBadRequest, "workflow_id and positive_prompt are required")
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
	tx, workflow := a.beginTaskCreation(w, r, input.WorkflowID)
	if tx == nil {
		return
	}
	defer tx.Rollback()
	// 单条提示词同样入库到 prompts 表（group_name 为手动提交）
	promptResult, err := tx.ExecContext(r.Context(),
		`INSERT INTO prompts(title, description, positive_prompt, negative_prompt, group_name, group_id, status, created_at, updated_at) VALUES(?, '', ?, ?, '手动提交', 'manual', 'running', ?, ?)`,
		title, input.PositivePrompt, input.NegativePrompt, now, now)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "create prompt failed")
		return
	}
	promptID, _ := promptResult.LastInsertId()
	result, err := tx.ExecContext(r.Context(),
		`INSERT INTO generation_tasks(source_type, workflow_id, comfyui_url, parameters_json, workflow_name,
			workflow_snapshot, workflow_mapping_snapshot, workflow_negative_snapshot, created_at) VALUES('direct',?,?,?,?,?,?,?,?)`,
		input.WorkflowID, comfyURL, string(parameters), workflow.name, workflow.json, workflow.mapping, workflow.negative, now)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "create task failed")
		return
	}
	taskID, _ := result.LastInsertId()
	_, err = tx.ExecContext(r.Context(),
		`INSERT INTO generation_items(task_id, prompt_id, positive_prompt, negative_prompt, status) VALUES(?,?,?,?,'pending')`,
		taskID, promptID, input.PositivePrompt, input.NegativePrompt)
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
	// 按提示词分组筛选：任务名下任一 item 的提示词属于该分组即命中
	if group := r.URL.Query().Get("group"); group != "" {
		where += ` AND EXISTS (SELECT 1 FROM generation_items gi JOIN prompts p ON p.id=gi.prompt_id WHERE gi.task_id=t.id AND p.group_name=?)`
		args = append(args, group)
	}
	// 统计总数
	var total int
	countArgs := make([]any, len(args))
	copy(countArgs, args)
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM generation_tasks t`+where, countArgs...).Scan(&total)
	// 分页
	page, pageSize, offset := parsePagination(r)
	queryArgs := append(args, pageSize, offset)
	rows, err := a.db.QueryContext(r.Context(), `SELECT t.id, t.source_type, t.workflow_id, COALESCE(NULLIF(t.workflow_name,''), w.name), t.status, t.total_count, t.success_count, t.failed_count, t.created_at,
		EXISTS (SELECT 1 FROM generation_items gi WHERE gi.task_id=t.id AND gi.status='submitted' AND gi.remote_failure_since>0),
		EXISTS (SELECT 1 FROM generation_items gi WHERE gi.task_id=t.id AND gi.status='cancelled' AND gi.error_message!=''),
		COALESCE((SELECT substr(gi.error_message,1,400) FROM generation_items gi
			WHERE gi.task_id=t.id AND gi.error_message!=''
			  AND (gi.status IN ('failed','cancelled') OR gi.status='submitted' AND gi.remote_failure_since>0)
			ORDER BY (gi.remote_failure_since>0) DESC, gi.id DESC LIMIT 1),'')
		FROM generation_tasks t LEFT JOIN workflows w ON w.id=t.workflow_id`+where+` ORDER BY t.id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query tasks failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	taskIDs := make([]int64, 0, pageSize)
	for rows.Next() {
		var id, workflowID, totalC, success, failed int
		var source, status, created, failureReason string
		var workflowName sql.NullString
		var connectionIssue, cancelUnconfirmed bool
		if err := rows.Scan(&id, &source, &workflowID, &workflowName, &status, &totalC, &success, &failed, &created, &connectionIssue, &cancelUnconfirmed, &failureReason); err != nil {
			writeError(w, http.StatusInternalServerError, "read task failed")
			return
		}
		taskIDs = append(taskIDs, int64(id))
		items = append(items, map[string]any{"id": id, "source_type": source, "workflow_id": workflowID,
			"workflow_name": workflowName.String, "status": status, "total_count": totalC,
			"success_count": success, "failed_count": failed, "created_at": created,
			"connection_issue": connectionIssue, "cancel_unconfirmed": cancelUnconfirmed, "failure_reason": failureReason})
	}
	a.enrichTaskListItems(r.Context(), items, taskIDs)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize})
}

// enrichTaskListItems 为本页任务批量附加代表结果图与关联提示词，纯追加字段，失败时保持原样降级。
func (a *app) enrichTaskListItems(ctx context.Context, items []map[string]any, taskIDs []int64) {
	if len(taskIDs) == 0 {
		return
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(taskIDs)), ",")
	args := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		args[i] = id
	}
	// Keep the representative image stable and include the total without loading every image ID.
	if rows, err := a.db.QueryContext(ctx, `SELECT gi.task_id, MIN(i.id), COUNT(*) FROM images i
		JOIN generation_items gi ON gi.id=i.generation_item_id
		WHERE gi.task_id IN (`+placeholders+`) GROUP BY gi.task_id`, args...); err == nil {
		type imageSummary struct{ firstID, count int64 }
		imageByTask := map[int64]imageSummary{}
		for rows.Next() {
			var taskID int64
			var summary imageSummary
			if err := rows.Scan(&taskID, &summary.firstID, &summary.count); err == nil {
				imageByTask[taskID] = summary
			}
		}
		_ = rows.Close()
		for _, item := range items {
			if id, ok := item["id"].(int); ok {
				if summary, found := imageByTask[int64(id)]; found {
					item["image_id"] = summary.firstID
					item["image_count"] = summary.count
				}
			}
		}
	}
	// 每个任务的提示词（按 item 顺序取第一个非空 prompt，统计去重后的提示词数）
	if rows, err := a.db.QueryContext(ctx, `SELECT gi.task_id, gi.prompt_id, p.title FROM generation_items gi
		JOIN prompts p ON p.id=gi.prompt_id
		WHERE gi.task_id IN (`+placeholders+`) ORDER BY gi.task_id, gi.id`, args...); err == nil {
		type promptInfo struct {
			id    int64
			title string
			count int64
		}
		firstByTask := map[int64]promptInfo{}
		seenPrompt := map[[2]int64]bool{}
		for rows.Next() {
			var taskID, promptID int64
			var title string
			if err := rows.Scan(&taskID, &promptID, &title); err != nil {
				continue
			}
			key := [2]int64{taskID, promptID}
			if seenPrompt[key] {
				continue
			}
			seenPrompt[key] = true
			if info, exists := firstByTask[taskID]; exists {
				info.count++
				firstByTask[taskID] = info
			} else {
				firstByTask[taskID] = promptInfo{id: promptID, title: title, count: 1}
			}
		}
		_ = rows.Close()
		for _, item := range items {
			if id, ok := item["id"].(int); ok {
				if info, found := firstByTask[int64(id)]; found {
					item["prompt_id"] = info.id
					item["prompt_title"] = info.title
					item["prompt_count"] = info.count
				}
			}
		}
	}
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
	rows, rowsErr := a.db.QueryContext(r.Context(), `SELECT i.id, i.prompt_id, i.positive_prompt,
		COALESCE(i.effective_negative_prompt, NULLIF(TRIM(i.negative_prompt),''), CASE WHEN t.workflow_snapshot!='' THEN t.workflow_negative_snapshot END, w.negative_prompt, ''),
		COALESCE(i.comfy_prompt_id,''), i.status, i.error_message, COALESCE(i.notes,''), i.output_count, i.saved_output_count,
		COALESCE(NULLIF(i.submitted_comfyui_url,''), t.comfyui_url)
		FROM generation_items i JOIN generation_tasks t ON t.id=i.task_id
		LEFT JOIN workflows w ON w.id=t.workflow_id WHERE i.task_id=? ORDER BY i.id`, id)
	if rowsErr == nil {
		defer rows.Close()
		for rows.Next() {
			var itemID, outputCount, savedOutputCount int
			var promptID sql.NullInt64
			var positive, negative, promptIDStr, itemStatus, itemError, itemNotes, itemURL string
			if err := rows.Scan(&itemID, &promptID, &positive, &negative, &promptIDStr, &itemStatus, &itemError, &itemNotes, &outputCount, &savedOutputCount, &itemURL); err == nil {
				items = append(items, map[string]any{"id": itemID, "prompt_id": promptID.Int64,
					"positive_prompt": positive, "negative_prompt": negative, "comfy_prompt_id": promptIDStr,
					"status": itemStatus, "error_message": itemError, "notes": itemNotes, "comfyui_url": itemURL, "output_count": outputCount, "saved_output_count": savedOutputCount})
			}
		}
	}
	a.attachItemImages(r.Context(), items)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "source_type": source, "workflow_id": workflowID,
		"comfyui_url": comfyURL, "parameters": json.RawMessage(parameters), "status": status,
		"total_count": total, "success_count": success, "failed_count": failed,
		"created_at": created, "items": items})
}

// attachItemImages 给任务详情的 items 附加各自的结果图列表（一个 item 可能出多张图）。
// 查询失败时静默降级：items 不带 images 字段，前端按无图处理。
func (a *app) attachItemImages(ctx context.Context, items []map[string]any) {
	if len(items) == 0 {
		return
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
	args := make([]any, len(items))
	for i, item := range items {
		args[i] = item["id"]
	}
	rows, err := a.db.QueryContext(ctx, `SELECT generation_item_id, id, filename, width, height FROM images
		WHERE generation_item_id IN (`+placeholders+`) ORDER BY id`, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	imgsByItem := map[int64][]map[string]any{}
	for rows.Next() {
		var itemID, imageID int64
		var filename string
		var imgW, imgH int
		if err := rows.Scan(&itemID, &imageID, &filename, &imgW, &imgH); err == nil {
			imgsByItem[itemID] = append(imgsByItem[itemID], map[string]any{"id": imageID, "filename": filename, "width": imgW, "height": imgH})
		}
	}
	for _, item := range items {
		if id, ok := item["id"].(int); ok {
			if imgs, found := imgsByItem[int64(id)]; found {
				item["images"] = imgs
			}
		}
	}
}

func (a *app) deleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid task id")
		return
	}
	if err := a.deleteTaskByID(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, errTaskNotFound):
			writeError(w, 404, "task not found")
		case errors.Is(err, errTaskRunning):
			writeError(w, 409, "cannot delete running task, cancel it first")
		default:
			writeError(w, 500, "delete task failed")
		}
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "deleted"})
}

// batchTaskInput 是批量操作共用的请求体。
type batchTaskInput struct {
	IDs []int64 `json:"ids"`
}

// batchResult 汇总批量操作结果：applied 为实际生效数，skipped 为状态不允许/不存在的数量。
func batchResult(applied, skipped int) map[string]any {
	return map[string]any{"applied": applied, "skipped": skipped}
}

// maxBatchIDs 批量接口单次允许的 id 上限。
const maxBatchIDs = 500

// runIDBatch 解析请求体里的 ids，再交给 execIDBatch 逐条执行。
func (a *app) runIDBatch(w http.ResponseWriter, r *http.Request, op func(ctx context.Context, id int64) error, skippable func(error) bool) {
	var input batchTaskInput
	if err := decodeJSON(r, &input); err != nil || len(input.IDs) == 0 {
		writeError(w, 400, "ids is required")
		return
	}
	a.execIDBatch(w, r.Context(), input.IDs, op, skippable)
}

// execIDBatch 对一组 id 逐个执行 op，统一做上限校验与结果计数。
// skippable 判定"这条跳过"（如不存在、状态不允许），其它错误视为服务端故障直接中断。
// 抽出它是为了给 body 结构不同的批量接口（如带 favorite 字段的批量收藏）复用同一套计数语义。
func (a *app) execIDBatch(w http.ResponseWriter, ctx context.Context, ids []int64, op func(ctx context.Context, id int64) error, skippable func(error) bool) {
	if len(ids) > maxBatchIDs {
		writeError(w, 400, "too many ids, max 500 per request")
		return
	}
	applied, skipped := 0, 0
	for _, id := range ids {
		err := op(ctx, id)
		switch {
		case err == nil:
			applied++
		case skippable(err):
			skipped++
		default:
			writeError(w, 500, "batch operation failed")
			return
		}
	}
	writeJSON(w, 200, batchResult(applied, skipped))
}

// isTaskSkippable 任务批量操作中可跳过的错误（不存在 / 状态不允许）。
func isTaskSkippable(err error) bool {
	return errors.Is(err, errTaskNotFound) || errors.Is(err, errTaskRunning) ||
		errors.Is(err, errTaskNotCancellable) || errors.Is(err, errTaskNotFailed) ||
		errors.Is(err, errTaskWorkflowGone)
}

func (a *app) batchCancelTasks(w http.ResponseWriter, r *http.Request) {
	a.runIDBatch(w, r, a.cancelTaskByID, isTaskSkippable)
}

func (a *app) batchRetryTasks(w http.ResponseWriter, r *http.Request) {
	a.runIDBatch(w, r, a.retryTaskByID, isTaskSkippable)
}

func (a *app) batchDeleteTasks(w http.ResponseWriter, r *http.Request) {
	a.runIDBatch(w, r, a.deleteTaskByID, isTaskSkippable)
}

// forgetSaveFailures 丢弃该 task 下所有 item 的保存失败计数。
// 任务被取消/删除后这些 item 不会再被 downloader 处理，计数留着只是让 map 无界增长。
func (a *app) forgetSaveFailures(ctx context.Context, taskID int64) {
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM generation_items WHERE task_id=?`, taskID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	a.saveFailMu.Lock()
	for _, id := range ids {
		delete(a.saveFailures, id)
	}
	a.saveFailMu.Unlock()
}

// cancelTaskByID 取消单个任务：更新 task 状态、通知 ComfyUI 清队列、推送事件。
// 已终态或不存在时返回 errTaskNotCancellable / errTaskNotFound。
func (a *app) cancelTaskByID(ctx context.Context, id int64) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM generation_tasks WHERE id=?", id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errTaskNotFound
		}
		return err
	}
	if status != "pending" && status != "running" {
		return errTaskNotCancellable
	}
	var currentURL string
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT value FROM settings WHERE key='comfyui_url'),'')").Scan(&currentURL); err != nil {
		return err
	}
	queued, err := cancellableQueueEntries(ctx, tx, id, currentURL)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE generation_tasks SET status='cancelled', completed_at=CURRENT_TIMESTAMP WHERE id=?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE generation_items SET status='cancelled', remote_missing_since=0, remote_failure_since=0, error_message='' WHERE task_id=? AND status IN ('pending','running','submitted')", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE prompts SET status=CASE WHEN completed_at IS NULL THEN 'pending' ELSE 'done' END, updated_at=CURRENT_TIMESTAMP
		WHERE status='running' AND id IN (SELECT prompt_id FROM generation_items WHERE task_id=? AND status='cancelled')
		AND NOT EXISTS (
			SELECT 1 FROM generation_items i JOIN generation_tasks t ON t.id=i.task_id
			WHERE i.prompt_id=prompts.id AND i.status IN ('pending','running','submitted') AND t.status IN ('pending','running')
		)`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	remoteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, entry := range queued {
		if err := cancelComfyJob(remoteCtx, entry.baseURL, entry.promptID); err != nil {
			a.recordRemoteCancellationError(entry.itemID, err)
		}
	}
	a.forgetSaveFailures(ctx, id)
	a.publish(id, map[string]any{"task_id": id, "status": "cancelled"})
	return nil
}

func (a *app) recordRemoteCancellationError(itemID int64, err error) {
	a.log.Printf("cancel ComfyUI job for item %d: %v", itemID, err)
	var taskID int64
	dbErr := a.db.QueryRow("UPDATE generation_items SET error_message=? WHERE id=? AND status='cancelled' RETURNING task_id",
		"本地已取消，远端取消未确认："+err.Error(), itemID).Scan(&taskID)
	if dbErr == nil {
		a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "cancelled"})
	} else if !errors.Is(dbErr, sql.ErrNoRows) {
		a.log.Printf("save remote cancellation error for item %d: %v", itemID, dbErr)
	}
}

type comfyQueueEntry struct {
	itemID            int64
	baseURL, promptID string
}

func cancellableQueueEntries(ctx context.Context, tx *sql.Tx, taskID int64, currentURL string) ([]comfyQueueEntry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(NULLIF(submitted_comfyui_url,''),
		NULLIF((SELECT comfyui_url FROM generation_tasks WHERE id=generation_items.task_id),''), ?), comfy_prompt_id
		FROM generation_items WHERE task_id=? AND status='submitted' AND COALESCE(comfy_prompt_id,'')!=''`, currentURL, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []comfyQueueEntry
	for rows.Next() {
		var entry comfyQueueEntry
		if err := rows.Scan(&entry.itemID, &entry.baseURL, &entry.promptID); err != nil {
			return nil, err
		}
		if entry.baseURL != "" {
			entries = append(entries, entry)
		}
	}
	return entries, rows.Err()
}

// retryTaskByID 重跑失败任务：重置 task 与失败 item，成功的 item 保持不动，避免重复出图。
func (a *app) retryTaskByID(ctx context.Context, id int64) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var workflowID int64
	var hasFailures bool
	err = tx.QueryRowContext(ctx, `SELECT status, workflow_id, EXISTS (
		SELECT 1 FROM generation_items WHERE task_id=? AND status='failed') FROM generation_tasks WHERE id=?`, id, id).
		Scan(&status, &workflowID, &hasFailures)
	if errors.Is(err, sql.ErrNoRows) {
		return errTaskNotFound
	}
	if err != nil {
		return err
	}
	if (status != "failed" && status != "completed") || !hasFailures {
		return errTaskNotFailed
	}
	workflow, err := a.readTaskWorkflow(ctx, tx, workflowID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return errTaskWorkflowGone
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE generation_items SET effective_negative_prompt=COALESCE(
			NULLIF(TRIM(negative_prompt),''), (SELECT CASE WHEN t.workflow_snapshot!='' THEN t.workflow_negative_snapshot
				ELSE w.negative_prompt END FROM generation_tasks t LEFT JOIN workflows w ON w.id=t.workflow_id
				WHERE t.id=generation_items.task_id), '')
		WHERE task_id=? AND status IN ('success','failed') AND effective_negative_prompt IS NULL`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE images SET effective_negative_prompt=(
			SELECT effective_negative_prompt FROM generation_items WHERE id=images.generation_item_id)
		WHERE effective_negative_prompt IS NULL AND generation_item_id IN
			(SELECT id FROM generation_items WHERE task_id=? AND status='failed')`, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE generation_tasks SET status='pending', failed_count=0, completed_at=NULL,
		comfyui_url=(SELECT value FROM settings WHERE key='comfyui_url'),
		workflow_snapshot=?, workflow_mapping_snapshot=?, workflow_negative_snapshot=? WHERE id=?`,
		workflow.json, workflow.mapping, workflow.negative, id)
	if err != nil {
		return err
	}
	// 关键：只重置 task 是没用的，submitter 只捞 item.status='pending'。
	// 必须把该 task 下失败的 item 一并重置（成功的 item 保持不动，避免重复出图）。
	//
	// 上一次尝试已经存下的图不动（收藏与否都不动）：重试可以换工作流，那本来就等于
	// 重新生成另一批图，旧图是上一版的结果，删掉等于用户拿不回来。图片的清理只走
	// 图库里的删除 / 整理页，重试不承担删图职责。
	if _, err := tx.ExecContext(ctx,
		`UPDATE generation_items SET status='pending', comfy_prompt_id='', error_message='', output_count=0, saved_output_count=0, submitted_comfyui_url='', remote_missing_since=0, remote_failure_since=0, effective_negative_prompt=NULL, notes='' WHERE task_id=? AND status='failed'`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	// 关联提示词状态同步回 pending，保持列表与任务状态一致
	if _, err := tx.ExecContext(ctx,
		`UPDATE prompts SET status='pending', updated_at=CURRENT_TIMESTAMP
		 WHERE id IN (SELECT prompt_id FROM generation_items WHERE task_id=? AND status='pending' AND prompt_id IS NOT NULL)`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return err
	}
	a.publish(id, map[string]any{"task_id": id, "status": "pending"})
	return nil
}

// deleteTaskByID 删除任务及其关联的 items / images（running 状态不允许删除）。
//
// 图片是**行和文件一起删**：只删行的话原图会变成没有任何记录引用的孤儿文件，占着磁盘
// 直到有人去维护页点「清理孤儿」。收藏的图片例外——那是用户在图库里明确留下的，
// 它的行、原图和缩略图都原地保留（只删掉所属的生成项，图库里照常能看到）。
func (a *app) deleteTaskByID(ctx context.Context, id int64) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM generation_tasks WHERE id=?", id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errTaskNotFound
		}
		return err
	}
	if status == "running" {
		return errTaskRunning
	}
	purge, err := taskImagesToPurge(ctx, tx, id)
	if err != nil {
		return err
	}
	if status == "pending" {
		if _, err := tx.ExecContext(ctx, `UPDATE prompts SET
			status=CASE WHEN completed_at IS NULL THEN 'pending' ELSE 'done' END, updated_at=CURRENT_TIMESTAMP
			WHERE status='running' AND id IN (SELECT prompt_id FROM generation_items WHERE task_id=?)
			AND NOT EXISTS (SELECT 1 FROM generation_items i JOIN generation_tasks t ON t.id=i.task_id
				WHERE i.prompt_id=prompts.id AND t.id!=? AND t.status IN ('pending','running')
				AND i.status IN ('pending','running','submitted'))`, id, id); err != nil {
			return err
		}
	}
	if len(purge) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM images WHERE is_favorite=0 AND id IN (`+placeholders(len(purge))+`)`, imageIDs(purge)...); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM generation_items WHERE task_id=?`, id); err != nil {
		return err
	}
	// 带上状态条件，避免 SELECT 之后任务转为 running 时被并发删除
	result, err := tx.ExecContext(ctx, `DELETE FROM generation_tasks WHERE id=? AND status!='running'`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return errTaskRunning
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return err
	}
	// 文件在提交之后删：事务回滚时盘上的原图必须还在。
	// 删失败也不回滚——维护页的孤儿清理能兜底，为此让整个删除操作报错不值得。
	for _, img := range purge {
		if target := a.imageFilePath(img.storagePath); target != "" {
			_ = os.Remove(target)
		}
		a.removeThumbs(img.id)
	}
	return nil
}

type taskImage struct {
	id          int64
	storagePath string
}

// taskImagesToPurge 取任务下「非收藏」的图片，供删除任务时清行+清盘。
// 收藏的图不在结果里，删除任务不会碰它的行、原图和缩略图。
func taskImagesToPurge(ctx context.Context, tx *sql.Tx, taskID int64) ([]taskImage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT img.id, COALESCE(img.storage_path,'') FROM images img
		WHERE img.is_favorite=0
		AND img.generation_item_id IN (SELECT id FROM generation_items WHERE task_id=?)`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []taskImage
	for rows.Next() {
		var img taskImage
		if err := rows.Scan(&img.id, &img.storagePath); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

func imageIDs(images []taskImage) []any {
	args := make([]any, len(images))
	for i, img := range images {
		args[i] = img.id
	}
	return args
}
