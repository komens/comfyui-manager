package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type comfySubmitResponse struct {
	PromptID string `json:"prompt_id"`
}

// maxComfyErrorBody 限制读入的拒绝响应体大小（ComfyUI 的错误体不会很大，
// 但这里读的是外部输入，不能无上限）。
const maxComfyErrorBody = 1 << 20

// comfyRejectedError 是 ComfyUI 明确拒绝（非 2xx）时返回的错误。
// 带上原始响应体：调用方要靠它判断能不能自动纠正（见 repair.go 的 planRepairs）。
type comfyRejectedError struct {
	Status int
	Body   []byte
}

func (e *comfyRejectedError) Error() string {
	return fmt.Sprintf("ComfyUI /prompt HTTP %d%s", e.Status, describeComfyRejection(bytes.NewReader(e.Body)))
}

// rejection 解析响应体里的逐节点错误；解析不出来返回零值（调用方按「修不了」处理）。
func (e *comfyRejectedError) rejection() comfyRejection {
	var parsed comfyRejection
	_ = json.Unmarshal(e.Body, &parsed)
	return parsed
}

func submitComfy(ctx context.Context, baseURL string, workflow map[string]any, clientID string) (string, error) {
	body, _ := json.Marshal(map[string]any{"prompt": workflow, "client_id": clientID})
	dumpSubmitPayload(baseURL+"/prompt", clientID, body) // debug 模式：DEBUG 开启时落盘实际载荷
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/prompt", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	response, trace, err := tracedDo(http.DefaultClient, req, "prompt", maxComfyErrorBody)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// 把响应体读进来再交出去，而不是直接喂给 describeComfyRejection：
		// 自动纠正要用原始结构（哪个节点、哪个字段），读完就没得读了。
		raw, _ := io.ReadAll(io.LimitReader(response.Body, maxComfyErrorBody))
		return "", &comfyRejectedError{Status: response.StatusCode, Body: raw}
	}
	var result comfySubmitResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || result.PromptID == "" {
		trace.emit("response has no prompt_id", nil)
		return "", fmt.Errorf("ComfyUI response has no prompt_id")
	}
	trace.emit("prompt_id="+result.PromptID, nil)
	return result.PromptID, nil
}

// submitComfyRepairing 提交；若被 ComfyUI 以「取值不在列表」拒绝，就把串位的控件值
// 挪回它真正属于的输入、腾空的字段补上默认值，然后重试一次（详见 repair.go）。
//
// 只试一次：纠正本身是确定性的一步，重试还失败说明不是串位问题，再试没有意义。
// 返回的 notes 是这次自动改了什么（可能为空），交给调用方落库——自动改值必须可追溯。
func submitComfyRepairing(ctx context.Context, baseURL string, workflow map[string]any, clientID string) (string, []string, error) {
	promptID, err := submitComfy(ctx, baseURL, workflow, clientID)
	if err == nil {
		return promptID, nil, nil
	}
	var rejected *comfyRejectedError
	if !errors.As(err, &rejected) {
		return "", nil, err // 连不上/响应异常，不是取值问题，纠正无从下手
	}
	repairs := planRepairs(workflow, rejected.rejection(), newRepairLookup(baseURL))
	if len(repairs) == 0 {
		// 修不了也要留现场：值已经没有归属（模型被删）或有歧义（多个字段都接受）时，
		// 这张工作流是一提交就必失败，而失败原因只有 ComfyUI 那句「不在候选里」。
		debugEvent("repair_not_possible", map[string]any{
			"rejection": string(rejected.Body),
		})
		return "", nil, err
	}
	notes := make([]string, 0, len(repairs))
	for _, repair := range repairs {
		notes = append(notes, repair.String())
	}
	retriedID, retryErr := submitComfy(ctx, baseURL, workflow, clientID)
	debugEvent("repair_applied", map[string]any{
		"rejection": string(rejected.Body),
		"repairs":   repairDetails(repairs),
		"retry_ok":  retryErr == nil,
		"retry_err": errorText(retryErr),
	})
	if retryErr != nil {
		return "", notes, fmt.Errorf("%v；已自动纠正（%s）后重试仍失败：%v", err, strings.Join(notes, "；"), retryErr)
	}
	return retriedID, notes, nil
}

// ============================================================================
// 提交被拒时，把 ComfyUI 的原话带回 error_message
//
// 背景：ComfyUI 对非法载荷返回 400，响应体里逐节点写明了「哪个字段、错在哪、
// 合法候选是什么」（node_errors[i].errors[j].details）。旧实现只回一句
// "ComfyUI /prompt HTTP 400"，把这份可以直接照抄修复的信息整个丢掉，
// 导致每次提交失败都要人工复刻 payload 重放一遍才能定位到字段。
//
// 这里只做「提炼」不做「判断」：认得出的结构压成一行人类可读文本，
// 认不出就退回响应体原文片段——绝不因为解析失败而把信息丢掉。
// ============================================================================

// maxComfyErrorDetail 限制单条错误详情的长度（按 rune 截断，不切断 UTF-8）。
// 模型名候选列表可能有几十项，不设上限会把 error_message 撑成一篇长文。
const maxComfyErrorDetail = 400

type comfyErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Details string `json:"details"`
}

// comfyErrorDetail 是某个节点上的一条具体错误。
//
// ExtraInfo.input_name 是「哪个输入」的权威来源：details 只是一句话，
// 要从里面抠字段名得靠猜格式。新版 ComfyUI 会带上它，老版没有——两条路都要留。
type comfyErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Details string `json:"details"`
	// ExtraInfo 只在部分错误类型上有内容，解不出来就是零值。
	ExtraInfo struct {
		InputName string `json:"input_name"`
	} `json:"extra_info"`
}

type comfyNodeError struct {
	ClassType string             `json:"class_type"`
	Errors    []comfyErrorDetail `json:"errors"`
}

type comfyRejection struct {
	Error      comfyErrorBody            `json:"error"`
	NodeErrors map[string]comfyNodeError `json:"node_errors"`
}

// describeComfyRejection 读 ComfyUI 的错误响应体，返回以 ": " 开头的可读原因。
// 读不动或认不出结构时返回 ": <原文片段>"；响应体为空则返回 ""，
// 让调用方的错误信息退化成原来的 "HTTP 400"。
func describeComfyRejection(reader io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}

	var rejection comfyRejection
	if err := json.Unmarshal(raw, &rejection); err != nil {
		return ": " + truncateRunes(string(bytes.TrimSpace(raw)), maxComfyErrorDetail)
	}

	// 节点 id 按数值排序，保证同一份工作流的报错顺序稳定可复现。
	ids := make([]int, 0, len(rejection.NodeErrors))
	for rawID := range rejection.NodeErrors {
		if id, err := strconv.Atoi(rawID); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)

	parts := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		nodeErr := rejection.NodeErrors[strconv.Itoa(id)]
		for _, detail := range nodeErr.Errors {
			text := strings.TrimSpace(detail.Details)
			if text == "" {
				text = strings.TrimSpace(detail.Message)
			}
			if text == "" {
				text = strings.TrimSpace(detail.Type)
			}
			if text == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("node %d (%s): %s", id, nodeErr.ClassType, truncateRunes(text, maxComfyErrorDetail)))
		}
	}
	if len(parts) == 0 {
		// 非逐节点错误（invalid_prompt 等）走 error.message / error.details；
		// type 只是最后的兜底，前两者有内容时不再堆上去当噪音。
		fallback := strings.Join(joinNonEmpty(rejection.Error.Message, rejection.Error.Details), ": ")
		if fallback == "" {
			fallback = strings.TrimSpace(rejection.Error.Type)
		}
		if fallback != "" {
			parts = append(parts, truncateRunes(fallback, maxComfyErrorDetail))
		}
	}
	if len(parts) == 0 {
		return ": " + truncateRunes(string(bytes.TrimSpace(raw)), maxComfyErrorDetail)
	}
	return ": " + strings.Join(parts, "; ")
}

// joinNonEmpty 返回入参里所有非空字符串，保持原顺序。
func joinNonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// truncateRunes 按 rune 截断字符串，超长追加省略号（不会切断 UTF-8）。
func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// submitter sends pending items using the configuration captured when they were queued.
func (a *app) submitter() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		a.submitPendingItems()
	}
}

func (a *app) submitPendingItems() {
	rows, err := a.db.Query(`SELECT i.id, i.task_id, i.positive_prompt, COALESCE(i.negative_prompt,''),
		t.parameters_json, COALESCE(NULLIF(t.comfyui_url,''),(SELECT value FROM settings WHERE key='comfyui_url'),''), t.workflow_snapshot, COALESCE(w.workflow_path,''),
		CASE WHEN t.workflow_snapshot!='' THEN t.workflow_mapping_snapshot ELSE COALESCE(w.mapping_json,'') END,
		CASE WHEN t.workflow_snapshot!='' THEN t.workflow_negative_snapshot ELSE COALESCE(w.negative_prompt,'') END
		FROM generation_items i
		JOIN generation_tasks t ON t.id=i.task_id
		LEFT JOIN workflows w ON w.id=t.workflow_id
		WHERE i.status='pending' AND t.status IN ('pending','running')
		ORDER BY i.id LIMIT 10`)
	if err != nil {
		a.log.Printf("submitter query error: %v", err)
		return
	}
	type pendingItem struct {
		ItemID           int64
		TaskID           int64
		Positive         string
		ItemNegative     string
		Parameters       string
		BaseURL          string
		WorkflowJSON     string
		WorkflowPath     string
		MappingJSON      string
		WorkflowNegative string
	}
	var items []pendingItem
	for rows.Next() {
		var p pendingItem
		if err := rows.Scan(&p.ItemID, &p.TaskID, &p.Positive, &p.ItemNegative, &p.Parameters, &p.BaseURL, &p.WorkflowJSON, &p.WorkflowPath, &p.MappingJSON, &p.WorkflowNegative); err == nil {
			items = append(items, p)
		}
	}
	rows.Close()
	for _, p := range items {
		negative := resolveEffectiveNegative(p.ItemNegative, p.WorkflowNegative)
		a.submitItem(p.ItemID, p.TaskID, p.Positive, p.BaseURL, p.Parameters, p.WorkflowJSON, p.WorkflowPath, p.MappingJSON, negative)
	}
}

// resolveEffectiveNegative 计算本条提交实际生效的负面词。
//
// 本条自带（prompts / generation_items 的 negative_prompt）非空 → 用它，支持按图换负面；
// 自带为空 → 回落到工作流静态负面列。后者是既有约定：injectDirectPrompt 里「negative
// 传空 = 保留工作流自带负向」，老工作流零行为变化，不要改成清空。
func resolveEffectiveNegative(itemNegative, workflowNegative string) string {
	if strings.TrimSpace(itemNegative) == "" {
		return workflowNegative
	}
	return itemNegative
}

func (a *app) claimPendingItem(ctx context.Context, itemID, taskID int64) (bool, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE generation_items SET status='running' WHERE id=? AND status='pending' AND EXISTS (SELECT 1 FROM generation_tasks WHERE id=? AND status IN ('pending','running'))", itemID, taskID)
	if err != nil {
		return false, err
	}
	if changed, err := result.RowsAffected(); changed == 0 || err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE generation_tasks SET status='running', started_at=COALESCE(started_at,?) WHERE id=? AND status='pending'", time.Now(), taskID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// submitItem 提交单个 item 到 ComfyUI
func (a *app) submitItem(itemID, taskID int64, positive, baseURL, parameters, workflowJSON, workflowPath, mappingJSON, negativePrompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	claimed, err := a.claimPendingItem(ctx, itemID, taskID)
	if err != nil {
		a.log.Printf("claim item %d: %v", itemID, err)
	}
	if !claimed {
		return
	}
	a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "running"})

	workflowBytes := []byte(workflowJSON)
	if workflowJSON == "" {
		// Historical tasks predate workflow snapshots.
		var err error
		workflowBytes, err = readWorkflowFile(a.workflowFilePath(workflowPath))
		if err != nil {
			a.failItem(itemID, taskID, "找不到该任务的工作流，请选择现有工作流重新提交")
			return
		}
	}
	var workflow map[string]any
	if err := json.Unmarshal(workflowBytes, &workflow); err != nil || workflow == nil {
		a.failItem(itemID, taskID, "提交时的工作流 JSON 缺失或不可读，请修复工作流后重试")
		return
	}
	if _, hasNodes := workflow["nodes"]; hasNodes {
		// 传入节点定义查询器：早期导出格式的工作流只留了裸位置数组，
		// 没有它就没法还原控件值（详见 widgetorder.go）。查询是按需触发的，
		// 新格式的工作流一次网络请求都不会发。
		workflow, err = uiToAPIFormatWithOrder(workflow, newWidgetSlotLookup(baseURL))
		if err != nil {
			a.failItem(itemID, taskID, fmt.Sprintf("convert workflow format failed: %v", err))
			return
		}
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(parameters), &input); err != nil {
		a.failItem(itemID, taskID, fmt.Sprintf("parse parameters failed: %v", err))
		return
	}
	// 正负向都以 generation_items 的列为准：parameters_json 是建任务时的快照副本，
	// 而任务详情 / 图片详情展示的是 item 列。只读 parameters_json 会让「页面上看到的正向」
	// 和「实际提交的正向」靠巧合保持一致；负面本来就走 item 列，两列同源才不会各说各话。
	input["positive_prompt"] = positive
	input["negative_prompt"] = negativePrompt
	actualNegative, err := injectPromptAndSeed(workflow, mappingJSON, input)
	if err != nil {
		a.failItem(itemID, taskID, fmt.Sprintf("inject prompt failed: %v", err))
		return
	}
	comfyPromptID, notes, err := submitComfyRepairing(ctx, baseURL, workflow, fmt.Sprintf("comfyui-server-task-%d", taskID))
	if err != nil {
		a.failItem(itemID, taskID, fmt.Sprintf("submit to ComfyUI failed: %v", err))
		return
	}
	note := ""
	if len(notes) > 0 {
		note = "提交时自动纠正了导出串位的控件值——" + strings.Join(notes, "；")
		a.log.Printf("item %d %s", itemID, note)
	}
	if !a.recordComfySubmission(itemID, taskID, baseURL, comfyPromptID, note, actualNegative) {
		return
	}
	a.log.Printf("item %d submitted to ComfyUI (prompt_id=%s)", itemID, comfyPromptID)
	debugEvent("item_submitted", map[string]any{
		"item_id":         itemID,
		"task_id":         taskID,
		"comfy_prompt_id": comfyPromptID,
		"notes":           strings.Join(notes, "；"),
	})
}

// recordComfySubmission handles cancellation that wins while ComfyUI is answering /prompt.
func (a *app) recordComfySubmission(itemID, taskID int64, baseURL, promptID, note, negative string) bool {
	result, err := a.db.Exec(`UPDATE generation_items SET status='submitted', comfy_prompt_id=?, submitted_comfyui_url=?, notes=?, effective_negative_prompt=?, remote_missing_since=0, remote_failure_since=0
		WHERE id=? AND task_id=? AND status='running'
		AND EXISTS (SELECT 1 FROM generation_tasks WHERE id=? AND status='running')`, promptID, baseURL, note, negative, itemID, taskID, taskID)
	if err != nil {
		a.failItem(itemID, taskID, "ComfyUI 已接收任务，但保存提交记录失败："+err.Error())
		go a.cancelUnrecordedComfyJob(itemID, baseURL, promptID)
		return false
	}
	if n, _ := result.RowsAffected(); n == 0 {
		go a.cancelUnrecordedComfyJob(itemID, baseURL, promptID)
		return false
	}
	a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "submitted"})
	return true
}

// failItem 标记 item 失败
func (a *app) failItem(itemID, taskID int64, errMsg string) {
	a.log.Printf("item %d failed: %s", itemID, errMsg)
	// docker logs 会滚、也可能没留在手边，所以失败原因同时进事件流，
	// 这样「task 是什么时候、因为什么失败的」在任何时候都查得到。
	debugEvent("item_failed", map[string]any{
		"item_id": itemID,
		"task_id": taskID,
		"error":   errMsg,
	})
	a.finishItem(itemID, taskID, "failed", errMsg)
}

// finishItem applies the item outcome and task totals together, preserving cancellation.
func (a *app) finishItem(itemID, taskID int64, status, message string) {
	finished, err := a.persistItemOutcome(itemID, taskID, status, message)
	if err != nil {
		a.log.Printf("item %d save %s outcome: %v", itemID, status, err)
		return
	}
	a.resetSaveFailure(itemID)
	if finished {
		a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": status, "error": message})
	}
}

func (a *app) persistItemOutcome(itemID, taskID int64, status, message string) (bool, error) {
	tx, err := a.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE generation_items SET status=?, error_message=?, remote_failure_since=0 WHERE id=? AND status IN ('running','submitted') AND EXISTS (SELECT 1 FROM generation_tasks WHERE id=? AND status!='cancelled')",
		status, message, itemID, taskID)
	if err != nil {
		return false, err
	}
	if changed, err := result.RowsAffected(); changed == 0 || err != nil {
		return false, err
	}
	promptStatus := status
	if status == "success" {
		promptStatus = "done"
	}
	_, err = tx.Exec("UPDATE prompts SET status=?, updated_at=CURRENT_TIMESTAMP, completed_at=CASE WHEN ?='done' THEN CURRENT_TIMESTAMP ELSE completed_at END WHERE id=(SELECT prompt_id FROM generation_items WHERE id=?)",
		promptStatus, promptStatus, itemID)
	if err != nil {
		return false, err
	}
	var pending, succeeded, failed int
	// 「未完成」要显式枚举，不能写 status NOT IN ('success','failed')：那样 cancelled 也会
	// 被算成未完成，一旦将来支持取消单条生成项，任务就会永远停在 running。
	err = tx.QueryRow("SELECT COUNT(*) FILTER (WHERE status IN ('pending','running','submitted')), COUNT(*) FILTER (WHERE status='success'), COUNT(*) FILTER (WHERE status='failed') FROM generation_items WHERE task_id=?", taskID).
		Scan(&pending, &succeeded, &failed)
	if err != nil {
		return false, err
	}
	taskStatus := "running"
	var completedAt any
	if pending == 0 {
		taskStatus = "completed"
		if succeeded == 0 {
			taskStatus = "failed"
		}
		completedAt = time.Now()
	}
	_, err = tx.Exec("UPDATE generation_tasks SET status=?, success_count=?, failed_count=?, completed_at=? WHERE id=?", taskStatus, succeeded, failed, completedAt, taskID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// maxSaveAttempts 是「图已生成、但保存失败」的连续重试上限。
// downloader 每 5s 一轮，40 轮≈200s；只有持续故障（磁盘满、库一直锁）才会触发判失败，
// 避免把可恢复错误当成「无输出」直接判死。
const maxSaveAttempts = 40

// bumpSaveFailure 记录一次保存失败，返回该 item 的连续失败次数。
func (a *app) bumpSaveFailure(itemID int64) int {
	a.saveFailMu.Lock()
	defer a.saveFailMu.Unlock()
	a.saveFailures[itemID]++
	return a.saveFailures[itemID]
}

// resetSaveFailure 在保存成功后清除该 item 的失败计数。
func (a *app) resetSaveFailure(itemID int64) {
	a.saveFailMu.Lock()
	defer a.saveFailMu.Unlock()
	delete(a.saveFailures, itemID)
}

// saveFailureIDs 返回当前处于「保存失败重试」中的 item id。
// downloader 用它把这些 item 排到取数队列末尾，避免它们长期霸占每轮的名额。
const downloadBatchSize = 20

func (a *app) downloader() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var afterID int64
	for range ticker.C {
		// 旧记录未保存实际提交地址，沿用旧版按当前配置查询的行为。
		rows, err := a.db.Query(`SELECT i.id, i.task_id, i.comfy_prompt_id,
			COALESCE(NULLIF(i.submitted_comfyui_url,''), NULLIF(t.comfyui_url,''),(SELECT value FROM settings WHERE key='comfyui_url'),'')
			FROM generation_items i JOIN generation_tasks t ON t.id=i.task_id
			WHERE i.id>? AND i.status='submitted' AND i.comfy_prompt_id!='' AND t.status!='cancelled'
			ORDER BY i.id LIMIT ?`, afterID, downloadBatchSize)
		if err != nil {
			a.log.Printf("downloader query: %v", err)
			continue
		}
		type submission struct {
			itemID, taskID    int64
			promptID, baseURL string
		}
		var items []submission
		for rows.Next() {
			var item submission
			if err := rows.Scan(&item.itemID, &item.taskID, &item.promptID, &item.baseURL); err == nil {
				items = append(items, item)
			}
		}
		if err := rows.Err(); err != nil {
			a.log.Printf("read submitted items: %v", err)
		}
		rows.Close()

		// 轮转游标，避免前面的长任务或故障项长期占满查询批次。
		afterID = 0
		if len(items) == downloadBatchSize {
			afterID = items[len(items)-1].itemID
		}
		var queues comfyQueueCache
		var work sync.WaitGroup
		jobs := make(chan submission, len(items))
		for _, item := range items {
			jobs <- item
		}
		close(jobs)
		for range min(len(items), 4) {
			work.Add(1)
			go func() {
				defer work.Done()
				for item := range jobs {
					a.checkAndDownload(item.itemID, item.taskID, item.promptID, item.baseURL, &queues)
				}
			}()
		}
		work.Wait()
	}
}

// maxWaitingSilenceRounds 卡在 submitted 的 item 每隔这么多轮留一次痕（downloader 5s 一轮）。
//
// 为什么需要它：「任务卡着不动」是这个项目最常见的抱怨，而轮询本身全是有来有回的 200
// ——按设计不该逐条记，于是日志里一片空白，完全看不出「到底还在轮询没有」。
// ComfyUI 重启后可能清空历史。下载器会先向队列核对，再判定结果是否丢失。
const maxWaitingSilenceRounds = 60

// noteWaiting 记一次「这个 item 还在等」。只在 DEBUG 开启时维护计数与落盘。
//
// 第 1 轮不记：刚提交完的那一轮本来就是 pending，记下来每张图都要多一行噪音。
func (a *app) noteWaiting(itemID int64, reason string) {
	if !debugOn {
		return
	}
	a.waitingMu.Lock()
	a.waiting[itemID]++
	round := a.waiting[itemID]
	a.waitingMu.Unlock()
	if round <= 1 || round%maxWaitingSilenceRounds != 0 {
		return
	}
	debugEvent("waiting_for_output", map[string]any{
		"item_id": itemID,
		"rounds":  round,
		"reason":  reason,
	})
}

// clearWaiting 在拿到结果（或判定失败）后清掉等待计数。
func (a *app) clearWaiting(itemID int64) {
	if !debugOn {
		return
	}
	a.waitingMu.Lock()
	delete(a.waiting, itemID)
	a.waitingMu.Unlock()
}

// checkAndDownload 检查单个 item 是否完成并下载图片
type comfyImageOutput struct {
	filename, subfolder, kind string
}

func comfyImageOutputs(outputs map[string]any) []comfyImageOutput {
	var result []comfyImageOutput
	seen := make(map[comfyImageOutput]bool)
	for _, nodeOutput := range outputs {
		node, _ := nodeOutput.(map[string]any)
		images, _ := node["images"].([]any)
		for _, img := range images {
			image, _ := img.(map[string]any)
			filename, _ := image["filename"].(string)
			subfolder, _ := image["subfolder"].(string)
			kind, _ := image["type"].(string)
			output := comfyImageOutput{filename, subfolder, kind}
			if filename != "" && !seen[output] {
				result = append(result, output)
				seen[output] = true
			}
		}
	}
	return result
}

var errComfyOutputInactive = errors.New("generation item is no longer active")

func (a *app) saveComfyOutput(ctx context.Context, itemID int64, promptID, baseURL string, output comfyImageOutput) error {
	// Scope the key to one execution so an explicit rerun can publish its own outputs.
	identity, err := json.Marshal([]any{itemID, promptID, output.kind, output.subfolder, output.filename})
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%x", sha256.Sum256(identity))
	var existingPath string
	err = a.db.QueryRowContext(ctx, "SELECT storage_path FROM images WHERE comfy_output_key=? AND comfy_output_key!=''", key).Scan(&existingPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if info, err := os.Stat(a.imageFilePath(existingPath)); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return nil
	}

	imgData, err := downloadComfyImage(ctx, baseURL, output.filename, output.subfolder, output.kind)
	if err != nil {
		return err
	}
	width, height := imageSizeFromBytes(imgData)
	if width <= 0 || height <= 0 {
		return fmt.Errorf("ComfyUI output %q is not a readable image", output.filename)
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM generation_items i JOIN generation_tasks t ON t.id=i.task_id WHERE i.id=? AND i.status='submitted' AND i.comfy_prompt_id=? AND t.status!='cancelled'", itemID, promptID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return errComfyOutputInactive
	}
	if err != nil {
		return err
	}
	// Recheck under the write lock so a concurrent saver cannot replace an existing file.
	var imageID int64
	err = tx.QueryRowContext(ctx, "SELECT id, storage_path FROM images WHERE comfy_output_key=? AND comfy_output_key!=''", key).Scan(&imageID, &existingPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if info, err := os.Stat(a.imageFilePath(existingPath)); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return nil
	}

	target := filepath.Join(a.dataDir, "images", "comfy-"+key+".png")
	if err := writeAtomic(target, imgData); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(target)
		}
	}()

	digest := fmt.Sprintf("%x", sha256.Sum256(imgData))
	if imageID != 0 {
		_, err = tx.ExecContext(ctx, "UPDATE images SET storage_path=?, image_hash=?, width=?, height=? WHERE id=?", target, digest, width, height, imageID)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, "INSERT INTO images(generation_item_id, filename, storage_path, image_hash, comfy_output_key, width, height, created_at) VALUES(?,?,?,?,?,?,?,?)",
			itemID, output.filename, target, digest, key, width, height, time.Now())
		if err == nil {
			imageID, err = result.LastInsertId()
		}
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	a.queueThumbBuild(imageID, target)
	return nil
}

func (a *app) checkAndDownload(itemID, taskID int64, comfyPromptID, baseURL string, queues *comfyQueueCache) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 检查任务是否被取消
	var taskStatus string
	_ = a.db.QueryRow(`SELECT status FROM generation_tasks WHERE id=?`, taskID).Scan(&taskStatus)
	if taskStatus == "cancelled" {
		a.clearWaiting(itemID)
		return
	}

	// 查询 ComfyUI /history
	history, err := pollComfyHistory(ctx, baseURL, comfyPromptID)
	if err != nil {
		a.noteWaiting(itemID, err.Error())
		if errors.Is(err, errComfyHistoryMissing) {
			queue := queues.get(ctx, baseURL)
			if queue.available {
				a.setRemotePollError(itemID, taskID, comfyPromptID, "")
				if !queue.promptIDs[comfyPromptID] {
					a.checkMissingRemote(ctx, itemID, taskID, comfyPromptID, baseURL)
					return
				}
			} else {
				a.setRemotePollError(itemID, taskID, comfyPromptID, "无法读取 ComfyUI 队列")
			}
		} else {
			a.setRemotePollError(itemID, taskID, comfyPromptID, err.Error())
		}
		a.clearRemoteMissing(itemID, comfyPromptID)
		return
	}
	a.setRemotePollError(itemID, taskID, comfyPromptID, "")
	a.clearRemoteMissing(itemID, comfyPromptID)
	a.clearWaiting(itemID)

	// ComfyUI 自己执行报错时 status.status_str == "error"，此时 outputs 往往是空的。
	// 漏掉这个判断只会报 "no images in ComfyUI output"——那句错误完全不提真实原因，
	// 曾经把排查方向带偏到下载逻辑上（waitAndDownload 里本来是有这个检查的）。
	if statusMap, _ := history["status"].(map[string]any); statusMap != nil {
		if statusStr, _ := statusMap["status_str"].(string); statusStr == "error" {
			// 报给用户的只有一句话，真正的原因（哪个节点、什么异常）在
			// history.status.messages 里，全扔了就没法查了。
			debugEvent("comfy_execution_error", map[string]any{
				"item_id":         itemID,
				"task_id":         taskID,
				"comfy_prompt_id": comfyPromptID,
				"history":         comfyHistoryDigest(history),
			})
			a.failItem(itemID, taskID, "ComfyUI 执行失败(status=error)：通常是工作流某个节点报错，请到 ComfyUI 控制台查看详情")
			return
		}
	}

	// 提取输出图片
	outputs, _ := history["outputs"].(map[string]any)
	if outputs == nil {
		a.failItem(itemID, taskID, "ComfyUI response missing outputs")
		return
	}
	expected := comfyImageOutputs(outputs)
	if len(expected) == 0 {
		// 「ComfyUI 说做完了，但输出里没有图」最容易被误判成下载逻辑的锅。
		// outputs 原样留下来才能看出到底是哪个节点没产出、还是产出被静音了。
		debugEvent("no_outputs", map[string]any{
			"item_id":         itemID,
			"task_id":         taskID,
			"comfy_prompt_id": comfyPromptID,
			"history":         comfyHistoryDigest(history),
		})
		a.failItem(itemID, taskID, "no images in ComfyUI output")
		return
	}
	var saveErr error
	saved := 0
	for _, output := range expected {
		err := a.saveComfyOutput(ctx, itemID, comfyPromptID, baseURL, output)
		if errors.Is(err, errComfyOutputInactive) {
			return
		}
		if err != nil {
			saveErr = err
			continue
		}
		saved++
	}
	progress, err := a.db.Exec("UPDATE generation_items SET output_count=?, saved_output_count=? WHERE id=? AND status='submitted' AND comfy_prompt_id=? AND (output_count!=? OR saved_output_count!=?)",
		len(expected), saved, itemID, comfyPromptID, len(expected), saved)
	if err != nil {
		a.log.Printf("item %d update save progress: %v", itemID, err)
		return
	}
	if saveErr != nil {
		n := a.bumpSaveFailure(itemID)
		if n >= maxSaveAttempts {
			a.failItem(itemID, taskID, fmt.Sprintf("已保存 %d/%d 张图片，连续 %d 次保存未完成：%v", saved, len(expected), n, saveErr))
			return
		}
		if changed, _ := progress.RowsAffected(); changed > 0 {
			a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "submitted"})
		}
		a.log.Printf("item %d 已保存 %d/%d 张图片（第 %d 次），等待重试: %v", itemID, saved, len(expected), n, saveErr)
		debugEvent("save_retry", map[string]any{
			"item_id": itemID, "attempt": n, "saved": saved, "total": len(expected), "error": saveErr.Error(),
		})
		return
	}
	a.finishItem(itemID, taskID, "success", "")
}

// Only mark a job lost when both endpoints repeatedly show it missing.
const remoteMissingGrace = time.Minute

var errComfyHistoryMissing = errors.New("prompt not found in history")

func (a *app) setRemotePollError(itemID, taskID int64, promptID, message string) {
	var since int64
	if message != "" {
		since = time.Now().Unix()
		message = "ComfyUI 连接或响应异常，正在等待恢复：" + message
	}
	result, err := a.db.Exec(`UPDATE generation_items
		SET remote_failure_since=CASE WHEN ?=0 THEN 0 WHEN remote_failure_since=0 THEN ? ELSE remote_failure_since END,
			error_message=?
		WHERE id=? AND status='submitted' AND comfy_prompt_id=? AND error_message!=?`,
		since, since, message, itemID, promptID, message)
	if err != nil {
		a.log.Printf("record remote polling state for item %d: %v", itemID, err)
		return
	}
	if changed, _ := result.RowsAffected(); changed > 0 {
		a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "submitted", "error": message})
	}
}

type comfyQueueCache struct {
	entries sync.Map
}

type comfyQueueCacheEntry struct {
	once  sync.Once
	queue comfyQueueSnapshot
}

func (c *comfyQueueCache) get(ctx context.Context, baseURL string) comfyQueueSnapshot {
	value, _ := c.entries.LoadOrStore(baseURL, &comfyQueueCacheEntry{})
	entry := value.(*comfyQueueCacheEntry)
	entry.once.Do(func() {
		// Keep capacity for the history recheck even if the queue endpoint is slow.
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		entry.queue = fetchComfyQueue(ctx, baseURL)
	})
	return entry.queue
}

type comfyQueueSnapshot struct {
	available bool
	promptIDs map[string]bool
	running   map[string]bool
}

func fetchComfyQueue(ctx context.Context, baseURL string) comfyQueueSnapshot {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/queue", nil)
	if err != nil {
		return comfyQueueSnapshot{}
	}
	resp, _, err := tracedDo(http.DefaultClient, req, "queue status", 2048)
	if err != nil {
		return comfyQueueSnapshot{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return comfyQueueSnapshot{}
	}
	var queue struct {
		Running [][]json.RawMessage `json:"queue_running"`
		Pending [][]json.RawMessage `json:"queue_pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queue); err != nil || queue.Running == nil || queue.Pending == nil {
		return comfyQueueSnapshot{}
	}
	result := comfyQueueSnapshot{available: true, promptIDs: make(map[string]bool), running: make(map[string]bool)}
	for index, entries := range [][][]json.RawMessage{queue.Running, queue.Pending} {
		for _, entry := range entries {
			var promptID string
			if len(entry) < 2 || json.Unmarshal(entry[1], &promptID) != nil || promptID == "" {
				return comfyQueueSnapshot{}
			}
			result.promptIDs[promptID] = true
			if index == 0 {
				result.running[promptID] = true
			}
		}
	}
	return result
}

func (a *app) clearRemoteMissing(itemID int64, promptID string) {
	if _, err := a.db.Exec("UPDATE generation_items SET remote_missing_since=0 WHERE id=? AND comfy_prompt_id=? AND remote_missing_since!=0", itemID, promptID); err != nil {
		a.log.Printf("clear missing remote state for item %d: %v", itemID, err)
	}
}

func (a *app) checkMissingRemote(ctx context.Context, itemID, taskID int64, promptID, baseURL string) {
	now := time.Now()
	var since int64
	err := a.db.QueryRowContext(ctx, `UPDATE generation_items
		SET remote_missing_since=CASE WHEN remote_missing_since=0 THEN ? ELSE remote_missing_since END
		WHERE id=? AND comfy_prompt_id=? AND status='submitted' RETURNING remote_missing_since`, now.Unix(), itemID, promptID).Scan(&since)
	if err != nil || now.Sub(time.Unix(since, 0)) < remoteMissingGrace {
		return
	}
	// A job can finish between the first history and queue requests.
	if _, err := pollComfyHistory(ctx, baseURL, promptID); !errors.Is(err, errComfyHistoryMissing) {
		a.clearRemoteMissing(itemID, promptID)
		return
	}
	a.failItem(itemID, taskID, "ComfyUI 队列和历史记录均找不到这条任务。ComfyUI 可能已重启或清空队列，可点击重试重新提交。")
}

// pollComfyHistory 查询 /history/{prompt_id} 来检查任务是否完成
//
// 这条路是「任务卡着不动」的第一现场，但它每 5 秒被走一遍，
// 所以只在真正异常时才留痕：非 2xx 由 tracedDo 自动记，
// 解码失败在这里补记。204/404 与「history 里查不到这个 prompt_id」
// 都是正常的「还没轮到」——记下来只会把日志淹掉。
func pollComfyHistory(ctx context.Context, baseURL, promptID string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/history/"+promptID, nil)
	if err != nil {
		return nil, err
	}
	resp, trace, err := tracedDo(http.DefaultClient, req, "history", maxDebugBodyBytes)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 || resp.StatusCode == 404 {
		return nil, errComfyHistoryMissing
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("history HTTP %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		trace.emit("history decode failed", nil)
		return nil, err
	}
	if _, ok := result[promptID]; !ok {
		return nil, errComfyHistoryMissing
	}
	promptResult, _ := result[promptID].(map[string]any)
	if promptResult == nil {
		trace.emit("history entry is not an object", nil)
		return nil, fmt.Errorf("invalid prompt result")
	}
	return promptResult, nil
}

func downloadComfyImage(ctx context.Context, baseURL, filename, subfolder, imgType string) ([]byte, error) {
	// 查询串必须逐参数编码：ComfyUI 侧是 aiohttp，原始非 ASCII 字节（中文 SaveImage
	// 前缀就会造出这种文件名）以及 & # 空格都会在解析层直接被拒，返回
	// “Invalid char in url query” 的 400，请求根本进不到 handler。表现是图取不回来、
	// 生成项一直停在 submitted，任务永远 running。
	query := url.Values{"filename": {filename}, "subfolder": {subfolder}, "type": {imgType}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/view?%s", baseURL, query.Encode()), nil)
	if err != nil {
		return nil, err
	}
	// errBodyLimit=0：响应体是图片，记进日志没有任何意义，只会把文件撑大
	resp, trace, err := tracedDo(http.DefaultClient, req, "view", 0)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ComfyUI view HTTP %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		trace.emit("read image body failed", map[string]any{"filename": filename})
		return nil, err
	}
	// 成功也记：出图数量与每张大小是「有没有真的拿到图」的直接证据
	trace.emit("downloaded", map[string]any{"filename": filename, "bytes": buf.Len()})
	return buf.Bytes(), nil
}

// promptMapping 是 workflows.mapping_json 的结构。
//
// 这里只存「正负向提示词写到哪」，没有 parameters / seed / output_prefix ——
// 工作流里其余的一切（模型、尺寸、步数、文件名前缀、连线）都不再由提交阶段改写，
// 工作流 JSON 是唯一真源。过去每个标量字段都会在这里存一份默认值并在提交时全量
// 写回，两份副本一旦漂移就会把工作流里的真实值覆盖掉：典型症状是「在 ComfyUI 里
// 换了主模型、重新导入，提交时又被改回旧模型」。
//
// Field 存裸字段名（如 "text"）；历史上存过 "inputs.text"，读取时两种都认。
type promptMapping struct {
	Positive promptTarget `json:"positive_prompt"`
	Negative promptTarget `json:"negative_prompt"`
}

// barePromptField 兼容历史数据里带 "inputs." 前缀的写法。
func barePromptField(field string) string {
	return strings.TrimPrefix(strings.TrimSpace(field), "inputs.")
}

// stringFieldValue 判断 inputs 里某字段是不是字符串（即真的有一个文本框）。
func stringFieldValue(inputs map[string]any, field string) (string, bool) {
	if field == "" {
		return "", false
	}
	text, ok := inputs[field].(string)
	return text, ok
}

// pinnedTarget 把编辑器里选好的节点/字段转成落点。
//
// 选中的节点不在这份工作流里（工作流被换过、节点被删）时返回 nil，
// 调用方退回自动解析——宁可自动解析，也不要写进一个不存在的节点。
func pinnedTarget(workflow map[string]any, pinned promptTarget, role promptRole) *promptTarget {
	if pinned.NodeID == "" {
		return nil
	}
	node, ok := workflow[pinned.NodeID].(map[string]any)
	if !ok {
		return nil
	}
	inputs := nodeInputs(node)
	fields := promptFieldsOf(inputs)
	field := barePromptField(pinned.Field)
	if _, ok := stringFieldValue(inputs, field); !ok {
		// 字段没配、或配的字段在这份工作流上不是文本框（工作流被改过）→ 按角色重挑
		field = pickPromptField(fields, role)
	}
	if field == "" {
		return nil
	}
	return &promptTarget{NodeID: pinned.NodeID, Field: field}
}

// resolveTarget 决定某一侧的落点：编辑器里选过就用选的，没选就按结构自动解析。
func resolveTarget(workflow map[string]any, pinned promptTarget, auto *promptTarget, role promptRole) *promptTarget {
	if target := pinnedTarget(workflow, pinned, role); target != nil {
		return target
	}
	return auto
}

// injectDirectPrompt 把正向 / 负向提示词写进工作流，外加一个随机种子。
//
// 只做这两件事。工作流的模型、尺寸、步数、输出文件名、连线一律原样提交。
//
// 落点优先取 mapping（编辑器里选好的），mapping 没配就按结构自动解析
// （见 promptnodes.go）。正向写不进去时**直接报错**而不是静默跳过——
// 静默跳过会让人以为提示词生效了，出来的却是工作流自带的旧内容。
func injectDirectPrompt(workflow map[string]any, mappingJSON string, input map[string]any) error {
	_, err := injectPromptAndSeed(workflow, mappingJSON, input)
	return err
}

func injectPromptAndSeed(workflow map[string]any, mappingJSON string, input map[string]any) (string, error) {
	var mapping promptMapping
	if trimmed := strings.TrimSpace(mappingJSON); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &mapping); err != nil {
			return "", fmt.Errorf("解析节点映射失败：%v", err)
		}
	}

	positive, _ := input["positive_prompt"].(string)
	negative, _ := input["negative_prompt"].(string)

	auto := resolvePromptTargets(workflow)
	positiveTarget := resolveTarget(workflow, mapping.Positive, auto.Positive, rolePositive)
	negativeTarget := resolveTarget(workflow, mapping.Negative, auto.Negative, roleNegative)

	// 正负向落到同一个节点的同一个字段：工作流把负向从正向 zero-out 派生出来，
	// 没有独立的负向文本框（Krea2 全系就是这样）。这时只能留正向，
	// 否则后写的负向会把正向盖掉。
	if positiveTarget != nil && negativeTarget != nil &&
		positiveTarget.NodeID == negativeTarget.NodeID && positiveTarget.Field == negativeTarget.Field {
		negativeTarget = nil
	}

	if positive != "" && positiveTarget == nil {
		return "", fmt.Errorf("未能确定正向提示词落点：这份工作流里找不到正向提示词节点，请到「工作流」页选中节点后再提交")
	}
	if positiveTarget != nil && positive != "" {
		if err := writeTargetField(workflow, positiveTarget, positive); err != nil {
			return "", err
		}
	}
	// negative 为空表示「保留工作流自带的负向」，这是既有约定，不要改成清空。
	if negativeTarget != nil && negative != "" {
		if err := writeTargetField(workflow, negativeTarget, negative); err != nil {
			return "", err
		}
	}

	// 种子：工作流里的 seed 是导出时的固定值，照原样提交会让「重跑」出一模一样的图。
	// 这里统一换成随机值；显式传了 seed 就用传的。
	seedValue, hasSeed := input["seed"]
	if !hasSeed || seedValue == nil || seedValue == float64(0) {
		seedValue = rand.Int63n(999999999999999)
	}
	autoInjectSeed(workflow, seedValue)
	if negativeTarget == nil {
		return "", nil
	}
	node, _ := workflow[negativeTarget.NodeID].(map[string]any)
	fields, _ := node["inputs"].(map[string]any)
	actualNegative, _ := stringFieldValue(fields, negativeTarget.Field)
	return actualNegative, nil
}

// writeTargetField 把值写进落点。落点字段为空（老格式离线解析的结果）算失败。
func writeTargetField(workflow map[string]any, target *promptTarget, value string) error {
	if target.Field == "" {
		return fmt.Errorf("节点 %s 的提示词字段未能确定，请到「工作流」页手动选择", target.NodeID)
	}
	return setWorkflowField(workflow, map[string]string{
		"node_id": target.NodeID,
		"field":   "inputs." + target.Field,
	}, value)
}

// autoInjectSeed 把种子写进工作流里所有「吃标量 seed 的节点」。
//
// 不再只认 KSampler / KSamplerAdvanced：自定义采样器、Noise 之类的节点名字五花八门，
// 按名字枚举必然漏。判据改成「inputs 里有非连线的 seed 字段」，与节点类型无关。
func autoInjectSeed(workflow map[string]any, seed any) {
	for _, id := range seedInputNodes(workflow) {
		node, _ := workflow[id].(map[string]any)
		nodeInputs(node)["seed"] = seed
	}
}

func setWorkflowField(workflow map[string]any, location map[string]string, value any) error {
	if location["node_id"] == "" || location["field"] == "" {
		return nil
	}
	node, ok := workflow[location["node_id"]].(map[string]any)
	if !ok {
		return fmt.Errorf("workflow node %s not found", location["node_id"])
	}
	parts := strings.Split(location["field"], ".")
	current := node
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return fmt.Errorf("workflow field %s not found", location["field"])
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
	return nil
}

// ============================================================================
// UI 导出格式 → /prompt 载荷
//
// ComfyUI 前端（目标机上是 1.52.7）在把画布转成载荷时，除了填 widget 值，还会做
// 三件我们过去没做的事。每一件都足以让「在 ComfyUI 里跑得好好的工作流」在本系统
// 提交时被 400 拒掉：
//
//  1. 剔除「前端专用节点」——Note / MarkdownNote / Reroute / PrimitiveNode。
//     它们只存在于前端，服务端 object_info 里查不到，照原样提交必得
//     missing_node_type: Node 'MarkdownNote' not found。
//  2. 剔除 mode==NEVER(2)（静音）与 mode==BYPASS(4)（旁路）的节点。
//  3. 清掉指向「已被剔除节点」的连线项，避免载荷里留着悬空引用。
//
// 前端 graphToPrompt 的对应片段（1.52.7）：
//
//	for (let e of i.values()) {
//	  if (e.isVirtualNode || e.mode === NEVER || e.mode === BYPASS) continue
//	  ...
//	}
//	for (let {inputs: e} of Object.values(a))
//	  for (let [t, n] of Object.entries(e))
//	    if (Array.isArray(n) && n.length === 2 && !a[n[0]]) delete e[t]
//
// 被剔除节点上的连线还要「穿过去」接到真正的上游（旁路/穿透按同序号槽位直连），
// 否则下游会因为拿不到输入而变成 required_input_missing。
// ============================================================================

const (
	// nodeModeNever / nodeModeBypass 与前端枚举一致：
	// ALWAYS=0, ON_EVENT=1, NEVER=2, ON_TRIGGER=3, BYPASS=4
	nodeModeNever  = 2
	nodeModeBypass = 4

	// maxLinkHops 限制连线穿透的层数，防止成环工作流把转换拖死。
	maxLinkHops = 32
)

// errUnsupportedWorkflowFeature 表示无法还原导出文件中的前端结构。
// 这类工作流在 ComfyUI 界面里能跑通，但本系统必须明确报错——照原样发出去只会换来
// 一句含糊的 400，静默丢弃又会跑出一张错图。校验接口据此把它和「工作流本身写错」区分开。
var errUnsupportedWorkflowFeature = errors.New("工作流包含本系统尚不支持的结构")

// frontendOnlyWidgetNames 是 ComfyUI 前端自己的控件，不是节点输入，必须剔除。
//
//	go 名下 —— 老格式里紧跟在 seed 后面；新格式里也一并导出。
//	upload —— LoadImage 的「选择文件」按钮，值是 "image"。
//
// 发进载荷后 ComfyUI 会当成未知输入。过去只剔了前一个，LoadImage 的
// upload 会一路带到服务端——所有图生图工作流都会踩到。
var frontendOnlyWidgetNames = map[string]bool{
	"control_after_generate": true,
	"upload":                 true,
}

// frontendOnlyNodeTypes 是只在 ComfyUI 前端存在的节点类型，服务端一律没有。
// 新前端会在虚拟节点上打 isVirtualNode 标记（见 nodeSkipReason），这里的清单
// 只是老导出的兜底——老导出没有那个标记，只能按类型名认。
var frontendOnlyNodeTypes = map[string]bool{
	"Note":          true,
	"MarkdownNote":  true,
	"Reroute":       true,
	"PrimitiveNode": true,
	"NodeGroup":     true,
}

// nodeSkipReason 判断节点会不会进入提交载荷；返回空串表示照常提交。
// 判据与前端 graphToPrompt 的 `if (isVirtualNode || mode===NEVER || mode===BYPASS) continue` 对齐。
func nodeSkipReason(node map[string]any) string {
	if mode, ok := numericValue(node["mode"]); ok {
		switch int(mode) {
		case nodeModeNever:
			return "静音（mode=2）"
		case nodeModeBypass:
			return "旁路（mode=4）"
		}
	}
	// 新前端给虚拟节点打的标记，比类型名清单更可靠：装了新的前端专用节点
	// （Reroute 的同类物）时，服务端 object_info 里查不到，照原样发出去必得
	// missing_node_type，而清单不可能预先收录所有类型。
	if virtual, ok := node["isVirtualNode"].(bool); ok && virtual {
		return "前端专用节点"
	}
	if virtual, ok := node["virtualNode"].(bool); ok && virtual {
		return "前端专用节点"
	}
	if classType, _ := node["type"].(string); frontendOnlyNodeTypes[classType] {
		return "前端专用节点"
	}
	return ""
}

// linkEndpoint 是 UI 格式 links 表里一条连线的上游端点。
type linkEndpoint struct {
	originID   int
	originSlot int
}

// uiToAPIFormat 将 ComfyUI UI 导出格式转换为 /prompt API 要求的格式
// UI格式: {nodes:[{id,type,widgets_values,inputs}], links:[...]}
// API格式: {"node_id": {class_type, inputs: {name: value_or_link}}}
//
// 不带节点定义，等价于 uiToAPIFormatWithOrder(ui, nil)：只有「inputs 里带 widget
// 标记」的老格式能靠文件自身还原控件值。更老的那种（连标记都没有）必须传 order。
func uiToAPIFormat(ui map[string]any) (map[string]any, error) {
	return uiToAPIFormatWithOrder(ui, nil)
}

// uiToAPIFormatWithOrder 是 uiToAPIFormat 的完整形态。
//
// order 用来还原「裸位置数组」型的控件值（见 widgetorder.go 的说明）：这类老格式
// 的工作流光看文件无法知道 widgets_values 每个位置对应哪个控件，必须借助目标
// ComfyUI 的节点定义。order 为 nil 时该路径不启用。
func uiToAPIFormatWithOrder(ui map[string]any, order widgetOrderLookup) (map[string]any, error) {
	rawNodes, ok := ui["nodes"].([]any)
	if !ok {
		return nil, fmt.Errorf("workflow missing nodes array")
	}
	if definitions := workflowSubgraphs(ui); len(definitions) > 0 {
		return subgraphsToAPI(ui, definitions, order)
	}

	// 先给全部节点建索引（含会被剔除的），连线穿透需要按 id 回溯上游。
	nodes := make(map[int]map[string]any, len(rawNodes))
	ids := make([]int, 0, len(rawNodes))
	for _, raw := range rawNodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, ok := numericValue(node["id"])
		if !ok {
			continue
		}
		if _, dup := nodes[int(id)]; !dup {
			ids = append(ids, int(id))
		}
		nodes[int(id)] = node
	}

	links := buildLinkTable(ui)
	skipped := make(map[int]bool, len(nodes))
	for id, node := range nodes {
		if nodeSkipReason(node) != "" {
			skipped[id] = true
		}
	}

	result := make(map[string]any, len(nodes))
	for _, id := range ids {
		node := nodes[id]
		if skipped[id] {
			continue
		}
		classType, _ := node["type"].(string)
		apiInputs := map[string]any{}

		// ① 连线优先。注意：老格式里「被连线占用的 widget」也带 widget 标记，
		// 这里只按 link 是否存在来判定，不再用 widget 标记把连线筛掉——
		// 否则这类输入会退回用 widgets_values_named 里的旧值，把真实上游覆盖掉。
		for _, raw := range asSlice(node["inputs"]) {
			def, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := def["name"].(string)
			if name == "" {
				continue
			}
			linkID, ok := numericValue(def["link"])
			if !ok || linkID == 0 {
				continue
			}
			originID, originSlot, ok := resolveLinkOrigin(int(linkID), links, nodes, skipped)
			if !ok {
				continue
			}
			apiInputs[name] = []any{strconv.Itoa(originID), originSlot}
		}

		applyUIWidgetValues(apiInputs, node, classType, order)

		result[strconv.Itoa(id)] = map[string]any{
			"class_type": classType,
			"inputs":     apiInputs,
		}
	}

	dropDanglingLinks(result)
	return result, nil
}

func applyUIWidgetValues(apiInputs, node map[string]any, classType string, order widgetOrderLookup) {
	// 优先读取带名称的控件值。widgets_values_named 是两代导出格式共同的权威来源：
	// 新前端（frontendVersion 1.52.x）的 inputs 里只剩连线输入，widget 条目整体
	// 消失，只保留 widgets_values_named。若仍按 widget 标记筛选，所有 widget
	// （unet_name / text / seed / steps / width ...）都会被漏掉，载荷缺必填输入，
	// ComfyUI 直接 400 prompt_outputs_failed_validation。
	namedValues, _ := node["widgets_values_named"].(map[string]any)
	if len(namedValues) > 0 {
		for name, value := range namedValues {
			// control_after_generate / upload 是前端控件，不是节点输入
			// （见 frontendOnlyWidgetNames）。其余键一律原样带上：像
			// SaveImageAdvanced 的 format.bit_depth、TextEncodeQwenImage21 的
			// images.image_1 这类带点的名字是节点自己的输入名，做任何规整或
			// 白名单过滤都会把它们误删。
			if frontendOnlyWidgetNames[name] {
				continue
			}
			// 该输入已被连线占用时以连线为准，widget 里的旧值不再生效。
			if _, occupied := apiInputs[name]; occupied {
				continue
			}
			apiInputs[name] = expandDateTemplate(value)
		}
	} else {
		// 没有 widgets_values_named。剩下两代老格式，区别在于文件里有没有
		// 留下控件顺序的线索：
		//   ① inputs 里挂着 widget 标记 —— 标记顺序就是位置数组的顺序，自带线索；
		//   ② 连标记都没有（早期导出/工具生成）—— 文件本身无从还原，只能拿
		//      目标 ComfyUI 的节点定义（order）去对号入座。
		// 过去只做了 ①，② 会一个控件值都读不出来，整份工作流被误判成
		// 「所有必填输入都缺失」——而它在 ComfyUI 里明明跑得通。
		widgetValues, _ := node["widgets_values"].([]any)
		switch {
		case len(widgetValues) == 0:
			// 没有位置值，无事可做
		case hasWidgetMarkers(node):
			applyMarkedWidgets(apiInputs, node, widgetValues)
		case order != nil:
			if slots, ok := order(classType); ok {
				applyPositionalWidgets(apiInputs, slots, widgetValues)
			}
		}
	}
}

// buildLinkTable 把 UI 格式的 links 数组转成 link_id → 上游端点。
func buildLinkTable(ui map[string]any) map[int]linkEndpoint {
	rawLinks, _ := ui["links"].([]any)
	table := make(map[int]linkEndpoint, len(rawLinks))
	for _, raw := range rawLinks {
		id, origin, slot, _, _, ok := parseUIEdge(raw)
		if ok {
			table[id] = linkEndpoint{originID: origin, originSlot: slot}
		}
	}
	return table
}

// resolveLinkOrigin 顺着连线找到真正会进入载荷的上游节点。
// 上游若是被剔除的节点（旁路/静音/前端专用），就按「同序号槽位直连」继续往上找，
// 与前端旁路穿透的语义一致。找不到可用上游时返回 false，调用方会丢弃这条连线。
func resolveLinkOrigin(linkID int, links map[int]linkEndpoint, nodes map[int]map[string]any, skipped map[int]bool) (int, int, bool) {
	for hop := 0; hop < maxLinkHops; hop++ {
		endpoint, ok := links[linkID]
		if !ok {
			return 0, 0, false
		}
		origin, known := nodes[endpoint.originID]
		if !known {
			return 0, 0, false
		}
		if !skipped[endpoint.originID] {
			return endpoint.originID, endpoint.originSlot, true
		}
		next, ok := inputLinkAtSlot(origin, endpoint.originSlot)
		if !ok {
			return 0, 0, false
		}
		linkID = next
	}
	return 0, 0, false
}

// inputLinkAtSlot 返回节点第 slot 个「带连线」的输入所对应的 link id。
func inputLinkAtSlot(node map[string]any, slot int) (int, bool) {
	seen := 0
	for _, raw := range asSlice(node["inputs"]) {
		def, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		linkID, ok := numericValue(def["link"])
		if !ok || linkID == 0 {
			continue
		}
		if seen == slot {
			return int(linkID), true
		}
		seen++
	}
	return 0, false
}

// dropDanglingLinks 删掉指向「没进载荷的节点」的连线项，
// 与前端 graphToPrompt 收尾的 `if (!a[n[0]]) delete e[t]` 一致。
func dropDanglingLinks(payload map[string]any) {
	for _, raw := range payload {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		inputs, ok := node["inputs"].(map[string]any)
		if !ok {
			continue
		}
		for name, value := range inputs {
			arr, ok := value.([]any)
			if !ok || len(arr) != 2 {
				continue
			}
			origin, ok := arr[0].(string)
			if !ok {
				continue
			}
			if _, exists := payload[origin]; !exists {
				delete(inputs, name)
			}
		}
	}
}

// asSlice 把 JSON 数组统一取成 []any；取不到返回 nil。
func asSlice(value any) []any {
	arr, _ := value.([]any)
	return arr
}

// numericValue 把 JSON 数字取成 float64（json 解码后数字一律是 float64）。
func numericValue(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok
}

// resolveDateTemplate 将 ComfyUI 的 %date:...% 模板替换为实际时间字符串
func resolveDateTemplate(s string) string {
	replacements := map[string]string{
		"%date:yyyy-MM-dd%": time.Now().Format("2006-01-02"),
		"%date:hhmmss%":     time.Now().Format("150405"),
		"%date:yyyyMMdd%":   time.Now().Format("20060102"),
		"%date:yyyy%":       time.Now().Format("2006"),
		"%date:MM%":         time.Now().Format("01"),
		"%date:dd%":         time.Now().Format("02"),
	}
	for k, v := range replacements {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// expandDateTemplate 对字符串值展开 %date:...% 模板，非字符串原样返回。
// 与 uiToAPIFormat 处理 widget 值时用的是同一套替换规则（resolveDateTemplate）。
func expandDateTemplate(value any) any {
	if s, ok := value.(string); ok {
		return resolveDateTemplate(s)
	}
	return value
}

// cancelComfyJob prefers ComfyUI's job-scoped API. Older versions can remove
// pending jobs; interrupt a running job only when that version supports a prompt ID.
func cancelComfyJob(ctx context.Context, baseURL, promptID string) error {
	status, err := postComfyCancellation(ctx, baseURL+"/api/jobs/"+url.PathEscape(promptID)+"/cancel", nil)
	if err == nil {
		return nil
	}
	if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		return err
	}
	if _, err := postComfyCancellation(ctx, baseURL+"/queue", map[string]any{"delete": []string{promptID}}); err != nil {
		return err
	}
	queue := fetchComfyQueue(ctx, baseURL)
	if !queue.available {
		return errors.New("无法确认 ComfyUI 已移除该任务")
	}
	if !queue.running[promptID] {
		if queue.promptIDs[promptID] {
			return errors.New("ComfyUI 队列里仍有该任务")
		}
		return nil
	}
	if !supportsScopedInterrupt(ctx, baseURL) {
		return errors.New("当前 ComfyUI 版本无法确认支持按任务中断，远端任务可能继续运行")
	}
	_, err = postComfyCancellation(ctx, baseURL+"/interrupt", map[string]any{"prompt_id": promptID})
	return err
}

func postComfyCancellation(ctx context.Context, endpoint string, body map[string]any) (int, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, _, err := tracedDo(http.DefaultClient, req, "cancel", 2048)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("ComfyUI 取消请求返回 HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func supportsScopedInterrupt(ctx context.Context, baseURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/system_stats", nil)
	if err != nil {
		return false
	}
	resp, _, err := tracedDo(http.DefaultClient, req, "system_stats", 2048)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var stats struct {
		System struct {
			Version string `json:"comfyui_version"`
		} `json:"system"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&stats) != nil {
		return false
	}
	version, _, _ := strings.Cut(strings.TrimPrefix(stats.System.Version, "v"), "+")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	var numbers [3]uint64
	for index, part := range parts {
		number, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return false
		}
		numbers[index] = number
	}
	major, minor, patch := numbers[0], numbers[1], numbers[2]
	return major > 0 || minor > 3 || minor == 3 && patch >= 60
}

func (a *app) cancelUnrecordedComfyJob(itemID int64, baseURL, promptID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cancelComfyJob(ctx, baseURL, promptID); err != nil {
		a.recordRemoteCancellationError(itemID, err)
	}
}
