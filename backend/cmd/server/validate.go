package main

// ============================================================================
// 工作流提交前校验（POST /api/workflows/{id}/validate）
//
// 要解决的问题：工作流的合法性过去只有「真的提交给 ComfyUI」才知道，
// 于是每次改工作流的循环都是「改 → 提交 → 等 30s → 失败 → 人工排查」。
// 而 ComfyUI 的 /prompt 校验本来就是纯静态检查（节点类型是否存在、combo 取值
// 是否在候选里、必填输入是否齐全），完全可以提前到编辑阶段做。
//
// 设计要点：
//   1. 校验对象是「实际会发出去的 API 载荷」——先走 submitItem 用的同一个
//      uiToAPIFormat，再拿结果去比对。这样 UI→API 转换本身的问题
//      （新前端导出格式变化导致 widget 丢失之类）也会在这里暴露，而不是
//      等提交时炸。
//   2. 逐节点拉 object_info/<class_type>，不拉全量。全量响应可能有十几 MB
//      且包含所有模型列表，逐类型更省。
//   3. 「连不上 ComfyUI」和「工作流本身有问题」必须分开报——否则服务没起
//      的时候会显示成一堆「节点不存在」，把人带偏。
// ============================================================================

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// errNodeTypeNotFound 表示目标 ComfyUI 上不存在该节点类型（缺自定义节点包）。
var errNodeTypeNotFound = errors.New("node type not found")

// workflowIssue 是一条校验发现。字段刻意做得直白，前端可以直接渲染成
// 「节点 8 (CLIPLoader) · clip_name = 'krea2' 不在候选 [...]」。
type workflowIssue struct {
	NodeID     string   `json:"node_id"`
	ClassType  string   `json:"class_type"`
	Field      string   `json:"field,omitempty"`
	Kind       string   `json:"kind"`
	Message    string   `json:"message"`
	Value      string   `json:"value,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

type workflowValidation struct {
	OK           bool            `json:"ok"`
	Unreachable  bool            `json:"unreachable,omitempty"`
	Message      string          `json:"message,omitempty"`
	WorkflowID   int64           `json:"workflow_id"`
	WorkflowName string          `json:"workflow_name"`
	ComfyUIURL   string          `json:"comfyui_url"`
	CheckedAt    string          `json:"checked_at"`
	Errors       []workflowIssue `json:"errors"`
	Warnings     []workflowIssue `json:"warnings"`
	// Skipped 是提交时会被自动跳过的节点（前端专用节点、旁路/静音节点）。
	// 这是与 ComfyUI 前端一致的行为，不是错误，但必须让用户看得见，
	// 否则会以为「我图里明明有这个节点，怎么没了」。
	Skipped []skippedNode `json:"skipped_nodes,omitempty"`
}

// validateWorkflow 校验指定工作流能否被目标 ComfyUI 接受。
//
// body 可选：{"workflow_json": ...}。传了就用它（前端可校验「还没保存的编辑内容」），
// 没传则读库里那份文件。
func (a *app) validateWorkflow(w http.ResponseWriter, r *http.Request) {
	// PathValue("id") 可缺省：POST /api/workflows/validate 专门用来校验
	// 「还没保存的新工作流」，这时只认 body 里的 workflow_json。
	var (
		id         int64
		name       string
		storedPath string
	)
	if raw := r.PathValue("id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid workflow id")
			return
		}
		id = parsed
		if err := a.db.QueryRowContext(r.Context(),
			`SELECT name, workflow_path FROM workflows WHERE id=?`, id).Scan(&name, &storedPath); err != nil {
			writeError(w, http.StatusNotFound, "workflow not found")
			return
		}
	}

	// body 优先（前端校验「还没保存的编辑内容」）；没给就退回库里那份文件。
	var input struct {
		WorkflowJSON json.RawMessage `json:"workflow_json"`
	}
	var rawJSON []byte
	bodyErr := decodeJSON(r, &input)
	if bodyErr == nil && len(input.WorkflowJSON) > 0 {
		// 归一化形态：客户端可能把导出文件的原文当字符串塞进来（见 normalizeWorkflowJSON），
		// 不剥这一层，校验会误报「工作流不是合法 JSON」。
		rawJSON = normalizeWorkflowJSON(input.WorkflowJSON)
	} else if id > 0 {
		var readErr error
		rawJSON, readErr = readWorkflowFile(a.workflowFilePath(storedPath))
		if readErr != nil {
			writeError(w, http.StatusInternalServerError, "read workflow file failed")
			return
		}
	} else {
		writeError(w, http.StatusBadRequest, "workflow_json is required")
		return
	}

	comfyURL, err := a.setting(r.Context(), "comfyui_url")
	if err != nil || comfyURL == "" {
		writeError(w, http.StatusBadRequest, "comfyui_url is not configured")
		return
	}

	result := workflowValidation{
		WorkflowID:   id,
		WorkflowName: name,
		ComfyUIURL:   comfyURL,
		CheckedAt:    time.Now().Format(time.RFC3339),
		Errors:       make([]workflowIssue, 0),
		Warnings:     make([]workflowIssue, 0),
	}

	result.Skipped = collectSkippedNodes(rawJSON)

	// 第一遍转换只为两件事：早点暴露「不支持的结构」这类格式错误，以及
	// 拿到「会提交哪些节点类型」。这一步不带节点定义，控件值可能还没还原，
	// 所以结果不能拿去校验。
	preliminary, err := buildSubmitPayload(rawJSON, nil)
	if err != nil {
		// 「本系统还不支持的结构」和「工作流本身写错了」必须分开报，
		// 否则用户会去改一份其实没问题的文件。
		kind := "workflow_format"
		if errors.Is(err, errUnsupportedWorkflowFeature) {
			kind = "unsupported_feature"
		}
		result.Errors = append(result.Errors, workflowIssue{Kind: kind, Message: err.Error()})
		writeJSON(w, http.StatusOK, result)
		return
	}
	if len(preliminary) == 0 {
		result.Errors = append(result.Errors, workflowIssue{Kind: "workflow_format", Message: "工作流里没有任何可提交的节点"})
		writeJSON(w, http.StatusOK, result)
		return
	}

	// 拉所需的节点定义：任何一次请求出现网络错误就判定为「不可达」。
	// 必须和「工作流有问题」分开报——服务没起时报一堆「节点不存在」会把人带偏。
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	classTypes := make([]string, 0, len(preliminary))
	seenType := map[string]bool{}
	for _, node := range preliminary {
		if node.ClassType != "" && !seenType[node.ClassType] {
			seenType[node.ClassType] = true
			classTypes = append(classTypes, node.ClassType)
		}
	}
	sort.Strings(classTypes)

	nodeDefs := make(map[string]nodeInputDef, len(classTypes)) // Spec 为 nil = 该类型不存在
	for _, classType := range classTypes {
		def, err := fetchNodeInputDef(ctx, comfyURL, classType)
		switch {
		case errors.Is(err, errNodeTypeNotFound):
			nodeDefs[classType] = nodeInputDef{}
		case err != nil:
			result.Unreachable = true
			result.Message = fmt.Sprintf("无法从 ComfyUI(%s) 读取节点定义：%v", comfyURL, err)
			writeJSON(w, http.StatusOK, result)
			return
		default:
			nodeDefs[classType] = def
		}
	}

	// 第二遍转换：这次带上节点定义，早期导出格式的控件值（裸位置数组）在这一步
	// 才被还原。少了这一步，这类工作流会被整份误判成「所有必填输入都缺失」。
	// 节点定义已经拿到手，直接用内存里的结果，不再发第二次请求。
	payload := preliminary
	if nested := buildSubmitPayloadWithDefs(rawJSON, nodeDefs); nested != nil {
		payload = nested
	}
	result.Warnings = append(result.Warnings, collectWidgetValueMismatches(rawJSON, nodeDefs)...)

	// 每个节点上「取值不在列表」的字段，留给后面的自动纠正提示复用。
	offendingFields := make(map[string][]string, 2)

	for _, node := range payload {
		def := nodeDefs[node.ClassType]
		spec := def.Spec
		if spec == nil {
			result.Errors = append(result.Errors, workflowIssue{
				NodeID:    node.ID,
				ClassType: node.ClassType,
				Kind:      "node_type_missing",
				Message:   "目标 ComfyUI 上没有这个节点类型（缺自定义节点/插件）",
			})
			continue
		}
		required, optional := splitInputSpec(spec)

		// ① 取值合法性：combo 字段的值必须在候选列表里，否则 ComfyUI 直接 400。
		for _, field := range sortedKeys(node.Inputs) {
			value := node.Inputs[field]
			if isLinkValue(value) {
				continue // 连线由上游提供，值本身不参与校验
			}
			fieldSpec, known := required[field]
			if !known {
				fieldSpec, known = optional[field]
			}
			if !known {
				result.Warnings = append(result.Warnings, workflowIssue{
					NodeID: node.ID, ClassType: node.ClassType, Field: field,
					Kind:    "unknown_field",
					Message: "节点定义里没有这个输入名，会被 ComfyUI 忽略（通常说明导出格式与当前版本不匹配）",
					Value:   describeValue(value),
				})
				continue
			}
			options := comboOptions(fieldSpec)
			if len(options) == 0 {
				continue
			}
			text, ok := value.(string)
			if !ok || containsString(options, text) {
				continue
			}
			result.Errors = append(result.Errors, workflowIssue{
				NodeID: node.ID, ClassType: node.ClassType, Field: field,
				Kind:       "value_not_in_list",
				Message:    fmt.Sprintf("取值 %q 不在候选列表里", text),
				Value:      text,
				Candidates: options,
			})
			offendingFields[node.ID] = append(offendingFields[node.ID], field)
		}

		// ② 必填齐全：required 里既没连线也没给值的，ComfyUI 会报 required_input_missing。
		for _, field := range sortedKeys(required) {
			if _, ok := node.Inputs[field]; !ok {
				result.Errors = append(result.Errors, workflowIssue{
					NodeID: node.ID, ClassType: node.ClassType, Field: field,
					Kind:    "required_missing",
					Message: "缺少必填输入（既没有值也没有连线）",
				})
			}
		}
	}

	// ③ 能自动纠正的串位：值虽然不在本字段的候选里，却是同一个节点另一个字段的
	// 合法取值——多半是导出时控件值串位了（ComfyUI 里点运行正常，导出成 JSON 就
	// 对不上名字）。提交时会把值挪回去再试一次，这里先讲清楚，
	// 免得用户看到一片红字以为这份工作流报废了。
	// 用副本试算：校验用的载荷不能被改动。
	for _, node := range payload {
		fields := offendingFields[node.ID]
		if len(fields) == 0 {
			continue
		}
		def := nodeDefs[node.ClassType]
		if def.Spec == nil {
			continue
		}
		for _, repair := range planNodeRepairs(node.ID, node.ClassType, cloneInputs(node.Inputs), fields, def.Spec) {
			result.Warnings = append(result.Warnings, workflowIssue{
				NodeID:    repair.NodeID,
				ClassType: repair.ClassType,
				Field:     repair.Field,
				Kind:      "auto_repairable",
				Message:   repair.String() + "。提交时会自动这么纠正后重试一次，工作流文件不会被改动",
			})
		}
	}

	result.OK = len(result.Errors) == 0
	writeJSON(w, http.StatusOK, result)
}

// payloadNode 是从工作流里抽出来的「一个节点会怎么发出去」。
type payloadNode struct {
	ID        string
	ClassType string
	Inputs    map[string]any
}

// nodeInputDef 是一个节点类型的定义：既要字段声明（校验取值/必填），
// 也要控件位顺序（还原老格式工作流的裸位置数组）。
type nodeInputDef struct {
	Spec  map[string]map[string]any
	Slots []widgetSlot
}

// buildSubmitPayloadWithDefs 用已经取到的节点定义再转一遍工作流。
//
// 拿到节点定义之后再转，早期导出格式（inputs 里没有任何 widget 标记）的控件值
// 才还原得出来。返回 nil 表示这次转换失败——调用方应保留第一遍的结果。
func buildSubmitPayloadWithDefs(rawJSON []byte, defs map[string]nodeInputDef) []payloadNode {
	order := func(classType string) ([]widgetSlot, bool) {
		def, ok := defs[classType]
		if !ok || def.Spec == nil {
			return nil, false
		}
		return def.Slots, true
	}
	nodes, err := buildSubmitPayload(rawJSON, order)
	if err != nil {
		return nil
	}
	return nodes
}

// buildSubmitPayload 把工作流 JSON 转成实际会提交的节点集合。
//
// 走的是 submitItem 用的同一个 uiToAPIFormatWithOrder，因此「校验通过」和
// 「提交成功」看的是同一份数据——UI→API 转换的问题也会在这里暴露。
// order 为 nil 时只认得自带控件顺序的导出格式（见 widgetorder.go）。
// 返回的 slice 按节点 id 数值排序，保证多次校验的结果顺序稳定。
func buildSubmitPayload(rawJSON []byte, order widgetOrderLookup) ([]payloadNode, error) {
	var workflow map[string]any
	if err := json.Unmarshal(rawJSON, &workflow); err != nil {
		return nil, fmt.Errorf("工作流不是合法 JSON：%v", err)
	}
	if _, hasNodes := workflow["nodes"]; hasNodes {
		converted, err := uiToAPIFormatWithOrder(workflow, order)
		if err != nil {
			return nil, fmt.Errorf("UI 格式转 API 格式失败：%v", err)
		}
		workflow = converted
	}

	nodes := make([]payloadNode, 0, len(workflow))
	for id, raw := range workflow {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		classType, _ := node["class_type"].(string)
		inputs, _ := node["inputs"].(map[string]any)
		if inputs == nil {
			inputs = map[string]any{}
		}
		nodes = append(nodes, payloadNode{ID: id, ClassType: classType, Inputs: inputs})
	}
	sort.Slice(nodes, func(i, j int) bool {
		left, leftErr := strconv.Atoi(nodes[i].ID)
		right, rightErr := strconv.Atoi(nodes[j].ID)
		if leftErr == nil && rightErr == nil {
			return left < right
		}
		return nodes[i].ID < nodes[j].ID
	})
	return nodes, nil
}

// fetchNodeInputDef 取单个节点类型的定义：字段声明 + 控件位顺序。
// 类型不存在时返回 errNodeTypeNotFound。
//
// 两类信息来自同一次请求——控件顺序必须从原始 JSON 流式解析（保序），
// 字段声明则照常用 map 解析，两边各取所需，不额外发请求。
func fetchNodeInputDef(ctx context.Context, baseURL, classType string) (nodeInputDef, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/object_info/"+url.PathEscape(classType), nil)
	if err != nil {
		return nodeInputDef{}, err
	}
	response, _, err := tracedDo(&http.Client{Timeout: 8 * time.Second}, request, "object_info", 2048)
	if err != nil {
		return nodeInputDef{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nodeInputDef{}, errNodeTypeNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nodeInputDef{}, fmt.Errorf("object_info HTTP %d", response.StatusCode)
	}

	// 只解析目标类型那一段，不为一个节点消化整包定义。
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload); err != nil {
		return nodeInputDef{}, err
	}
	raw, ok := payload[classType]
	if !ok {
		return nodeInputDef{}, errNodeTypeNotFound // 未知类型时 ComfyUI 回 {}
	}
	inputJSON, err := extractInputSection(raw)
	if err != nil {
		return nodeInputDef{}, err
	}

	def := nodeInputDef{Spec: map[string]map[string]any{}}
	if len(inputJSON) == 0 {
		return def, nil
	}
	if err := json.Unmarshal(inputJSON, &def.Spec); err != nil || def.Spec == nil {
		def.Spec = map[string]map[string]any{}
	}
	// 控件顺序解析失败不该让整次校验失败：退回「还原不出控件值」的保守结果，
	// 由 collectWidgetValueMismatches 把这件事报给用户。
	// 留痕同样必要——它会以「所有必填都缺失」的面目出现，看不出因果。
	if slots, err := parseWidgetSlots(inputJSON); err == nil {
		def.Slots = slots
	} else {
		debugEvent("widget_slots_unparsed", map[string]any{
			"class_type": classType,
			"url":        baseURL,
			"error":      err.Error(),
		})
	}
	return def, nil
}

// collectWidgetValueMismatches 找出「位置数组长度和节点定义对不上」的老格式节点。
//
// 这类工作流多半是旧版本 ComfyUI 导出的（节点后来加了控件），值只能按前缀对齐，
// 有可能对错位。报出来是为了让人知道「这份文件该重新导出一次了」，
// 而不是等到出图结果不对才回头怀疑系统。
func collectWidgetValueMismatches(rawJSON []byte, defs map[string]nodeInputDef) []workflowIssue {
	var workflow map[string]any
	if err := json.Unmarshal(rawJSON, &workflow); err != nil {
		return nil
	}
	nodes, _ := workflow["nodes"].([]any)
	issues := make([]workflowIssue, 0)
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok || nodeSkipReason(node) != "" {
			continue
		}
		// 只在「文件自己不带控件顺序」的老格式上检查，其他格式由名字/标记保证正确。
		if named, _ := node["widgets_values_named"].(map[string]any); len(named) > 0 {
			continue
		}
		if hasWidgetMarkers(node) {
			continue
		}
		values, _ := node["widgets_values"].([]any)
		if len(values) == 0 {
			continue
		}
		classType, _ := node["type"].(string)
		def, known := defs[classType]
		if !known || def.Spec == nil || len(def.Slots) == 0 {
			continue
		}
		expected := len(expandWidgetSlots(def.Slots))
		withoutControl := 0
		for _, slot := range def.Slots {
			if slot.Name != "" {
				withoutControl++
			}
		}
		if len(values) == expected || len(values) == withoutControl {
			continue
		}
		id := ""
		if number, ok := numericValue(node["id"]); ok {
			id = strconv.Itoa(int(number))
		}
		issues = append(issues, workflowIssue{
			NodeID:    id,
			ClassType: classType,
			Kind:      "widget_values_mismatch",
			Message: fmt.Sprintf("这份工作流是旧版导出的：控件值有 %d 个，当前节点定义有 %d 个位置。"+
				"已按顺序前缀对齐，建议在 ComfyUI 里重新导出一次", len(values), expected),
		})
	}
	return issues
}

// splitInputSpec 把 input 段拆成 required / optional 两份字段声明。
func splitInputSpec(spec map[string]map[string]any) (required, optional map[string]any) {
	required, optional = map[string]any{}, map[string]any{}
	if raw, ok := spec["required"]; ok {
		required = raw
	}
	if raw, ok := spec["optional"]; ok {
		optional = raw
	}
	return required, optional
}

// comboOptions 从字段声明里提取 combo 候选值；不是 combo 类型则返回 nil。
//
// ComfyUI 的字段声明有两代格式，必须都认：
//
//	旧: [["a","b"], {"default":"a"}]
//	新: ["COMBO", {"options":["a","b"]}]
//
// 像 ["MODEL"] 这种是连线型声明（首元素是类型名），返回 nil 表示不校验取值。
func comboOptions(spec any) []string {
	arr, ok := spec.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	if list, ok := arr[0].([]any); ok {
		options := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				options = append(options, text)
			}
		}
		return options
	}
	if kind, ok := arr[0].(string); ok && kind == "COMBO" && len(arr) > 1 {
		if meta, ok := arr[1].(map[string]any); ok {
			if list, ok := meta["options"].([]any); ok {
				options := make([]string, 0, len(list))
				for _, item := range list {
					if text, ok := item.(string); ok {
						options = append(options, text)
					}
				}
				return options
			}
		}
	}
	return nil
}

// isLinkValue 判断一个输入值是不是连线（API 格式里是 [上游节点id, 输出槽位]）。
func isLinkValue(value any) bool {
	arr, ok := value.([]any)
	return ok && len(arr) == 2
}

// sortedKeys 返回 map 的键并排序，让校验结果的顺序可复现。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func containsString(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

// skippedNode 记录一个「提交时会被自动跳过」的节点。
type skippedNode struct {
	NodeID    string `json:"node_id"`
	ClassType string `json:"class_type"`
	Reason    string `json:"reason"`
}

// collectSkippedNodes 列出提交时会被跳过的节点。
//
// 这些节点不会进入 /prompt 载荷，因为 ComfyUI 前端也不会把它们放进去
// （见 comfyui.go 的 nodeSkipReason）：前端专用节点（Note / MarkdownNote /
// Reroute / PrimitiveNode）服务端根本没有，旁路（mode=4）与静音（mode=2）
// 节点前端也会剔除。不报出来会让人以为「我图里明明有这个节点」。
func collectSkippedNodes(rawJSON []byte) []skippedNode {
	var workflow map[string]any
	if err := json.Unmarshal(rawJSON, &workflow); err != nil {
		return nil
	}
	nodes, _ := workflow["nodes"].([]any)
	skipped := make([]skippedNode, 0, 2)
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		reason := nodeSkipReason(node)
		if reason == "" {
			continue
		}
		classType, _ := node["type"].(string)
		id := ""
		if number, ok := numericValue(node["id"]); ok {
			id = strconv.Itoa(int(number))
		}
		skipped = append(skipped, skippedNode{NodeID: id, ClassType: classType, Reason: reason})
	}
	sort.Slice(skipped, func(i, j int) bool {
		left, leftErr := strconv.Atoi(skipped[i].NodeID)
		right, rightErr := strconv.Atoi(skipped[j].NodeID)
		if leftErr == nil && rightErr == nil {
			return left < right
		}
		return skipped[i].NodeID < skipped[j].NodeID
	})
	return skipped
}

// describeValue 把输入值渲染成短字符串，用于错误提示。
func describeValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return truncateRunes(typed, 80)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return truncateRunes(string(encoded), 80)
	}
}
