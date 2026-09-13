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

// 任务操作的统一错误语义：单任务接口据此映射 HTTP 状态码，批量接口据此计入 skipped。
var (
	errTaskNotFound       = errors.New("task not found")
	errTaskRunning        = errors.New("cannot delete running task, cancel it first")
	errTaskNotCancellable = errors.New("task is not cancellable")
	errTaskNotFailed      = errors.New("only failed tasks can be retried")
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
		`INSERT INTO generation_items(task_id, prompt_id, positive_prompt, status) VALUES(?,?,?,'pending')`,
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
		errors.Is(err, errTaskNotCancellable) || errors.Is(err, errTaskNotFailed)
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
	var status string
	if err := a.db.QueryRowContext(ctx, `SELECT status FROM generation_tasks WHERE id=?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errTaskNotFound
		}
		return err
	}
	if status != "pending" && status != "running" {
		return errTaskNotCancellable
	}
	// 先查出该 task 下所有已提交的 comfy_prompt_id（一个 task 可能有多个 item）
	var comfyPromptIDs []string
	if rows, err := a.db.QueryContext(ctx, `SELECT comfy_prompt_id FROM generation_items WHERE task_id=? AND COALESCE(comfy_prompt_id,'')!=''`, id); err == nil {
		for rows.Next() {
			var pid string
			if err := rows.Scan(&pid); err == nil && pid != "" {
				comfyPromptIDs = append(comfyPromptIDs, pid)
			}
		}
		rows.Close()
	}
	// 地址取当前设置（同 submitter/downloader：task 快照会因地址变更而失效，
	// 拿废弃地址去删队列只会静默失败，ComfyUI 里排队的任务就撤不掉了）
	comfyURL, _ := a.setting(ctx, "comfyui_url")

	result, err := a.db.ExecContext(ctx, `UPDATE generation_tasks SET status='cancelled', completed_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('pending','running')`, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		// 状态在 SELECT 与 UPDATE 之间被改掉（并发），按不可取消处理
		return errTaskNotCancellable
	}
	// 通知 ComfyUI 从队列中删除（只删排队项；正在执行的不能靠 /interrupt，那会影响同实例的其它任务）
	if len(comfyPromptIDs) > 0 && comfyURL != "" {
		for _, pid := range comfyPromptIDs {
			go a.deleteComfyQueueItem(comfyURL, pid)
		}
	}
	a.forgetSaveFailures(ctx, id)
	a.publish(id, map[string]any{"task_id": id, "status": "cancelled"})
	return nil
}

// retryTaskByID 重跑失败任务：重置 task 与失败 item，成功的 item 保持不动，避免重复出图。
func (a *app) retryTaskByID(ctx context.Context, id int64) error {
	// 先查一次是否存在：只靠下面带 status='failed' 条件的 UPDATE，
	// 「任务不存在」和「任务不是 failed」都只会得到 n==0，接口层没法分别返回 404 / 409。
	var status string
	if err := a.db.QueryRowContext(ctx, `SELECT status FROM generation_tasks WHERE id=?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errTaskNotFound
		}
		return err
	}
	if status != "failed" {
		return errTaskNotFailed
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE generation_tasks SET status='pending', failed_count=0, completed_at=NULL WHERE id=? AND status='failed'`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return errTaskNotFailed
	}
	// 关键：只重置 task 是没用的，submitter 只捞 item.status='pending'。
	// 必须把该 task 下失败的 item 一并重置（成功的 item 保持不动，避免重复出图）。
	if _, err := tx.ExecContext(ctx,
		`UPDATE generation_items SET status='pending', comfy_prompt_id='', error_message='' WHERE task_id=? AND status='failed'`, id); err != nil {
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
func (a *app) deleteTaskByID(ctx context.Context, id int64) error {
	var status string
	if err := a.db.QueryRowContext(ctx, `SELECT status FROM generation_tasks WHERE id=?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errTaskNotFound
		}
		return err
	}
	if status == "running" {
		return errTaskRunning
	}
	// 必须在删 items 之前清计数：删完就查不到 item id 了
	a.forgetSaveFailures(ctx, id)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	_, _ = tx.ExecContext(ctx, `DELETE FROM images WHERE generation_item_id IN (SELECT id FROM generation_items WHERE task_id=?)`, id)
	_, _ = tx.ExecContext(ctx, `DELETE FROM generation_items WHERE task_id=?`, id)
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
	return nil
}
