package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type comfySubmitResponse struct {
	PromptID string `json:"prompt_id"`
}

func submitComfy(ctx context.Context, baseURL string, workflow map[string]any, clientID string) (string, error) {
	body, _ := json.Marshal(map[string]any{"prompt": workflow, "client_id": clientID})
	dumpSubmitPayload(baseURL+"/prompt", clientID, body) // 临时调试：落盘真正提交的载荷
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/prompt", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("ComfyUI /prompt HTTP %d", response.StatusCode)
	}
	var result comfySubmitResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || result.PromptID == "" {
		return "", fmt.Errorf("ComfyUI response has no prompt_id")
	}
	return result.PromptID, nil
}

// submitter 提交阶段（每3秒扫描 pending items 提交到 ComfyUI）
func (a *app) submitter() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		rows, err := a.db.Query(`SELECT i.id, i.task_id, i.positive_prompt, t.comfyui_url, t.parameters_json, w.workflow_path, w.mapping_json, COALESCE(w.negative_prompt,'')
			FROM generation_items i
			JOIN generation_tasks t ON t.id=i.task_id
			JOIN workflows w ON w.id=t.workflow_id
			WHERE i.status='pending' AND t.status='pending'
			ORDER BY i.id LIMIT 10`)
		if err != nil {
			a.log.Printf("submitter query error: %v", err)
			continue
		}
		type pendingItem struct {
			ItemID         int64
			TaskID         int64
			Positive       string
			BaseURL        string
			Parameters     string
			WorkflowPath   string
			MappingJSON    string
			NegativePrompt string
		}
		var items []pendingItem
		for rows.Next() {
			var p pendingItem
			if err := rows.Scan(&p.ItemID, &p.TaskID, &p.Positive, &p.BaseURL, &p.Parameters, &p.WorkflowPath, &p.MappingJSON, &p.NegativePrompt); err == nil {
				items = append(items, p)
			}
		}
		rows.Close()
		for _, p := range items {
			a.submitItem(p.ItemID, p.TaskID, p.Positive, p.BaseURL, p.Parameters, p.WorkflowPath, p.MappingJSON, p.NegativePrompt)
		}
	}
}

// submitItem 提交单个 item 到 ComfyUI
func (a *app) submitItem(itemID, taskID int64, positive, baseURL, parameters, workflowPath, mappingJSON, negativePrompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 标记 task 为 running
	_, _ = a.db.Exec(`UPDATE generation_tasks SET status='running', started_at=CASE WHEN started_at IS NULL THEN ? ELSE started_at END WHERE id=? AND status='pending'`, time.Now(), taskID)
	_, _ = a.db.Exec(`UPDATE generation_items SET status='running' WHERE id=?`, itemID)
	a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "running"})

	// workflow_path 在库里可能是纯文件名（新）或绑定旧 cwd 的相对路径，统一按当前 dataDir 解析
	resolvedPath := a.workflowFilePath(workflowPath)
	workflowBytes, err := os.ReadFile(resolvedPath)
	if err != nil {
		a.failItem(itemID, taskID, fmt.Sprintf("read workflow failed: %v (resolved path: %s)", err, resolvedPath))
		return
	}
	var workflow map[string]any
	if err := json.Unmarshal(workflowBytes, &workflow); err != nil {
		a.failItem(itemID, taskID, fmt.Sprintf("parse workflow failed: %v", err))
		return
	}
	if _, hasNodes := workflow["nodes"]; hasNodes {
		workflow, err = uiToAPIFormat(workflow)
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
	input["negative_prompt"] = negativePrompt
	if err := injectDirectPrompt(workflow, mappingJSON, input); err != nil {
		a.failItem(itemID, taskID, fmt.Sprintf("inject prompt failed: %v", err))
		return
	}
	comfyPromptID, err := submitComfy(ctx, baseURL, workflow, fmt.Sprintf("comfyui-server-task-%d", taskID))
	if err != nil {
		a.failItem(itemID, taskID, fmt.Sprintf("submit to ComfyUI failed: %v", err))
		return
	}
	_, _ = a.db.Exec(`UPDATE generation_items SET status='submitted', comfy_prompt_id=? WHERE id=?`, comfyPromptID, itemID)
	a.log.Printf("item %d submitted to ComfyUI (prompt_id=%s)", itemID, comfyPromptID)
}

// failItem 标记 item 失败
func (a *app) failItem(itemID, taskID int64, errMsg string) {
	a.log.Printf("item %d failed: %s", itemID, errMsg)
	_, _ = a.db.Exec(`UPDATE generation_items SET status='failed', error_message=? WHERE id=?`, errMsg, itemID)
	_, _ = a.db.Exec(`UPDATE prompts SET status='failed', updated_at=CURRENT_TIMESTAMP WHERE id=(SELECT prompt_id FROM generation_items WHERE id=?)`, itemID)
	_, _ = a.db.Exec(`UPDATE generation_tasks SET status='failed', failed_count=failed_count+1, completed_at=? WHERE id=?`, time.Now(), taskID)
	a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "failed", "error": errMsg})
}

// downloader 轮询下载阶段（每5秒检查 submitted items 是否完成）
func (a *app) downloader() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		rows, err := a.db.Query(`SELECT i.id, i.task_id, i.comfy_prompt_id, t.comfyui_url
			FROM generation_items i
			JOIN generation_tasks t ON t.id=i.task_id
			WHERE i.status='submitted' AND i.comfy_prompt_id!=''
			ORDER BY i.id LIMIT 20`)
		if err != nil {
			a.log.Printf("downloader query error: %v", err)
			continue
		}
		type submittedItem struct {
			ItemID        int64
			TaskID        int64
			ComfyPromptID string
			BaseURL       string
		}
		var items []submittedItem
		for rows.Next() {
			var s submittedItem
			if err := rows.Scan(&s.ItemID, &s.TaskID, &s.ComfyPromptID, &s.BaseURL); err == nil {
				items = append(items, s)
			}
		}
		rows.Close()
		for _, s := range items {
			a.checkAndDownload(s.ItemID, s.TaskID, s.ComfyPromptID, s.BaseURL)
		}
	}
}

// checkAndDownload 检查单个 item 是否完成并下载图片
func (a *app) checkAndDownload(itemID, taskID int64, comfyPromptID, baseURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 检查任务是否被取消
	var taskStatus string
	_ = a.db.QueryRow(`SELECT status FROM generation_tasks WHERE id=?`, taskID).Scan(&taskStatus)
	if taskStatus == "cancelled" {
		return
	}

	// 查询 ComfyUI /history
	history, err := pollComfyHistory(ctx, baseURL, comfyPromptID)
	if err != nil {
		return // 还没完成或查询出错，下次再试
	}

	// 提取输出图片
	outputs, _ := history["outputs"].(map[string]any)
	if outputs == nil {
		a.failItem(itemID, taskID, "ComfyUI response missing outputs")
		return
	}
	found := false
	for _, nodeOutput := range outputs {
		nodeMap, _ := nodeOutput.(map[string]any)
		if nodeMap == nil {
			continue
		}
		images, _ := nodeMap["images"].([]any)
		for _, img := range images {
			imgMap, _ := img.(map[string]any)
			if imgMap == nil {
				continue
			}
			filename, _ := imgMap["filename"].(string)
			subfolder, _ := imgMap["subfolder"].(string)
			imgType, _ := imgMap["type"].(string)
			if filename == "" {
				continue
			}
			imgData, dlErr := downloadComfyImage(ctx, baseURL, filename, subfolder, imgType)
			if dlErr != nil {
				a.log.Printf("download image failed: %v", dlErr)
				continue
			}
			res, insertErr := a.db.Exec(`INSERT INTO images(generation_item_id, filename, storage_path, created_at) VALUES(?,?,'',?)`, itemID, filename, time.Now())
			if insertErr != nil {
				a.log.Printf("insert image record failed: %v", insertErr)
				continue
			}
			imageID, _ := res.LastInsertId()
			imgFilename := fmt.Sprintf("%s_%d.png", time.Now().Format("20060102_150405"), imageID)
			target := filepath.Join(a.dataDir, "images", imgFilename)
			if writeErr := os.WriteFile(target, imgData, 0o644); writeErr != nil {
				a.log.Printf("write image file failed: %v", writeErr)
				continue
			}
			_, _ = a.db.Exec(`UPDATE images SET storage_path=? WHERE id=?`, target, imageID)
			found = true
		}
	}
	if !found {
		a.failItem(itemID, taskID, "no images in ComfyUI output")
		return
	}
	// 成功
	_, _ = a.db.Exec(`UPDATE generation_items SET status='success' WHERE id=?`, itemID)
	_, _ = a.db.Exec(`UPDATE prompts SET status='done', completed_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP WHERE id=(SELECT prompt_id FROM generation_items WHERE id=?)`, itemID)
	// 检查 task 下所有 items 是否都完成了
	var pending int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM generation_items WHERE task_id=? AND status NOT IN ('success','failed')`, taskID).Scan(&pending)
	if pending == 0 {
		var successCt, failedCt int
		_ = a.db.QueryRow(`SELECT COUNT(*) FROM generation_items WHERE task_id=? AND status='success'`, taskID).Scan(&successCt)
		_ = a.db.QueryRow(`SELECT COUNT(*) FROM generation_items WHERE task_id=? AND status='failed'`, taskID).Scan(&failedCt)
		taskStatus := "completed"
		if failedCt > 0 && successCt == 0 {
			taskStatus = "failed"
		} else if failedCt > 0 {
			taskStatus = "completed" // 部分成功也算完成
		}
		_, _ = a.db.Exec(`UPDATE generation_tasks SET status=?, success_count=?, failed_count=?, completed_at=? WHERE id=?`, taskStatus, successCt, failedCt, time.Now(), taskID)
	}
	a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "success"})
}

// pollComfyHistory 查询 /history/{prompt_id} 来检查任务是否完成
func pollComfyHistory(ctx context.Context, baseURL, promptID string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/history/"+promptID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 || resp.StatusCode == 404 {
		return nil, fmt.Errorf("not ready")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("history HTTP %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if _, ok := result[promptID]; !ok {
		return nil, fmt.Errorf("prompt not found in history")
	}
	promptResult, _ := result[promptID].(map[string]any)
	if promptResult == nil {
		return nil, fmt.Errorf("invalid prompt result")
	}
	return promptResult, nil
}

func downloadComfyImage(ctx context.Context, baseURL, filename, subfolder, imgType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/view?filename=%s&subfolder=%s&type=%s", baseURL, filename, subfolder, imgType), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ComfyUI view HTTP %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func injectDirectPrompt(workflow map[string]any, mappingJSON string, input map[string]any) error {
	var mapping struct {
		Positive     map[string]string `json:"positive_prompt"`
		Negative     map[string]string `json:"negative_prompt"`
		Seed         map[string]string `json:"seed"`
		OutputPrefix map[string]string `json:"output_prefix"`
		Parameters   map[string]struct {
			NodeID  string `json:"node_id"`
			Field   string `json:"field"`
			Default any    `json:"default"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(mappingJSON), &mapping); err != nil {
		return err
	}
	positive, _ := input["positive_prompt"].(string)
	negative, _ := input["negative_prompt"].(string)
	// 优先使用 mapping 注入
	if mapping.Positive["node_id"] != "" {
		if err := setWorkflowField(workflow, mapping.Positive, positive); err != nil {
			return err
		}
	}
	if mapping.Negative["node_id"] != "" {
		if err := setWorkflowField(workflow, mapping.Negative, negative); err != nil {
			return err
		}
	}
	// mapping 为空时，自动查找 CLIPTextEncode 节点注入
	if mapping.Positive["node_id"] == "" && positive != "" {
		autoInjectText(workflow, positive, negative)
	}
	// Inject seed (random if not provided)
	params, _ := input["parameters"].(map[string]any)
	seedValue, hasSeed := input["seed"]
	if !hasSeed {
		seedValue, hasSeed = params["seed"]
	}
	if !hasSeed || seedValue == nil || seedValue == float64(0) {
		seedValue = rand.Int63n(999999999999999)
	}
	if mapping.Seed["node_id"] != "" {
		if err := setWorkflowField(workflow, mapping.Seed, seedValue); err != nil {
			return err
		}
	} else {
		// mapping 为空时，自动注入随机种子到 KSampler
		autoInjectSeed(workflow, seedValue)
	}
	// Inject output prefix with task ID for uniqueness
	if mapping.OutputPrefix["node_id"] != "" {
		prefix := fmt.Sprintf("comfyui-server/task-%d", time.Now().UnixMilli())
		if userPrefix, ok := input["output_prefix"].(string); ok && userPrefix != "" {
			prefix = userPrefix
		} else if userPrefix, ok := params["output_prefix"].(string); ok && userPrefix != "" {
			prefix = userPrefix
		}
		if err := setWorkflowField(workflow, mapping.OutputPrefix, expandDateTemplate(prefix)); err != nil {
			return err
		}
	}
	for name, location := range mapping.Parameters {
		value, ok := params[name]
		if !ok && location.Default != nil {
			value, ok = location.Default, true
		}
		if ok {
			// 展开 %date:...% 模板：uiToAPIFormat 已对工作流自带的 widget 值做过，
			// 但经由 parameters 显式覆盖是另一条入口，漏掉会让 ComfyUI 收到未替换的
			// %date:yyyy-MM-dd% 字面量——Windows 下目录名含 ':' 会被判为非法路径，
			// 整个节点执行失败（no images in ComfyUI output）。
			if err := setWorkflowField(workflow, map[string]string{"node_id": location.NodeID, "field": location.Field}, expandDateTemplate(value)); err != nil {
				return err
			}
		}
	}
	return nil
}

// autoInjectText 当没有 mapping 时，自动查找 CLIPTextEncode 节点注入提示词
// 通过启发式判断：文本中包含负面关键词的是负面提示词节点
func autoInjectText(workflow map[string]any, positive, negative string) {
	type textNode struct {
		id   string
		text string
	}
	var nodes []textNode
	for nodeID, raw := range workflow {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ct, _ := node["class_type"].(string)
		if ct != "CLIPTextEncode" {
			continue
		}
		inputs, _ := node["inputs"].(map[string]any)
		text, _ := inputs["text"].(string)
		nodes = append(nodes, textNode{id: nodeID, text: text})
	}
	negKeywords := []string{"nsfw", "bad", "worst", "low quality", "低质量", "模糊", "畸形", "错误", "多余", "丑陋", "崩坏", "变形", "水印", "logo"}
	for _, n := range nodes {
		lower := strings.ToLower(n.text)
		isNeg := false
		for _, kw := range negKeywords {
			if strings.Contains(lower, kw) {
				isNeg = true
				break
			}
		}
		if isNeg && negative != "" {
			setWorkflowField(workflow, map[string]string{"node_id": n.id, "field": "inputs.text"}, negative)
		} else if !isNeg && positive != "" {
			setWorkflowField(workflow, map[string]string{"node_id": n.id, "field": "inputs.text"}, positive)
		}
	}
}

// autoInjectSeed 自动查找 KSampler 节点注入种子
func autoInjectSeed(workflow map[string]any, seed any) {
	for _, raw := range workflow {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ct, _ := node["class_type"].(string)
		if ct != "KSampler" && ct != "KSamplerAdvanced" {
			continue
		}
		inputs, _ := node["inputs"].(map[string]any)
		if inputs != nil {
			inputs["seed"] = seed
		}
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

// uiToAPIFormat 将 ComfyUI UI 导出格式转换为 /prompt API 要求的格式
// UI格式: {nodes:[{id,type,widgets_values,inputs}], links:[...]}
// API格式: {"node_id": {class_type, inputs: {name: value_or_link}}}
func uiToAPIFormat(ui map[string]any) (map[string]any, error) {
	rawNodes, ok := ui["nodes"].([]any)
	if !ok {
		return nil, fmt.Errorf("workflow missing nodes array")
	}
	// 构建 link 查找表: link_id → [source_node_id, source_output_slot]
	links, _ := ui["links"].([]any)
	linkMap := map[int][]any{}
	for _, l := range links {
		if arr, ok := l.([]any); ok && len(arr) >= 3 {
			if id, ok := arr[0].(float64); ok {
				linkMap[int(id)] = arr
			}
		}
	}
	result := map[string]any{}
	for _, raw := range rawNodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		nodeID := fmt.Sprintf("%v", node["id"])
		classType, _ := node["type"].(string)
		widgetsValues, _ := node["widgets_values"].([]any)
		inputDefs, _ := node["inputs"].([]any)
		apiInputs := map[string]any{}
		// 先处理 link 类型的输入（非 widget）
		for _, inp := range inputDefs {
			def, ok := inp.(map[string]any)
			if !ok {
				continue
			}
			name, _ := def["name"].(string)
			if _, isWidget := def["widget"]; isWidget {
				continue
			}
			linkID, ok := def["link"].(float64)
			if !ok || linkID == 0 {
				continue
			}
			linkData, exists := linkMap[int(linkID)]
			if !exists || len(linkData) < 3 {
				continue
			}
			srcNode, _ := linkData[1].(float64)
			srcSlot, _ := linkData[2].(float64)
			apiInputs[name] = []any{fmt.Sprintf("%v", int(srcNode)), int(srcSlot)}
		}
		// 处理 widget 类型的输入，优先使用 widgets_values_named（含精确字段名映射）
		namedValues, _ := node["widgets_values_named"].(map[string]any)
		if len(namedValues) > 0 {
			for _, inp := range inputDefs {
				def, ok := inp.(map[string]any)
				if !ok {
					continue
				}
				if _, isWidget := def["widget"]; !isWidget {
					continue
				}
				name, _ := def["name"].(string)
				if v, exists := namedValues[name]; exists {
					if s, ok := v.(string); ok {
						v = resolveDateTemplate(s)
					}
					apiInputs[name] = v
				}
			}
		} else {
			// 回退到按顺序映射 widgets_values
			widgetIdx := 0
			for _, inp := range inputDefs {
				def, ok := inp.(map[string]any)
				if !ok {
					continue
				}
				if _, isWidget := def["widget"]; !isWidget {
					continue
				}
				name, _ := def["name"].(string)
				if widgetIdx < len(widgetsValues) {
					v := widgetsValues[widgetIdx]
					if s, ok := v.(string); ok {
						v = resolveDateTemplate(s)
					}
					apiInputs[name] = v
					widgetIdx++
				}
			}
		}
		result[nodeID] = map[string]any{
			"class_type": classType,
			"inputs":     apiInputs,
		}
	}
	return result, nil
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

func (a *app) waitAndDownload(ctx context.Context, baseURL, promptID string, taskID, itemID int64) error {
	for attempt := 0; attempt < 720; attempt++ {
		history, err := pollComfyHistory(ctx, baseURL, promptID)
		if err != nil {
			return err
		}
		statusMap, _ := history["status"].(map[string]any)
		if statusMap != nil {
			if statusStr, _ := statusMap["status_str"].(string); statusStr == "error" {
				return fmt.Errorf("ComfyUI task failed")
			}
		}
		outputs, _ := history["outputs"].(map[string]any)
		for _, nodeOutput := range outputs {
			nodeMap, _ := nodeOutput.(map[string]any)
			if nodeMap == nil {
				continue
			}
			images, _ := nodeMap["images"].([]any)
			for _, img := range images {
				imgMap, _ := img.(map[string]any)
				if imgMap == nil {
					continue
				}
				filename, _ := imgMap["filename"].(string)
				subfolder, _ := imgMap["subfolder"].(string)
				imgType, _ := imgMap["type"].(string)
				if filename == "" {
					continue
				}
				res, err := a.db.Exec(`INSERT INTO images(generation_item_id, filename, storage_path, created_at) VALUES(?,?,?,?)`, itemID, "pending", "", time.Now())
				if err != nil {
					return err
				}
				imageID, _ := res.LastInsertId()
				outFilename := fmt.Sprintf("%s_%d.png", time.Now().Format("20060102_150405"), imageID)
				target := filepath.Join(a.dataDir, "images", outFilename)
				imgData, dlErr := downloadComfyImage(ctx, baseURL, filename, subfolder, imgType)
				if dlErr != nil {
					return dlErr
				}
				if writeErr := os.WriteFile(target, imgData, 0o644); writeErr != nil {
					return writeErr
				}
				if _, err := a.db.Exec(`UPDATE images SET filename=?, storage_path=? WHERE id=?`, filepath.Base(target), target, imageID); err != nil {
					return err
				}
			}
			if len(images) > 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("ComfyUI task timed out")
}

// deleteComfyQueueItem 通知 ComfyUI 从队列中删除指定 prompt
func (a *app) deleteComfyQueueItem(baseURL, promptID string) {
	payload, _ := json.Marshal(map[string]any{
		"delete": []string{promptID},
	})
	req, err := http.NewRequest("POST", baseURL+"/queue", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		a.log.Printf("deleteComfyQueueItem failed: %v", err)
		return
	}
	resp.Body.Close()
}
