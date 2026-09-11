package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type workflowInput struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	WorkflowJSON   json.RawMessage `json:"workflow_json"`
	Mapping        json.RawMessage `json:"mapping"`
	NegativePrompt string          `json:"negative_prompt"`
	ParamsSchema   json.RawMessage `json:"params_schema"`
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
	// 只存文件名，读取时按当前 dataDir 解析（避免绑定启动目录）
	fileName := safeFilename(input.Name) + ".json"
	if err := writeAtomic(filepath.Join(a.dataDir, "workflows", fileName), input.WorkflowJSON); err != nil {
		writeError(w, http.StatusInternalServerError, "save workflow file failed")
		return
	}
	result, err := a.db.ExecContext(r.Context(),
		`INSERT INTO workflows(name, description, workflow_path, mapping_json, negative_prompt, params_schema, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		input.Name, input.Description, fileName, string(mapping), input.NegativePrompt, string(schema), time.Now(), time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "save workflow failed")
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": input.Name, "workflow_path": fileName,
		"mapping": json.RawMessage(mapping), "negative_prompt": input.NegativePrompt,
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
	name := input.Name
	if strings.TrimSpace(name) == "" {
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
		path = safeFilename(name) + ".json"
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
	description := input.Description
	if description == "" {
		description = oldDescription
	}
	negative := input.NegativePrompt
	schema := string(input.ParamsSchema)
	if len(input.ParamsSchema) == 0 {
		schema = oldSchema
	} else if !json.Valid(input.ParamsSchema) {
		writeError(w, http.StatusBadRequest, "params_schema must be valid JSON")
		return
	}
	_, err = a.db.ExecContext(r.Context(),
		`UPDATE workflows SET name=?, description=?, workflow_path=?, mapping_json=?, negative_prompt=?, params_schema=?, updated_at=? WHERE id=?`,
		name, description, path, mapping, negative, schema, time.Now(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update workflow failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "workflow_path": path, "mapping": json.RawMessage(mapping)})
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
