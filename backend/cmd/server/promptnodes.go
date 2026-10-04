package main

// ============================================================================
// 提示词落点解析
//
// 本系统对一份工作流只关心一件事：**正向提示词、负向提示词各写到哪个节点的
// 哪个字段**。其余内容（模型、尺寸、步数、连线）一律原样提交给 ComfyUI，不做
// 参数识别、不做类型白名单、不改工作流的其他部分。
//
// 过去是反过来的：正负向落点写死在 CLIPTextEncode 这个类型上，种子只认
// KSampler，另外还要把工作流里**每一个**标量字段都解析成「可编辑参数」并在提交时
// 全量写回。结果是换一个插件编码器节点就**静默失效**，而参数那份副本一旦与工作流
// JSON 漂移，提交时就会把工作流的真实值覆盖掉（比如换了主模型又被改回旧的）。
//
// 现在只有两条解析路径，都只看结构、不看类型：
//
//	1. API 格式（uiToAPIFormat 之后）—— 首选。顺着条件采样器的 positive /
//	   negative 连线向上游走，落到带文本框的节点上。字段名是 ComfyUI 真正认的名字。
//	2. UI 导出格式（nodes + links）—— ComfyUI 连不上时也能用。字段名取自
//	   widgets_values_named；更老的导出格式没有 named，只能拿到节点 id，
//	   字段名留到提交时再定（那时 API 格式已经还原好了）。
//
// ⚠️ 落点是「节点 + 字段」，不能只记节点：TextEncodeQwenImage21 这类节点在
// 同一个节点上同时有 prompt 和 negative_prompt 两个框，正负向都从它出发。
//
// ⚠️ 只有节点和字段都相同才算「负向与正向同源」：Krea2 全系把负向从正向
// ConditioningZeroOut 出来的，工作流里根本没有负向文本，负向提示词无处可写。
// 这种情况要在界面上明说，不能悄悄丢弃。
// ============================================================================

import (
	"sort"
	"strconv"
	"strings"
)

// promptRole 区分一处落点是写给正向还是写给负向的。
type promptRole int

const (
	rolePositive promptRole = iota
	roleNegative
)

// positiveFieldHints 是「正向提示词」倾向的字段名，按优先级排列。
// 只看字段名和值类型，不看节点类型——这是能适配未知编码器的关键。
var positiveFieldHints = []string{
	"positive_prompt", "positive", "prompt", "text", "text_g", "text_l", "caption", "template",
}

// negativeFieldHints 是「负向提示词」倾向的字段名，按优先级排列。
var negativeFieldHints = []string{
	"negative_prompt", "neg_prompt", "negative", "uncond",
}

// maxPromptHops 限制向上游穿透的层数，防止成环的工作流把解析拖死。
const maxPromptHops = 8

// promptTarget 是一处提示词落点：某个节点的某个输入字段。
//
// Field 允许为空：更老的导出格式（连 widgets_values_named 都没有）在离线时
// 拿不到字段名，只拿得到节点 id——提交时 ComfyUI 必然在线，届时再按角色解析。
type promptTarget struct {
	NodeID string `json:"node_id,omitempty"`
	Field  string `json:"field,omitempty"`
}

// promptTargets 是一份工作流解析出来的完整落点信息。
type promptTargets struct {
	Positive *promptTarget `json:"positive"`
	Negative *promptTarget `json:"negative"`
	// NegativeShared 为真表示负向解析到的位置与正向完全一致——工作流从正向
	// zero-out 出负向，没有独立的负向文本框，负向提示词写不进去。
	NegativeShared bool `json:"negative_shared"`
}

// newPromptTargets 组装结果，并判定正负向是不是撞在了同一处。
func newPromptTargets(positive, negative *promptTarget) promptTargets {
	shared := false
	if positive != nil && negative != nil {
		shared = positive.NodeID == negative.NodeID && positive.Field == negative.Field
	}
	return promptTargets{Positive: positive, Negative: negative, NegativeShared: shared}
}

// promptCandidate 是编辑器里「提示词节点」下拉框的一个选项。
type promptCandidate struct {
	NodeID    string `json:"node_id"`
	ClassType string `json:"class_type"`
	Field     string `json:"field,omitempty"`
	Preview   string `json:"preview"`
	// Role 是自动识别给出的角色提示（positive / negative / 空）。
	Role string `json:"role,omitempty"`
}

// ---------------------------------------------------------------------------
// 字段挑选
// ---------------------------------------------------------------------------

// isNegativeFieldName 判断字段名是不是「负向专用」。
// 只看名字里的 neg / uncond——插件里的负向框几乎都带这两个词根。
func isNegativeFieldName(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "neg") || lower == "uncond"
}

// promptFieldsOf 挑出节点上所有「像提示词文本框」的字符串字段。
// 返回顺序固定为「正向类在前、负向类在后」，让 pickPromptField 的结果可预测。
func promptFieldsOf(inputs map[string]any) []string {
	if len(inputs) == 0 {
		return nil
	}
	lowerToActual := make(map[string]string, len(inputs))
	for key := range inputs {
		lowerToActual[strings.ToLower(key)] = key
	}
	fields := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	appendHint := func(hint string) {
		actual, ok := lowerToActual[hint]
		if !ok || seen[actual] {
			return
		}
		// 非字符串的值不是文本框（可能是连线的占位、也可能是数值控件）
		if _, isString := inputs[actual].(string); !isString {
			return
		}
		seen[actual] = true
		fields = append(fields, actual)
	}
	for _, hint := range positiveFieldHints {
		appendHint(hint)
	}
	for _, hint := range negativeFieldHints {
		appendHint(hint)
	}
	// 没有命中任何提示词字段时返回 nil 而不是空切片：调用方一律用 len() 判断，
	// 但 nil 让「这个节点不是文本框」这件事在 == nil 的断言里也成立。
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// pickPromptField 在一组候选字段里为指定角色挑一个。
//
//	positive：第一个「非负向」的字段。
//	negative：第一个「负向专用」字段；没有就退回正向那个字段——两侧于是完全
//	          相同，调用方据此判定「这份工作流没有独立负向文本」。
func pickPromptField(fields []string, role promptRole) string {
	positiveField := ""
	for _, field := range fields {
		if !isNegativeFieldName(field) {
			positiveField = field
			break
		}
	}
	if role == rolePositive {
		if positiveField != "" {
			return positiveField
		}
		// 只剩负向字段的极端情况：至少把值写进去，不要静默丢弃
		if len(fields) > 0 {
			return fields[0]
		}
		return ""
	}
	for _, field := range fields {
		if isNegativeFieldName(field) {
			return field
		}
	}
	return positiveField
}

// ---------------------------------------------------------------------------
// API 格式：结构解析（首选路径）
// ---------------------------------------------------------------------------

// sortedNodeIDs 返回 API 格式工作流的节点 id，数值升序，保证多次解析结果一致。
func sortedNodeIDs(workflow map[string]any) []string {
	ids := make([]string, 0, len(workflow))
	for id := range workflow {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, leftErr := strconv.Atoi(ids[i])
		right, rightErr := strconv.Atoi(ids[j])
		if leftErr == nil && rightErr == nil {
			return left < right
		}
		return ids[i] < ids[j]
	})
	return ids
}

// nodeInputs 取节点的 inputs；取不到时返回空 map（不是 nil，省掉调用方判空）。
//
// 注意：字段存在时返回的是原 map（不是副本），调用方可以直接改写。
func nodeInputs(node map[string]any) map[string]any {
	inputs, _ := node["inputs"].(map[string]any)
	if inputs == nil {
		return map[string]any{}
	}
	return inputs
}

// nodeClassType 取节点的 class_type（API 格式）。
func nodeClassType(node map[string]any) string {
	classType, _ := node["class_type"].(string)
	return classType
}

// linkUpstream 取出一个连线输入指向的上游节点 id；不是连线则返回空串。
// API 格式里连线写成 [上游节点id, 输出槽位]，节点 id 可能是字符串也可能是数字。
func linkUpstream(value any) string {
	arr, ok := value.([]any)
	if !ok || len(arr) != 2 {
		return ""
	}
	switch head := arr[0].(type) {
	case string:
		return head
	case float64:
		return strconv.Itoa(int(head))
	}
	return ""
}

// normalizeNodeType 去掉大小写与分隔符，便于按名字片段判断节点种类：
// "KSampler" / "K_Sampler" / "ksampler-advanced" 都归一成不含分隔符的小写。
func normalizeNodeType(classType string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "", ".", "").Replace(strings.ToLower(classType))
}

// findConditioningSampler 找出「同时把 positive 和 negative 作为连线输入」的节点。
//
// 这是条件采样器的通用特征，Krea2 / Anima / Pony / SDXL / Qwen-Image 各家都一样，
// 与 class_type 无关。名字里带 sampler 的优先。
func findConditioningSampler(nodes map[string]map[string]any, ids []string) string {
	samplerID := ""
	for _, id := range ids {
		inputs := nodeInputs(nodes[id])
		if linkUpstream(inputs["positive"]) == "" || linkUpstream(inputs["negative"]) == "" {
			continue
		}
		if strings.Contains(normalizeNodeType(nodeClassType(nodes[id])), "sampler") {
			return id
		}
		if samplerID == "" {
			samplerID = id
		}
	}
	return samplerID
}

// walkToTextTarget 从 startID 出发沿连线上溯，找到第一个「带提示词文本框」的节点，
// 并按角色挑出字段。
//
// 优先走名字里带 condition 的连线（条件流），其余连线作为备选，
// 这样即使中间隔着 ConditioningKrea2Rebalance / ConditioningZeroOut 之类的
// 透传节点也能落到真正的编码器上。
func walkToTextTarget(nodes map[string]map[string]any, startID string, role promptRole) *promptTarget {
	if startID == "" {
		return nil
	}
	visited := make(map[string]bool, maxPromptHops)
	frontier := []string{startID}
	for hop := 0; hop < maxPromptHops && len(frontier) > 0; hop++ {
		next := make([]string, 0, len(frontier))
		for _, id := range frontier {
			if visited[id] {
				continue
			}
			visited[id] = true
			node, ok := nodes[id]
			if !ok {
				continue
			}
			inputs := nodeInputs(node)
			if fields := promptFieldsOf(inputs); len(fields) > 0 {
				return &promptTarget{NodeID: id, Field: pickPromptField(fields, role)}
			}
			preferred := make([]string, 0, 2)
			others := make([]string, 0, 2)
			for _, name := range sortedKeys(inputs) {
				upstream := linkUpstream(inputs[name])
				if upstream == "" {
					continue
				}
				if strings.Contains(strings.ToLower(name), "condition") {
					preferred = append(preferred, upstream)
				} else {
					others = append(others, upstream)
				}
			}
			next = append(next, preferred...)
			next = append(next, others...)
		}
		frontier = next
	}
	return nil
}

// resolvePromptTargets 在 API 格式工作流上解析正负向落点。
//
// 返回的两侧都可以为 nil（该侧没有连编码器，或结构识别不出来），调用方自行取舍；
// 也可以两侧指向同一节点的不同字段（Qwen-Image 的 prompt / negative_prompt）。
func resolvePromptTargets(workflow map[string]any) promptTargets {
	nodes := make(map[string]map[string]any, len(workflow))
	for id, raw := range workflow {
		if node, ok := raw.(map[string]any); ok {
			nodes[id] = node
		}
	}
	ids := sortedNodeIDs(workflow)
	samplerID := findConditioningSampler(nodes, ids)
	if samplerID == "" {
		return promptTargets{}
	}
	inputs := nodeInputs(nodes[samplerID])
	return newPromptTargets(
		walkToTextTarget(nodes, linkUpstream(inputs["positive"]), rolePositive),
		walkToTextTarget(nodes, linkUpstream(inputs["negative"]), roleNegative),
	)
}

// seedInputNodes 列出所有「用标量 seed 输入」的节点 id。
//
// 判据是字段本身而不是节点类型：自定义采样器、Noise / RandomNoise 之类名字五花八门，
// 按名字枚举必然漏。seed 若是连线则跳过——那种情况值由上游决定，写进去反而会破坏连线。
func seedInputNodes(workflow map[string]any) []string {
	ids := make([]string, 0, 2)
	for _, id := range sortedNodeIDs(workflow) {
		node, ok := workflow[id].(map[string]any)
		if !ok {
			continue
		}
		inputs := nodeInputs(node)
		if _, has := inputs["seed"]; !has {
			continue
		}
		if linkUpstream(inputs["seed"]) != "" {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// ---------------------------------------------------------------------------
// UI 导出格式：图遍历（离线兜底路径）
// ---------------------------------------------------------------------------

// uiPromptFieldsOf 从 UI 导出格式的节点上取提示词字段。
//
// 只有 widgets_values_named 能同时给出「字段名 + 值」，所以老格式（没有 named 的）
// 在这里一律返回空——宁可不自动识别，也不猜错落点（猜错会静默出错图）。
func uiPromptFieldsOf(node map[string]any) []string {
	named, ok := node["widgets_values_named"].(map[string]any)
	if !ok {
		return nil
	}
	return promptFieldsOf(named)
}

// uiInputLinks 取 UI 格式节点的「输入名 → 连线 id」，忽略没有连线的输入。
func uiInputLinks(node map[string]any) map[string]int {
	links := map[string]int{}
	for _, raw := range asSlice(node["inputs"]) {
		def, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := def["name"].(string)
		if name == "" {
			continue
		}
		link, ok := numericValue(def["link"])
		if !ok || link == 0 {
			continue
		}
		links[name] = int(link)
	}
	return links
}

// uiLinkOrigins 把 UI 格式的 links 数组转成「连线 id → 上游节点 id」。
// 兼容数组形式 [id, origin_id, origin_slot, target_id, ...] 与字典形式。
func uiLinkOrigins(ui map[string]any) map[int]int {
	origins := make(map[int]int, len(asSlice(ui["links"])))
	for _, raw := range asSlice(ui["links"]) {
		id, origin := 0, 0
		switch typed := raw.(type) {
		case []any:
			if len(typed) < 2 {
				continue
			}
			left, okLeft := numericValue(typed[0])
			right, okRight := numericValue(typed[1])
			if !okLeft || !okRight {
				continue
			}
			id, origin = int(left), int(right)
		case map[string]any:
			left, okLeft := numericValue(typed["id"])
			right, okRight := numericValue(typed["origin_id"])
			if !okLeft || !okRight {
				continue
			}
			id, origin = int(left), int(right)
		default:
			continue
		}
		origins[id] = origin
	}
	return origins
}

// uiNodesByID 把 UI 格式的 nodes 数组索引成 id → 节点，并返回升序 id 列表。
func uiNodesByID(ui map[string]any) (map[int]map[string]any, []int) {
	nodes := map[int]map[string]any{}
	ids := make([]int, 0, len(asSlice(ui["nodes"])))
	for _, raw := range asSlice(ui["nodes"]) {
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
	sort.Ints(ids)
	return nodes, ids
}

// walkUIToTextTarget 是 walkToTextTarget 的 UI 格式版本。
func walkUIToTextTarget(nodes map[int]map[string]any, origins map[int]int, startID int, role promptRole) *promptTarget {
	visited := make(map[int]bool, maxPromptHops)
	frontier := []int{startID}
	for hop := 0; hop < maxPromptHops && len(frontier) > 0; hop++ {
		next := make([]int, 0, len(frontier))
		for _, id := range frontier {
			if visited[id] {
				continue
			}
			visited[id] = true
			node, ok := nodes[id]
			if !ok {
				continue
			}
			if fields := uiPromptFieldsOf(node); len(fields) > 0 {
				return &promptTarget{NodeID: strconv.Itoa(id), Field: pickPromptField(fields, role)}
			}
			links := uiInputLinks(node)
			preferred := make([]int, 0, 2)
			others := make([]int, 0, 2)
			for _, name := range sortedKeys(links) {
				origin, ok := origins[links[name]]
				if !ok {
					continue
				}
				if strings.Contains(strings.ToLower(name), "condition") {
					preferred = append(preferred, origin)
				} else {
					others = append(others, origin)
				}
			}
			next = append(next, preferred...)
			next = append(next, others...)
		}
		frontier = next
	}
	return nil
}

// resolvePromptTargetsFromUI 在 UI 导出格式上解析正负向落点（不需要 ComfyUI 在线）。
//
// 老格式（没有 widgets_values_named）会解析不出落点，返回空——这没关系：
// 提交时走的是 API 格式那条路径，那时值已经还原好了。
func resolvePromptTargetsFromUI(ui map[string]any) promptTargets {
	nodes, ids := uiNodesByID(ui)
	if len(ids) == 0 {
		return promptTargets{}
	}
	origins := uiLinkOrigins(ui)

	samplerID := 0
	for _, id := range ids {
		links := uiInputLinks(nodes[id])
		if links["positive"] == 0 || links["negative"] == 0 {
			continue
		}
		classType, _ := nodes[id]["type"].(string)
		if strings.Contains(normalizeNodeType(classType), "sampler") {
			samplerID = id
			break
		}
		if samplerID == 0 {
			samplerID = id
		}
	}
	if samplerID == 0 {
		return promptTargets{}
	}

	links := uiInputLinks(nodes[samplerID])
	var positive, negative *promptTarget
	if origin, ok := origins[links["positive"]]; ok {
		positive = walkUIToTextTarget(nodes, origins, origin, rolePositive)
	}
	if origin, ok := origins[links["negative"]]; ok {
		negative = walkUIToTextTarget(nodes, origins, origin, roleNegative)
	}
	return newPromptTargets(positive, negative)
}

// ---------------------------------------------------------------------------
// 候选列表（编辑器下拉框的数据源）
// ---------------------------------------------------------------------------

// promptPreviewLimit 是候选列表里显示的工作流原文长度上限。
const promptPreviewLimit = 80

// collectPromptCandidates 列出工作流里所有「可能承载提示词」的节点，供编辑器手动选择。
//
// 同时支持 UI 导出格式与 API 格式。老格式拿不到字段名时 Field 为空，
// 但仍把节点列出来（附上控件原文），让人至少能选中节点。
func collectPromptCandidates(workflow map[string]any) []promptCandidate {
	candidates := make([]promptCandidate, 0, 4)

	if _, isUIFormat := workflow["nodes"].([]any); isUIFormat {
		nodes, ids := uiNodesByID(workflow)
		for _, id := range ids {
			node := nodes[id]
			// 前端专用节点（Note / Reroute / PrimitiveNode）与静音、旁路节点都不会进
			// 提交载荷，列进下拉只会让人选中一个写了也白写的位置。
			if nodeSkipReason(node) != "" {
				continue
			}
			classType, _ := node["type"].(string)
			named, _ := node["widgets_values_named"].(map[string]any)
			fields := promptFieldsOf(named)
			if len(fields) > 0 {
				for _, field := range fields {
					text, _ := named[field].(string)
					candidates = append(candidates, promptCandidate{
						NodeID:    strconv.Itoa(id),
						ClassType: classType,
						Field:     field,
						Preview:   truncateRunes(strings.TrimSpace(text), promptPreviewLimit),
					})
				}
				continue
			}
			// 老格式：字段名拿不到，但有控件原文可看
			if text := firstWidgetString(node["widgets_values"]); text != "" {
				candidates = append(candidates, promptCandidate{
					NodeID:    strconv.Itoa(id),
					ClassType: classType,
					Preview:   truncateRunes(text, promptPreviewLimit),
				})
			}
		}
	} else {
		for _, id := range sortedNodeIDs(workflow) {
			node, ok := workflow[id].(map[string]any)
			if !ok {
				continue
			}
			// API 格式的节点没有 type/mode，只认得出前端专用节点的类名，
			// 与 /prompt 提交载荷的剔除规则保持一致。
			if frontendOnlyNodeTypes[nodeClassType(node)] {
				continue
			}
			inputs := nodeInputs(node)
			for _, field := range promptFieldsOf(inputs) {
				text, _ := inputs[field].(string)
				candidates = append(candidates, promptCandidate{
					NodeID:    id,
					ClassType: nodeClassType(node),
					Field:     field,
					Preview:   truncateRunes(strings.TrimSpace(text), promptPreviewLimit),
				})
			}
		}
	}

	targets := resolvePromptTargetsFromUI(workflow)
	if _, isUIFormat := workflow["nodes"].([]any); !isUIFormat {
		targets = resolvePromptTargets(workflow)
	}
	for index := range candidates {
		candidate := &candidates[index]
		switch {
		case matchesTarget(targets.Positive, candidate):
			candidate.Role = "positive"
		case matchesTarget(targets.Negative, candidate) && !targets.NegativeShared:
			candidate.Role = "negative"
		}
	}
	return candidates
}

// matchesTarget 判断候选是不是就是解析出来的那个落点。
// 落点字段为空（老格式）时只比节点 id。
func matchesTarget(target *promptTarget, candidate *promptCandidate) bool {
	if target == nil || candidate == nil || target.NodeID != candidate.NodeID {
		return false
	}
	return target.Field == "" || candidate.Field == "" || target.Field == candidate.Field
}

// firstWidgetString 取控件位置数组里第一个非空字符串，用于老格式的候选项预览。
func firstWidgetString(raw any) string {
	for _, value := range asSlice(raw) {
		text, ok := value.(string)
		if ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}
