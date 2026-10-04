package main

// ============================================================================
// 提交被拒后的自动纠正：把「串位的控件值」挪回它真正属于的输入
//
// 背景：ComfyUI 把工作流导出成 JSON 时，控件值是按「位置数组 + 名字映射」两份
// 一起写出的。节点定义在版本之间加了控件、或这份 JSON 是由脚本/更早的版本拼出来
// 的时候，值会写到隔壁字段上。这种文件在 ComfyUI 里点运行可能是好的（前端按控件
// 对象取值），导出的 JSON 却过不了 /prompt 的校验：
//
//     node 8 (CLIPLoader): clip_name: 'krea2' not in [ ... ]
//
// 这里做的是最保守的一种补救，而且**只在 ComfyUI 明确报「取值不在列表」时才动手**：
//
//	如果这个错位的值，恰好是同一个节点另一个输入的合法取值 → 把它挪过去，
//	腾空的字段用这个字段自己的默认值补上，然后重试一次。
//
// 例：clip_name 拿到了 "krea2"（那是 type 的合法取值），而 type 停在默认值上。
// 挪回去之后 clip_name 用默认值补上——正好就是本来的文本编码器。
//
// 明确不做的事：
//   - 值本身已经没有归属（模型被删、选项下线）时一个字都不改。那种情况没有正确
//     答案，静默换成别的模型会让人以为跑的是自己选的东西，不如把 ComfyUI 的原话
//     原样报出来。
//   - 只影响这一次提交的载荷。落盘的工作流文件（data/workflows/*.json）不被改写。
//   - 只重试一次。纠正本身是确定性的一步，再失败说明不是串位问题。
// ============================================================================

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// payloadRepair 是一处「值串位」的纠正记录。
// 存在的意义是让用户看得见自动做了什么——自动改值必须可追溯，否则比报错更糟。
type payloadRepair struct {
	NodeID    string
	ClassType string
	Field     string // 值原本落在（错的）字段
	Value     string // 被挪动的值
	Target    string // 值真正属于的字段
	Replaced  string // Target 上原有的值（为空表示原本没有这个输入）
	Filled    string // 腾空的 Field 补上的默认值
}

// String 渲染成一句人类可读的说明，进日志、进任务备注、进校验提示都用它。
func (p payloadRepair) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "node %s (%s)：%s 的 %q 不在候选里，但 %s 接受它 → 挪到 %s",
		p.NodeID, p.ClassType, p.Field, p.Value, p.Target, p.Target)
	if p.Replaced != "" && p.Replaced != p.Value {
		fmt.Fprintf(&b, "（原值 %q）", p.Replaced)
	}
	fmt.Fprintf(&b, "，%s 用默认值 %q 补上", p.Field, p.Filled)
	return b.String()
}

// repairDetails 把纠正记录转成可直接落盘的 map 列表（DEBUG 的 events.jsonl 用）。
// String() 是给人读的一句话，这里要的是能逐字段比对的结构——将来出现
// 「纠正错了」的时候，需要的正是「改前是什么、改成了什么」。
func repairDetails(repairs []payloadRepair) []map[string]any {
	out := make([]map[string]any, 0, len(repairs))
	for _, repair := range repairs {
		out = append(out, map[string]any{
			"node_id":    repair.NodeID,
			"class_type": repair.ClassType,
			"field":      repair.Field,
			"value":      repair.Value,
			"moved_to":   repair.Target,
			"replaced":   repair.Replaced,
			"filled":     repair.Filled,
		})
	}
	return out
}

// planNodeRepairs 只算不改：返回这个节点上可以安全施行的纠正，inputs 不被改动。
// 调用方要用试算结果（比如校验接口的提示）就传副本，要真改就自己应用。
//
// offending 是「取值不在候选列表里」的字段名（来自 ComfyUI 的 node_errors，
// 或本地校验算出来的同一件事）。
func planNodeRepairs(nodeID, classType string, inputs map[string]any, offending []string, spec map[string]map[string]any) []payloadRepair {
	if len(offending) == 0 || len(inputs) == 0 || len(spec) == 0 {
		return nil
	}
	required, optional := splitInputSpec(spec)

	// 排序后处理：同一份载荷每次都要得到同样的纠正，否则日志里同样是错位却
	// 每次改的字段不一样，没法对照。
	fields := append([]string(nil), offending...)
	sort.Strings(fields)

	claimed := map[string]bool{}
	for _, field := range fields {
		claimed[field] = true
	}

	repairs := make([]payloadRepair, 0, len(fields))
	for _, field := range fields {
		value, ok := inputs[field].(string)
		if !ok || value == "" {
			continue // 非字符串（数字/连线）不猜
		}
		target, unique := uniqueFieldAccepting(inputs, required, optional, field, value, claimed)
		if !unique {
			continue // 没有归属，或者有多个字段都接受 —— 都不动
		}
		filled, ok := defaultOption(declaredSpec(required, optional, field))
		if !ok || filled == value {
			continue
		}
		replaced := ""
		if existing, ok := inputs[target].(string); ok {
			replaced = existing
		}
		claimed[target] = true
		repairs = append(repairs, payloadRepair{
			NodeID:    nodeID,
			ClassType: classType,
			Field:     field,
			Value:     value,
			Target:    target,
			Replaced:  replaced,
			Filled:    filled,
		})
	}
	return repairs
}

// uniqueFieldAccepting 找出同一个节点里「恰好接受这个值」的另一个字段。
//
// 必须唯一：多个字段都接受说明这是歧义（比如两个字段共用一份候选），
// 硬选一个等于替用户猜。claim 里是已被占用的字段（本轮已处理过的 + 目标字段），
// 避免两个错位值抢同一个落点。
func uniqueFieldAccepting(inputs map[string]any, required, optional map[string]any, current, value string, claimed map[string]bool) (string, bool) {
	found := ""
	for _, name := range declaredFields(required, optional) {
		if name == current || claimed[name] {
			continue
		}
		if isLinkValue(inputs[name]) {
			continue // 这个输入由连线供电，写进去也不会生效
		}
		if !containsString(comboOptions(declaredSpec(required, optional, name)), value) {
			continue
		}
		if found != "" {
			return "", false
		}
		found = name
	}
	return found, found != ""
}

// declaredFields 返回节点定义里所有字段名（required + optional），已排序。
func declaredFields(required, optional map[string]any) []string {
	seen := make(map[string]bool, len(required)+len(optional))
	out := make([]string, 0, len(required)+len(optional))
	for _, source := range []map[string]any{required, optional} {
		for name := range source {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// declaredSpec 取某个字段的声明，required 优先（同名时 required 才是生效的那个）。
func declaredSpec(required, optional map[string]any, field string) any {
	if spec, ok := required[field]; ok {
		return spec
	}
	return optional[field]
}

// defaultOption 取字段声明的默认值：显式 default 优先，否则用候选列表第一项
// ——ComfyUI 对没写 default 的下拉就是这么取的（新拖出来的节点显示的就是第一项）。
func defaultOption(spec any) (string, bool) {
	if value, ok := specOptions(spec)["default"].(string); ok && value != "" {
		return value, true
	}
	if options := comboOptions(spec); len(options) > 0 {
		return options[0], true
	}
	return "", false
}

// planRepairs 读 ComfyUI 的拒绝原因，算出纠正**并写进 prompt**（这是提交前的载荷
// 副本，不是工作流文件）。按节点 id 顺序处理，结果稳定可复现。
//
// lookup 取节点定义；拿不到定义的节点直接跳过——没有候选列表就无从判断该挪去哪。
func planRepairs(prompt map[string]any, rejection comfyRejection, lookup func(classType string) (nodeInputDef, bool)) []payloadRepair {
	repairs := make([]payloadRepair, 0, 1)
	for _, nodeID := range sortedNodeErrorIDs(rejection) {
		nodeErr := rejection.NodeErrors[nodeID]
		node, ok := prompt[nodeID].(map[string]any)
		if !ok {
			continue
		}
		inputs, ok := node["inputs"].(map[string]any)
		if !ok {
			continue
		}
		classType := strings.TrimSpace(nodeErr.ClassType)
		if classType == "" {
			classType, _ = node["class_type"].(string)
		}
		def, ok := lookup(classType)
		if !ok || def.Spec == nil {
			continue
		}
		offending := make([]string, 0, len(nodeErr.Errors))
		for _, detail := range nodeErr.Errors {
			if detail.Type != "value_not_in_list" {
				continue
			}
			if field := offendingField(detail); field != "" {
				offending = append(offending, field)
			}
		}
		nodeRepairs := planNodeRepairs(nodeID, classType, inputs, offending, def.Spec)
		applyRepairs(inputs, nodeRepairs)
		repairs = append(repairs, nodeRepairs...)
	}
	return repairs
}

// applyRepairs 把纠正写进载荷：值挪到落点，腾空的字段补默认值。
// 两个值都过 expandDateTemplate —— 写进节点的新值必须和别的入口一样展开日期模板，
// 否则 Windows 上目录名带 ':' 会让 ComfyUI 侧保存失败（详见 comfyui.go）。
func applyRepairs(inputs map[string]any, repairs []payloadRepair) {
	for _, repair := range repairs {
		inputs[repair.Target] = expandDateTemplate(repair.Value)
		inputs[repair.Field] = expandDateTemplate(repair.Filled)
	}
}

// cloneInputs 复制一层输入表，供「只算不改」的试算使用
// （校验接口要拿纠正结果当提示，但不能真的改动校验用的载荷）。
func cloneInputs(inputs map[string]any) map[string]any {
	out := make(map[string]any, len(inputs))
	for name, value := range inputs {
		out[name] = value
	}
	return out
}

// offendingField 从一个「取值不在列表」的错误里取出字段名。
//
// ComfyUI 新版把字段名放在 extra_info.input_name；老版只能从 details 的前缀抠
// ——形如 `clip_name: 'krea2' not in [...]`。抠不出来就返回空，宁可不修。
func offendingField(detail comfyErrorDetail) string {
	if name := strings.TrimSpace(detail.ExtraInfo.InputName); name != "" {
		return name
	}
	head, _, found := strings.Cut(strings.TrimSpace(detail.Details), ":")
	if !found {
		return ""
	}
	name := strings.TrimSpace(head)
	// 字段名只能是标识符。带空格/引号/括号说明这行不是「字段名: 值」的形态，
	// 硬切出来的东西写回载荷只会更糟。
	if name == "" || strings.ContainsAny(name, " '\"[](){}") {
		return ""
	}
	return name
}

// sortedNodeErrorIDs 按数值顺序返回出错的节点 id（非数值的排在后面），
// 保证同一份载荷每次得到同样的处理顺序。
func sortedNodeErrorIDs(rejection comfyRejection) []string {
	numeric := make([]int, 0, len(rejection.NodeErrors))
	others := make([]string, 0, len(rejection.NodeErrors))
	for rawID := range rejection.NodeErrors {
		if id, err := strconv.Atoi(rawID); err == nil {
			numeric = append(numeric, id)
		} else {
			others = append(others, rawID)
		}
	}
	sort.Ints(numeric)
	sort.Strings(others)
	out := make([]string, 0, len(numeric)+len(others))
	for _, id := range numeric {
		out = append(out, strconv.Itoa(id))
	}
	return append(out, others...)
}

// newRepairLookup 造一个绑定到某个 ComfyUI 地址的节点定义查询器（带缓存）。
// 缓存和控件顺序共用一个（见 lookupNodeInputDef）——同一个 object_info 请求
// 同时给出字段声明和控件顺序，分两份缓存只会把同一个请求发两遍。
func newRepairLookup(baseURL string) func(classType string) (nodeInputDef, bool) {
	return func(classType string) (nodeInputDef, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return lookupNodeInputDef(ctx, baseURL, classType)
	}
}
