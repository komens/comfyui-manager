package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// workflowInput 是创建/更新工作流的请求体。
//
// Description / NegativePrompt 用指针是为了区分「没传这个字段」和「传了空串」：
// nil = 保持原值，"" = 清空。用普通 string 时二者无法区分——曾经同一个函数里
// description 是「空=不改」、negative_prompt 是「空=清空」，结果两个字段各缺一半语义。
//
// ParamsSchema 只剩兼容用途：新版本不再解析工作流的可编辑参数，收到就当没看见。
// 留着是为了让浏览器里缓存了旧前端的情况不至于直接报「未知字段」。
type workflowInput struct {
	Name           string          `json:"name"`
	Description    *string         `json:"description"`
	WorkflowJSON   json.RawMessage `json:"workflow_json"`
	Mapping        json.RawMessage `json:"mapping"`
	NegativePrompt *string         `json:"negative_prompt"`
	ParamsSchema   json.RawMessage `json:"params_schema"`
}

// strOrFallback 取指针指向的值；nil（未提供）时回退到 fallback。
func strOrFallback(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

// maxWorkflowJSONLayers 限制剥离层数。正常只有「对象」和「被当字符串传的 JSON」
// 两种形态，多留两层是兜畸形输入，同时防止「字符串里还是字符串」无限剥下去。
const maxWorkflowJSONLayers = 3

// normalizeWorkflowJSON 把 workflow_json 统一成「对象」的原始 JSON。
//
// 客户端有两种送法，都得认：直接给对象（本系统前端就是 JSON.parse 后发的），
// 或者把导出文件的原文当字符串塞进来（curl、脚本、别家导出工具都这么干）。
// 后者若原样落盘，文件内容会多出一层引号（"{\"nodes\":...}"），此后每次提交都报
//
//	parse workflow failed: json: cannot unmarshal string into Go value of type map[string]interface {}
//
// 而根源只是那层多余的引号——「我导出的工作流给你跑不了」里最常见的一种。
//
// 剥不动就原样返回：合法性由调用方按自己的语境判断，这里只负责形态。
func normalizeWorkflowJSON(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	for depth := 0; depth < maxWorkflowJSONLayers && len(trimmed) > 0 && trimmed[0] == '"'; depth++ {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err != nil {
			return trimmed
		}
		next := bytes.TrimSpace([]byte(inner))
		if len(next) == 0 || !json.Valid(next) {
			return trimmed
		}
		trimmed = next
	}
	return trimmed
}

// decodeWorkflowBody 从请求体里取工作流 JSON 并归一化形态。
//
// 三个入口（创建 / 更新 / 校验）都从这里走，保证「接口收到的」和「落盘 / 校验的」
// 是同一份形态。返回 false 表示已经写过响应（调用方直接 return）。
func decodeWorkflowBody(w http.ResponseWriter, raw json.RawMessage, requiredField string) (json.RawMessage, bool) {
	normalized := normalizeWorkflowJSON(raw)
	if !json.Valid(normalized) {
		writeError(w, http.StatusBadRequest, requiredField+" must be valid JSON")
		return nil, false
	}
	var probe any
	if err := json.Unmarshal(normalized, &probe); err != nil {
		writeError(w, http.StatusBadRequest, requiredField+" must be valid JSON")
		return nil, false
	}
	if _, isObject := probe.(map[string]any); !isObject {
		// 字符串/数组/数字都不是工作流。明确报出来，别让它落盘成一个
		// 每次提交都失败、却看不出原因的文件。
		writeError(w, http.StatusBadRequest, requiredField+" must be a JSON object")
		return nil, false
	}
	return normalized, true
}

// readWorkflowFile 读工作流文件并归一化形态。
// 老库里可能存着多一层引号的畸形文件，读的时候顺手纠正——不必去改用户的文件，
// 也能让这些存量数据继续跑。
func readWorkflowFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return normalizeWorkflowJSON(raw), nil
}

func (a *app) listWorkflows(w http.ResponseWriter, r *http.Request) {
	// 统计总数
	var total int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM workflows`).Scan(&total)
	// 分页
	page, pageSize, offset := parsePagination(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, name, description, workflow_path, mapping_json, COALESCE(negative_prompt,''), enabled, is_default, created_at, updated_at,
		(SELECT COUNT(*) FROM generation_tasks t WHERE t.workflow_id=workflows.id),
		(SELECT COUNT(*) FROM generation_tasks t WHERE t.workflow_id=workflows.id AND t.status IN ('pending','running'))
		FROM workflows ORDER BY id DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query workflows failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, enabled, isDefault int
		var name, description, path, mapping, negative, created, updated string
		var taskCount, inFlight int
		if err := rows.Scan(&id, &name, &description, &path, &mapping, &negative, &enabled, &isDefault, &created, &updated, &taskCount, &inFlight); err != nil {
			writeError(w, http.StatusInternalServerError, "read workflow failed")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "description": description,
			"workflow_path": path, "mapping": json.RawMessage(mapping),
			"negative_prompt": negative,
			"enabled":         enabled == 1, "is_default": isDefault == 1, "created_at": created, "updated_at": updated,
			"task_count": taskCount, "in_flight": inFlight})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (a *app) createWorkflow(w http.ResponseWriter, r *http.Request) {
	var input workflowInput
	if err := decodeJSON(r, &input); err != nil || strings.TrimSpace(input.Name) == "" || len(input.WorkflowJSON) == 0 {
		writeError(w, http.StatusBadRequest, "name and workflow_json are required")
		return
	}
	name := strings.TrimSpace(input.Name)
	workflowJSON, ok := decodeWorkflowBody(w, input.WorkflowJSON, "workflow_json")
	if !ok {
		return
	}
	mapping := input.Mapping
	if len(mapping) > 0 && !json.Valid(mapping) {
		writeError(w, http.StatusBadRequest, "mapping must be valid JSON")
		return
	}
	// 编辑器没选的那一侧由自动识别补上：导入即用，不需要先点「识别」再保存——
	// 「配一个新工作流就报错」最常见的来源就是这一步。
	mapping = mergePromptMapping(mapping, workflowJSON)
	if len(mapping) == 0 {
		mapping = json.RawMessage(`{}`)
	}
	description := strOrFallback(input.Description, "")
	negative := strOrFallback(input.NegativePrompt, "")

	// 先入库拿到 id，再用 id 拼文件名：只用 name 的话，两个同名工作流会指向同一个文件，
	// 改其中一个会静默改掉另一个（旧实现的隐患）。
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save workflow failed")
		return
	}
	result, err := tx.ExecContext(r.Context(),
		`INSERT INTO workflows(name, description, workflow_path, mapping_json, negative_prompt, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		name, description, "", string(mapping), negative, time.Now(), time.Now())
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "save workflow failed")
		return
	}
	id, _ := result.LastInsertId()
	// 只存文件名，读取时按当前 dataDir 解析（避免绑定启动目录）
	fileName := workflowFileName(name, id)
	if err := writeAtomic(filepath.Join(a.dataDir, "workflows", fileName), workflowJSON); err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "save workflow file failed")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE workflows SET workflow_path=? WHERE id=?`, fileName, id); err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "save workflow failed")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "save workflow failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": name, "workflow_path": fileName,
		"mapping": json.RawMessage(mapping), "negative_prompt": negative})
}

func (a *app) getWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow id")
		return
	}
	var name, description, path, mapping, negative, created, updated string
	var enabled, isDefault int
	err = a.db.QueryRowContext(r.Context(),
		`SELECT name, description, workflow_path, mapping_json, COALESCE(negative_prompt,''), enabled, is_default, created_at, updated_at FROM workflows WHERE id=?`, id).
		Scan(&name, &description, &path, &mapping, &negative, &enabled, &isDefault, &created, &updated)
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	workflowJSON, err := readWorkflowFile(a.workflowFilePath(path))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read workflow file failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": name, "description": description, "workflow_path": filepath.Base(path),
		"workflow_json": json.RawMessage(workflowJSON), "mapping": json.RawMessage(mapping),
		"negative_prompt": negative,
		"enabled":         enabled == 1, "is_default": isDefault == 1, "created_at": created, "updated_at": updated,
	})
}

// setDefaultWorkflow 设置/取消默认工作流。默认工作流全局唯一：
// 设为默认时会先把其它工作流的 is_default 清零，保证任意时刻至多一个默认。
func (a *app) setDefaultWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow id")
		return
	}
	var input struct {
		IsDefault bool `json:"is_default"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "set default failed")
		return
	}
	var exists int
	if err := tx.QueryRowContext(r.Context(), `SELECT 1 FROM workflows WHERE id=?`, id).Scan(&exists); err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE workflows SET is_default=0 WHERE is_default=1`); err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "set default failed")
		return
	}
	if input.IsDefault {
		if _, err := tx.ExecContext(r.Context(), `UPDATE workflows SET is_default=1, updated_at=? WHERE id=?`, time.Now(), id); err != nil {
			_ = tx.Rollback()
			writeError(w, http.StatusInternalServerError, "set default failed")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "set default failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "is_default": input.IsDefault})
}

func (a *app) updateWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow id")
		return
	}
	var input workflowInput
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var oldPath, oldMapping, oldName, oldDescription, oldNegative string
	err = a.db.QueryRowContext(r.Context(),
		`SELECT name, description, workflow_path, mapping_json, COALESCE(negative_prompt,'') FROM workflows WHERE id=?`, id).
		Scan(&oldName, &oldDescription, &oldPath, &oldMapping, &oldNegative)
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = oldName
	}
	// 归一化为文件名（兼容旧数据里的 "./data/workflows/x.json"）
	path := filepath.Base(oldPath)
	if len(input.WorkflowJSON) > 0 {
		workflowJSON, ok := decodeWorkflowBody(w, input.WorkflowJSON, "workflow_json")
		if !ok {
			return
		}
		input.WorkflowJSON = workflowJSON // 后面的自动识别与落盘都用归一化后的形态
		path = workflowFileName(name, id)
		if err := writeAtomic(filepath.Join(a.dataDir, "workflows", path), workflowJSON); err != nil {
			writeError(w, http.StatusInternalServerError, "save workflow file failed")
			return
		}
	}
	if len(input.Mapping) > 0 && !json.Valid(input.Mapping) {
		writeError(w, http.StatusBadRequest, "mapping must be valid JSON")
		return
	}
	// 拿哪份工作流做自动识别：优先请求里带的新内容，其次是已经存着的那份。
	// 换了工作流却不重新识别的话，旧映射会冲着旧节点 id 写，提示词就打偏了。
	detectSource := input.WorkflowJSON
	if len(detectSource) == 0 {
		stored, err := os.ReadFile(a.workflowFilePath(path))
		if err == nil {
			detectSource = stored
		}
	}
	provided := input.Mapping
	if len(provided) == 0 {
		provided = json.RawMessage(oldMapping)
	}
	mapping := "{}"
	if merged := mergePromptMapping(provided, detectSource); len(merged) > 0 {
		mapping = string(merged)
	}
	// nil = 请求里没带这个字段（保持原值）；非 nil = 以请求为准，空串即清空
	description := strOrFallback(input.Description, oldDescription)
	negative := strOrFallback(input.NegativePrompt, oldNegative)
	_, err = a.db.ExecContext(r.Context(),
		`UPDATE workflows SET name=?, description=?, workflow_path=?, mapping_json=?, negative_prompt=?, updated_at=? WHERE id=?`,
		name, description, path, mapping, negative, time.Now(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update workflow failed")
		return
	}
	// 换了文件名就顺手清掉旧文件（只在没有任何其它记录指向它时才删）
	a.removeOrphanWorkflowFile(r.Context(), id, oldPath, path)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "workflow_path": path, "mapping": json.RawMessage(mapping)})
}

// buildPromptMapping 把解析结果转成要落库的映射。
//
// 负向与正向同源时**不写进映射**：那种工作流没有独立的负向文本框，存进去只会让
// 编辑器显示一个「选了也不生效」的节点。留空即表示「提交时自动解析」，结果一样。
func buildPromptMapping(targets promptTargets) promptMapping {
	mapping := promptMapping{}
	if targets.Positive != nil {
		mapping.Positive = *targets.Positive
	}
	if targets.Negative != nil && !targets.NegativeShared {
		mapping.Negative = *targets.Negative
	}
	return mapping
}

// autoDetectPromptMapping 识别一份工作流该把正负向提示词写到哪里。
//
// 走离线路径（UI 导出格式图遍历，见 promptnodes.go）：导入工作流不该依赖 ComfyUI
// 在线。更老的导出格式（连 widgets_values_named 都没有）只能解析出节点 id，
// 字段名留到提交时再定——那时 API 格式已经还原好了。
//
// 解析不出任何落点时返回 nil，调用方会把映射存成 {}，提交时再自动解析一遍。
func autoDetectPromptMapping(workflowRaw json.RawMessage) json.RawMessage {
	var workflow map[string]any
	if err := json.Unmarshal(workflowRaw, &workflow); err != nil {
		return nil
	}
	targets := resolvePromptTargetsFromUI(workflow)
	if _, isUIFormat := workflow["nodes"].([]any); !isUIFormat {
		targets = resolvePromptTargets(workflow)
	}
	if targets.Positive == nil && targets.Negative == nil {
		return nil
	}
	encoded, err := json.Marshal(buildPromptMapping(targets))
	if err != nil {
		return nil
	}
	return json.RawMessage(encoded)
}

// mergePromptMapping 把「编辑器里选好的落点」与「按结构自动识别的落点」合并。
//
// 每一侧以显式选择为准，没选的那一侧用自动识别的补上。这样编辑器里「留空」
// 就永远等于「自动」——不会出现「把选择清空、旧节点却还留着」的错位。
//
// 每侧都记 node_id + field：同一个节点上可能同时有 prompt 和 negative_prompt
// 两个框（TextEncodeQwenImage21），只记节点会让负向把正向盖掉。
func mergePromptMapping(provided json.RawMessage, workflowJSON json.RawMessage) json.RawMessage {
	var chosen promptMapping
	if len(provided) > 0 {
		if err := json.Unmarshal(provided, &chosen); err != nil {
			return provided
		}
	}
	if detected := autoDetectPromptMapping(workflowJSON); len(detected) > 0 {
		var auto promptMapping
		if err := json.Unmarshal(detected, &auto); err == nil {
			if chosen.Positive.NodeID == "" {
				chosen.Positive = auto.Positive
			}
			if chosen.Negative.NodeID == "" {
				chosen.Negative = auto.Negative
			}
		}
	}
	merged, err := json.Marshal(chosen)
	if err != nil {
		return nil
	}
	return json.RawMessage(merged)
}

// workflowFileName 生成工作流文件的存储名。
// 带上 id 是为了让每个工作流独占一个文件：旧实现只用 safeFilename(name)，
// 两个同名工作流会指向同一个文件，改其中一个会静默改掉另一个。
func workflowFileName(name string, id int64) string {
	return fmt.Sprintf("%s-%d.json", safeFilename(name), id)
}

// removeOrphanWorkflowFile 在 workflow 换了文件名后清理旧文件。
// 仅当该 basename 没有任何**其它** workflow 记录指向时才删——
// 历史数据里同名工作流可能共用同一文件，盲目删除会把别人的工作流一起删掉。
func (a *app) removeOrphanWorkflowFile(ctx context.Context, id int64, oldPath, newPath string) {
	oldBase := filepath.Base(strings.TrimSpace(oldPath))
	if oldBase == "" || oldBase == "." || oldBase == filepath.Base(newPath) {
		return
	}
	rows, err := a.db.QueryContext(ctx, `SELECT workflow_path FROM workflows WHERE id<>?`, id)
	if err != nil {
		return
	}
	referenced := false
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err == nil && filepath.Base(strings.TrimSpace(path)) == oldBase {
			referenced = true
			break
		}
	}
	rows.Close()
	if referenced {
		return
	}
	_ = os.Remove(filepath.Join(a.dataDir, "workflows", oldBase))
}

func (a *app) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow id")
		return
	}
	// 只拦「在途」任务：submitter 的查询 JOIN workflows，删掉被 pending/running
	// 任务引用的工作流会让那些任务永远卡在队列里。历史完成任务不受影响——
	// 任务表已冗余 workflow_name，展示不依赖 workflows 表；重试会在
	// retryTaskByID 里给出明确报错。检查与删除放同一事务，避免
	// 「检查通过后、删除前恰好有新任务引用该工作流」的竞态。
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete workflow failed")
		return
	}
	var inFlight int
	if err := tx.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM generation_tasks WHERE workflow_id=? AND status IN ('pending','running')`, id).Scan(&inFlight); err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "delete workflow failed")
		return
	}
	if inFlight > 0 {
		_ = tx.Rollback()
		writeError(w, http.StatusConflict, fmt.Sprintf("该工作流还有 %d 个排队中/生成中的任务，请等待完成或先取消这些任务", inFlight))
		return
	}
	var path string
	if err := tx.QueryRowContext(r.Context(), `SELECT workflow_path FROM workflows WHERE id=?`, id).Scan(&path); err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	result, err := tx.ExecContext(r.Context(), `DELETE FROM workflows WHERE id=?`, id)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "delete workflow failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		_ = tx.Rollback()
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "delete workflow failed")
		return
	}
	// 文件删除放在事务提交后：库里已无引用，即使删文件失败也只是留下
	// 可人工清理的孤儿 JSON，不会反过来让界面出现删不掉的记录。
	// newPath 传空串表示「这个记录要消失了」——复用同一个引用检查，
	// 历史数据里两个工作流可能指向同一文件，不能盲目删。
	a.removeOrphanWorkflowFile(r.Context(), id, path, "")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// promptTargetsResponse 是「提示词落点」接口的返回体。
type promptTargetsResponse struct {
	// Detected 是按结构自动识别的结果（离线可算，不需要 ComfyUI 在线）。
	Detected promptTargets `json:"detected"`
	// Stored 是这份工作流已保存的映射——编辑器里手动选过的以它为准。
	Stored promptMapping `json:"stored"`
	// Candidates 是可供选择的提示词节点，供编辑器做成下拉框。
	Candidates []promptCandidate `json:"candidates"`
	// NegativeUnavailable 说明负向提示词为什么写不进去；空串表示没有这个问题。
	NegativeUnavailable string `json:"negative_unavailable"`
}

// buildPromptTargetsResponse 组装落点信息。
func buildPromptTargetsResponse(workflow map[string]any) promptTargetsResponse {
	_, isUIFormat := workflow["nodes"].([]any)
	detected := resolvePromptTargetsFromUI(workflow)
	if !isUIFormat {
		detected = resolvePromptTargets(workflow)
	}
	response := promptTargetsResponse{
		Detected:   detected,
		Candidates: collectPromptCandidates(workflow),
	}
	switch {
	case detected.NegativeShared:
		response.NegativeUnavailable = "这份工作流的负向由正向派生（ConditioningZeroOut 之类），没有独立的负向提示词节点，负向提示词不会生效。"
	case detected.Negative == nil:
		response.NegativeUnavailable = "未能自动识别负向提示词节点，请手动选择；选好后提交时会写进去。"
	}
	return response
}

// detectWorkflowPromptTargets 扫描请求体里的 workflow_json，返回落点与候选节点。
//
// 走的是不带 id 的通用入口，所以「还没保存的新工作流」也能先看落点再保存。
func (a *app) detectWorkflowPromptTargets(w http.ResponseWriter, r *http.Request) {
	var input struct {
		WorkflowJSON json.RawMessage `json:"workflow_json"`
	}
	if err := decodeJSON(r, &input); err != nil || len(input.WorkflowJSON) == 0 {
		writeError(w, http.StatusBadRequest, "workflow_json is required")
		return
	}
	normalized := normalizeWorkflowJSON(input.WorkflowJSON)
	var workflow map[string]any
	if err := json.Unmarshal(normalized, &workflow); err != nil {
		writeError(w, http.StatusBadRequest, "workflow_json must be valid JSON")
		return
	}
	writeJSON(w, http.StatusOK, buildPromptTargetsResponse(workflow))
}

// getWorkflowPromptTargets 读取已保存的工作流，返回落点、候选节点与已保存的映射。
func (a *app) getWorkflowPromptTargets(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow id")
		return
	}
	var path, mapping string
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT workflow_path, mapping_json FROM workflows WHERE id=?`, id).Scan(&path, &mapping); err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	workflowBytes, err := readWorkflowFile(a.workflowFilePath(path))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read workflow file failed")
		return
	}
	var workflow map[string]any
	if err := json.Unmarshal(workflowBytes, &workflow); err != nil {
		writeError(w, http.StatusInternalServerError, "parse workflow failed")
		return
	}
	response := buildPromptTargetsResponse(workflow)
	// 已保存的映射优先：用户可能手工选过，自动识别不能把它盖掉
	if trimmed := strings.TrimSpace(mapping); trimmed != "" {
		var stored promptMapping
		if err := json.Unmarshal([]byte(trimmed), &stored); err == nil {
			response.Stored = stored
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// safeFilename 把工作流名转成文件名片段：在 macOS / Linux / Windows 上都安全，
// 同时尽量保留可读性。
//
// 旧实现把所有非 ASCII 字母数字一律换成 "_"，中文名会整段塌成下划线
// （「风景写实」→ "____"），data/workflows/ 里一眼分不清哪个文件对应哪个工作流。
// 现在保留 Unicode 字母/数字（中文、日文、带音标的拉丁字母都原样留下），只替换
// 文件系统真正不安全的字符：路径分隔符、Windows 保留字符（: * ? " < > |）、
// 控制字符和各类空白。连续的不安全字符合并成一个 "-"，过长时按字节安全截断。
//
// 读取侧不受影响：workflowFilePath 只取 basename 再拼当前 dataDir，
// 旧的下划线文件名照样读得到，只有下次保存/改名时才会换成新名字。
func safeFilename(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	pendingSeparator := false
	for _, r := range value {
		if isSafeFilenameRune(r) {
			// 不安全字符先攒着，等下一个安全字符出现时补一个分隔符，
			// 免得 "a  b" 变成 "a--b" 这种连续分隔。
			if pendingSeparator && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingSeparator = false
			b.WriteRune(r)
			continue
		}
		pendingSeparator = true
	}
	// 单个文件名在各主流文件系统上通常限制 255 字节，截到 200 给 "-<id>.json" 留足余量。
	name := strings.Trim(truncateBytes(b.String(), 200), "-")
	if name == "" {
		return "workflow"
	}
	return name
}

// isSafeFilenameRune 判断字符能否原样进文件名：只放行 Unicode 字母与数字，
// 外加 - _ . 三个无歧义的连接符。
func isSafeFilenameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.'
}

// truncateBytes 按字节上限截断，且不会把多字节字符切碎。
func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// workflowFilePath 把 workflows.workflow_path 解析成当前 dataDir 下的真实文件路径。
// 新数据只存文件名；旧数据可能是 "./data/workflows/x.json" 这种绑定写入时工作目录的相对路径，
// 统一取 basename 后重新拼到 dataDir/workflows 下，避免换启动目录 / 换 DATA_DIR / Docker 挂载点后失效。
func (a *app) workflowFilePath(stored string) string {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return ""
	}
	base := filepath.Base(stored)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(a.dataDir, "workflows", base)
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
