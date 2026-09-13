package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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

func (a *app) listWorkflows(w http.ResponseWriter, r *http.Request) {
	// 统计总数
	var total int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM workflows`).Scan(&total)
	// 分页
	page, pageSize, offset := parsePagination(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, name, description, workflow_path, mapping_json, COALESCE(negative_prompt,''), params_schema, enabled, is_default, created_at, updated_at FROM workflows ORDER BY id DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query workflows failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, enabled, isDefault int
		var name, description, path, mapping, negative, schema, created, updated string
		if err := rows.Scan(&id, &name, &description, &path, &mapping, &negative, &schema, &enabled, &isDefault, &created, &updated); err != nil {
			writeError(w, http.StatusInternalServerError, "read workflow failed")
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "description": description,
			"workflow_path": path, "mapping": json.RawMessage(mapping),
			"negative_prompt": negative, "params_schema": json.RawMessage(schema),
			"enabled": enabled == 1, "is_default": isDefault == 1, "created_at": created, "updated_at": updated})
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
	schema := input.ParamsSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{}`)
	}
	if !json.Valid(schema) {
		writeError(w, http.StatusBadRequest, "params_schema must be valid JSON")
		return
	}
	mapping = syncMappingDefaults(mapping, schema)
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
		`INSERT INTO workflows(name, description, workflow_path, mapping_json, negative_prompt, params_schema, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		name, description, "", string(mapping), negative, string(schema), time.Now(), time.Now())
	if err != nil {
		_ = tx.Rollback()
		writeError(w, http.StatusInternalServerError, "save workflow failed")
		return
	}
	id, _ := result.LastInsertId()
	// 只存文件名，读取时按当前 dataDir 解析（避免绑定启动目录）
	fileName := workflowFileName(name, id)
	if err := writeAtomic(filepath.Join(a.dataDir, "workflows", fileName), input.WorkflowJSON); err != nil {
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
		"mapping": json.RawMessage(mapping), "negative_prompt": negative,
		"params_schema": json.RawMessage(schema)})
}

func (a *app) getWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workflow id")
		return
	}
	var name, description, path, mapping, negative, schema, created, updated string
	var enabled, isDefault int
	err = a.db.QueryRowContext(r.Context(),
		`SELECT name, description, workflow_path, mapping_json, COALESCE(negative_prompt,''), params_schema, enabled, is_default, created_at, updated_at FROM workflows WHERE id=?`, id).
		Scan(&name, &description, &path, &mapping, &negative, &schema, &enabled, &isDefault, &created, &updated)
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	workflowJSON, err := os.ReadFile(a.workflowFilePath(path))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read workflow file failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": name, "description": description, "workflow_path": filepath.Base(path),
		"workflow_json": json.RawMessage(workflowJSON), "mapping": json.RawMessage(mapping),
		"negative_prompt": negative, "params_schema": json.RawMessage(schema),
		"enabled": enabled == 1, "is_default": isDefault == 1, "created_at": created, "updated_at": updated,
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
	var oldPath, oldMapping, oldName, oldDescription, oldNegative, oldSchema string
	err = a.db.QueryRowContext(r.Context(),
		`SELECT name, description, workflow_path, mapping_json, COALESCE(negative_prompt,''), params_schema FROM workflows WHERE id=?`, id).
		Scan(&oldName, &oldDescription, &oldPath, &oldMapping, &oldNegative, &oldSchema)
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
	mapping := oldMapping
	if len(input.WorkflowJSON) > 0 {
		var workflow any
		if err := json.Unmarshal(input.WorkflowJSON, &workflow); err != nil {
			writeError(w, http.StatusBadRequest, "workflow_json must be valid JSON")
			return
		}
		path = workflowFileName(name, id)
		if err := writeAtomic(filepath.Join(a.dataDir, "workflows", path), input.WorkflowJSON); err != nil {
			writeError(w, http.StatusInternalServerError, "save workflow file failed")
			return
		}
	}
	if len(input.Mapping) > 0 {
		if !json.Valid(input.Mapping) {
			writeError(w, http.StatusBadRequest, "mapping must be valid JSON")
			return
		}
		mapping = string(input.Mapping)
	}
	// nil = 请求里没带这个字段（保持原值）；非 nil = 以请求为准，空串即清空
	description := strOrFallback(input.Description, oldDescription)
	negative := strOrFallback(input.NegativePrompt, oldNegative)
	schema := string(input.ParamsSchema)
	if len(input.ParamsSchema) == 0 {
		schema = oldSchema
	} else if !json.Valid(input.ParamsSchema) {
		writeError(w, http.StatusBadRequest, "params_schema must be valid JSON")
		return
	}
	mapping = string(syncMappingDefaults(json.RawMessage(mapping), json.RawMessage(schema)))
	_, err = a.db.ExecContext(r.Context(),
		`UPDATE workflows SET name=?, description=?, workflow_path=?, mapping_json=?, negative_prompt=?, params_schema=?, updated_at=? WHERE id=?`,
		name, description, path, mapping, negative, schema, time.Now(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update workflow failed")
		return
	}
	// 换了文件名就顺手清掉旧文件（只在没有任何其它记录指向它时才删）
	a.removeOrphanWorkflowFile(r.Context(), id, oldPath, path)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "workflow_path": path, "mapping": json.RawMessage(mapping)})
}

// syncMappingDefaults 把 params_schema 的默认值同步进 mapping.parameters，消除第二份副本。
//
// 「参数默认值」在库里存了两处：mapping_json.parameters[].default 与 params_schema[].default。
// 前端表单只读 params_schema；但后端 injectDirectPrompt 在参数缺键时会退回 mapping 里的
// default（典型是 visible:false 的隐藏参数——ParamForm 不会渲染、也就不会提交）。两处一旦
// 漂移，就会出现「改了工作流参数，重跑却还是旧值」，且现场只表现为参数没生效，极难定位。
// 这里统一以 params_schema 为准：同一份默认值只有一个真源。
//
// 只覆盖两边**同名**的参数；mapping 里手工加过、schema 里没有的条目原样保留。
func syncMappingDefaults(mapping, schema json.RawMessage) json.RawMessage {
	if len(mapping) == 0 || len(schema) == 0 {
		return mapping
	}
	var mappingDoc map[string]any
	if err := json.Unmarshal(mapping, &mappingDoc); err != nil {
		return mapping
	}
	params, _ := mappingDoc["parameters"].(map[string]any)
	if len(params) == 0 {
		return mapping
	}
	var list []struct {
		Name    string `json:"name"`
		Default any    `json:"default"`
	}
	if err := json.Unmarshal(schema, &list); err != nil {
		return mapping
	}
	changed := false
	for _, s := range list {
		if s.Name == "" || s.Default == nil {
			continue
		}
		entry, ok := params[s.Name].(map[string]any)
		if !ok {
			continue
		}
		// 用 DeepEqual 而不是 ==：JSON 里的数组/对象反序列化成 slice/map，
		// 这类不可比较类型直接用 == 会 panic。
		if reflect.DeepEqual(entry["default"], s.Default) {
			continue
		}
		entry["default"] = s.Default
		changed = true
	}
	if !changed {
		return mapping
	}
	out, err := json.Marshal(mappingDoc)
	if err != nil {
		return mapping
	}
	return out
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
	var taskCount int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM generation_tasks WHERE workflow_id=?`, id).Scan(&taskCount)
	if taskCount > 0 {
		writeError(w, http.StatusConflict, "workflow is referenced by existing tasks and cannot be deleted")
		return
	}
	var path string
	if err := a.db.QueryRowContext(r.Context(), `SELECT workflow_path FROM workflows WHERE id=?`, id).Scan(&path); err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	// 先删文件、再删库（和删图片同一顺序）：反过来的话，删文件失败就会在
	// data/workflows/ 里留下界面上再也清不掉的孤儿 JSON。
	// newPath 传空串表示「这个记录要消失了」——复用同一个引用检查，
	// 历史数据里两个工作流可能指向同一文件，不能盲目删。
	a.removeOrphanWorkflowFile(r.Context(), id, path, "")
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM workflows WHERE id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete workflow failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// detectWorkflowParams 解析 workflow_json，自动识别可编辑参数并生成 mapping
func (a *app) detectWorkflowParams(w http.ResponseWriter, r *http.Request) {
	var input struct {
		WorkflowJSON json.RawMessage `json:"workflow_json"`
	}
	if err := decodeJSON(r, &input); err != nil || len(input.WorkflowJSON) == 0 {
		writeError(w, http.StatusBadRequest, "workflow_json is required")
		return
	}
	var workflow map[string]any
	if err := json.Unmarshal(input.WorkflowJSON, &workflow); err != nil {
		writeError(w, http.StatusBadRequest, "workflow_json must be valid JSON")
		return
	}
	params := make([]map[string]any, 0)
	positiveNode, negativeNode := "", ""

	// 归一化节点：同时支持 API 格式（nodeID 为键）和 UI 导出格式（nodes 数组）
	type nodeInfo struct {
		id        string
		classType string
		inputs    map[string]any
	}
	var nodes []nodeInfo
	if rawNodes, ok := workflow["nodes"].([]any); ok {
		// UI 导出格式
		for _, raw := range rawNodes {
			n, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id := fmt.Sprintf("%v", n["id"])
			classType, _ := n["type"].(string)
			inputs := parseUIWidgets(classType, n)
			nodes = append(nodes, nodeInfo{id: id, classType: classType, inputs: inputs})
		}
	} else {
		// API 格式
		for nodeID, nodeRaw := range workflow {
			node, ok := nodeRaw.(map[string]any)
			if !ok {
				continue
			}
			classType, _ := node["class_type"].(string)
			inputs, _ := node["inputs"].(map[string]any)
			if inputs == nil {
				inputs = map[string]any{}
			}
			nodes = append(nodes, nodeInfo{id: nodeID, classType: classType, inputs: inputs})
		}
	}

	for _, n := range nodes {
		classType := n.classType
		inputs := n.inputs
		nodeID := n.id
		switch {
		case classType == "KSampler" || classType == "KSamplerAdvanced":
			if v, ok := inputs["steps"]; ok {
				params = append(params, map[string]any{"name": "steps", "label": "采样步数", "type": "integer", "node_id": nodeID, "field": "inputs.steps", "default": v})
			}
			if v, ok := inputs["cfg"]; ok {
				params = append(params, map[string]any{"name": "cfg", "label": "CFG 系数", "type": "number", "node_id": nodeID, "field": "inputs.cfg", "default": v})
			}
			if _, ok := inputs["seed"]; ok {
				params = append(params, map[string]any{"name": "seed", "label": "随机种子", "type": "seed", "node_id": nodeID, "field": "inputs.seed", "default": 0})
			}
		case classType == "EmptyLatentImage" || classType == "EmptySD3LatentImage":
			if v, ok := inputs["width"]; ok {
				params = append(params, map[string]any{"name": "width", "label": "宽度", "type": "integer", "node_id": nodeID, "field": "inputs.width", "default": v})
			}
			if v, ok := inputs["height"]; ok {
				params = append(params, map[string]any{"name": "height", "label": "高度", "type": "integer", "node_id": nodeID, "field": "inputs.height", "default": v})
			}
			if v, ok := inputs["batch_size"]; ok {
				params = append(params, map[string]any{"name": "batch_size", "label": "批量数量", "type": "integer", "node_id": nodeID, "field": "inputs.batch_size", "default": v})
			}
		case classType == "CheckpointLoaderSimple":
			if v, ok := inputs["ckpt_name"]; ok {
				params = append(params, map[string]any{"name": "checkpoint", "label": "模型", "type": "string", "node_id": nodeID, "field": "inputs.ckpt_name", "default": v})
			}
		default:
			// 通用参数检测：跳过已知类型和连接类型输入
			knownInputs := map[string]bool{
				"steps": true, "cfg": true, "seed": true, "width": true, "height": true,
				"batch_size": true, "ckpt_name": true, "text": true, "image": true,
				"model": true, "clip": true, "vae": true, "positive": true, "negative": true,
				"samples": true, "latent": true, "conditioning": true, "control_net": true,
			}
			for k, v := range inputs {
				if knownInputs[k] {
					continue
				}
				// 跳过连接类型（数组 [node_id, slot]）
				if _, ok := v.([]any); ok {
					continue
				}
				paramType := "string"
				switch v.(type) {
				case float64:
					if v == float64(int(v.(float64))) {
						paramType = "integer"
					} else {
						paramType = "number"
					}
				case bool:
					paramType = "boolean"
				}
				if paramType == "string" {
					if s, ok := v.(string); ok && len(s) > 200 {
						continue // 跳过长文本
					}
				}
				label := k
				if len(label) > 30 {
					label = label[:30]
				}
				params = append(params, map[string]any{
					"name": fmt.Sprintf("%s_%s", classType, k), "label": label, "type": paramType,
					"node_id": nodeID, "field": "inputs." + k, "default": v,
				})
			}
		case classType == "CLIPTextEncode":
			text, _ := inputs["text"].(string)
			lower := strings.ToLower(text)
			negKeywords := []string{"nsfw", "bad", "worst", "low quality", "低质量", "模糊", "畸形", "错误", "多余", "丑陋", "崩坏", "变形"}
			isNegative := false
			for _, kw := range negKeywords {
				if strings.Contains(lower, strings.ToLower(kw)) {
					isNegative = true
					break
				}
			}
			if isNegative {
				if negativeNode == "" {
					negativeNode = nodeID
				}
			} else if positiveNode == "" && text != "" {
				positiveNode = nodeID
			}
		}
	}
	// 生成 mapping 建议
	mapping := map[string]any{
		"parameters": map[string]any{},
	}
	if positiveNode != "" {
		mapping["positive_prompt"] = map[string]string{"node_id": positiveNode, "field": "inputs.text"}
	}
	if negativeNode != "" {
		mapping["negative_prompt"] = map[string]string{"node_id": negativeNode, "field": "inputs.text"}
	}
	paramMap := map[string]any{}
	for _, p := range params {
		name, _ := p["name"].(string)
		nodeID, _ := p["node_id"].(string)
		field, _ := p["field"].(string)
		if name == "seed" {
			// seed 单独走 mapping.seed，避免被 parameters 循环用默认值 0 覆盖掉随机种子
			mapping["seed"] = map[string]string{"node_id": nodeID, "field": field}
			continue
		}
		// 带上 type/label/default：前端 ParamForm 靠 def.type 选控件类型，
		// 只给 node_id/field 会让 steps、cfg、width 等全部退化成文本输入框。
		entry := map[string]any{"node_id": nodeID, "field": field}
		for _, key := range []string{"type", "label", "default"} {
			if v, ok := p[key]; ok {
				entry[key] = v
			}
		}
		paramMap[name] = entry
	}
	mapping["parameters"] = paramMap
	mappingJSON, _ := json.Marshal(mapping)
	schemaJSON, _ := json.Marshal(params)
	writeJSON(w, http.StatusOK, map[string]any{
		"params":        params,
		"mapping":       json.RawMessage(mappingJSON),
		"positive_node": positiveNode,
		"negative_node": negativeNode,
		"params_schema": json.RawMessage(schemaJSON),
	})
}

// parseUIWidgets 将 ComfyUI UI 导出格式的 widgets_values 数组转换为 inputs 映射
func parseUIWidgets(classType string, node map[string]any) map[string]any {
	inputs := map[string]any{}
	// 优先使用 widgets_values_named（精确 key-value，跳过 control_after_generate 等控制项）
	if named, ok := node["widgets_values_named"].(map[string]any); ok {
		for k, v := range named {
			inputs[k] = v
		}
		return inputs
	}
	widgets, ok := node["widgets_values"].([]any)
	if !ok {
		return inputs
	}
	get := func(i int) (any, bool) {
		if i < len(widgets) {
			return widgets[i], true
		}
		return nil, false
	}
	switch classType {
	case "KSampler":
		seedIdx, stepsIdx, cfgIdx := 0, 1, 2
		if len(widgets) > 1 {
			if _, isStr := widgets[1].(string); isStr {
				stepsIdx, cfgIdx = 2, 3
			}
		}
		if v, ok := get(seedIdx); ok {
			inputs["seed"] = v
		}
		if v, ok := get(stepsIdx); ok {
			inputs["steps"] = v
		}
		if v, ok := get(cfgIdx); ok {
			inputs["cfg"] = v
		}
	case "KSamplerAdvanced":
		if v, ok := get(1); ok {
			inputs["seed"] = v
		}
		stepsIdx, cfgIdx := 2, 3
		if len(widgets) > 3 {
			if _, isStr := widgets[2].(string); isStr {
				stepsIdx, cfgIdx = 3, 4
			}
		}
		if v, ok := get(stepsIdx); ok {
			inputs["steps"] = v
		}
		if v, ok := get(cfgIdx); ok {
			inputs["cfg"] = v
		}
	case "EmptyLatentImage", "EmptySD3LatentImage":
		if v, ok := get(0); ok {
			inputs["width"] = v
		}
		if v, ok := get(1); ok {
			inputs["height"] = v
		}
		if v, ok := get(2); ok {
			inputs["batch_size"] = v
		}
	case "CheckpointLoaderSimple":
		if v, ok := get(0); ok {
			inputs["ckpt_name"] = v
		}
	case "CLIPTextEncode":
		if v, ok := get(0); ok {
			inputs["text"] = v
		}
	}
	return inputs
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
