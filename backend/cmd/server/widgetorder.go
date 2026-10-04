package main

// ============================================================================
// 老格式工作流的控件值还原
//
// ComfyUI 的工作流导出格式至少有三代，widget 值的存放方式各不相同：
//
//	A. 新（frontendVersion >= 1.4x）：widgets_values_named 里有「名字 → 值」，
//	   inputs 里同时保留带 widget 标记的条目。名字是权威的，直接读。
//	B. 中（1.4x 之前一档）：没有 widgets_values_named，但 inputs 里挂着 widget
//	   标记，标记顺序 == widgets_values 的位置顺序。按标记顺序吃位置数组即可。
//	C. 老（早期导出/工具生成）：inputs 里只有真正的连线输入，widget 一个都不标，
//	   值全在裸位置数组 widgets_values 里。**光看文件无法知道哪个位置对应哪个
//	   控件**，必须借助目标 ComfyUI 的节点定义（object_info）还原顺序。
//
// 过去只实现了 A 和 B。C 类文件（目标机上那批 Pony / Z-Image 工作流就是）会在
// 这里被整份误判——所有控件值都读不出来，于是校验报出一堆「缺少必填输入」，
// 而它们在 ComfyUI 里明明跑得通。
//
// 顺序规则照抄前端（见 1.52.7 的 migrateWidgetsValues / addValueControlWidgets）：
//
//	按节点定义 required → optional 的键顺序遍历；
//	类型是标量/下拉（INT/FLOAT/STRING/BOOLEAN/COMBO）且未声明 forceInput 的才算控件；
//	控件若带 control_after_generate: true，前端会在它后面挂一个下拉框，
//	那个下拉框不参与 /prompt，但在 widgets_values 里占一格。
//
// 最后一条是关键：漏掉这格占位，它后面的值会整体前移（实测 KSampler.steps
// 会读到 'randomize'）。反过来，**不能**按名字猜——目标机上有 59 个节点叫 seed
// 却没有这个控件，按名字补占位会把它们的值全部错位。
// ============================================================================

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// widgetSlot 是节点定义里的一个可编辑控件位。
type widgetSlot struct {
	// Name 为空表示这是一个「控制位」占位（control_after_generate），值不参与载荷。
	Name    string
	Control bool
}

// controlSlot 是 control_after_generate 在 widgets_values 里占的那一格。
var controlSlot = widgetSlot{Control: true}

// widgetOrderLookup 按 class_type 返回有序控件位；第二个返回值为 false 表示拿不到定义。
type widgetOrderLookup func(classType string) ([]widgetSlot, bool)

// widgetInputTypes 是会被前端渲染成控件的输入类型；其余（MODEL/IMAGE/LATENT/...）是连线型。
var widgetInputTypes = map[string]bool{
	"INT": true, "FLOAT": true, "STRING": true, "BOOLEAN": true, "COMBO": true,
}

// specOptions 取出字段声明的选项对象（声明形如 ["INT", {...}]）。
func specOptions(spec any) map[string]any {
	arr, ok := spec.([]any)
	if !ok || len(arr) < 2 {
		return nil
	}
	options, _ := arr[1].(map[string]any)
	return options
}

// isWidgetInput 判断一个输入声明会不会被前端渲染成控件（即是否占 widgets_values 的一格）。
//
// 判据与前端 registration 一致：类型是标量或下拉，且没有声明 forceInput
// （forceInput 表示这个输入只能靠连线，前端会建插槽而不是控件）。
func isWidgetInput(spec any) bool {
	arr, ok := spec.([]any)
	if !ok || len(arr) == 0 {
		return false
	}
	if forced, _ := specOptions(spec)["forceInput"].(bool); forced {
		return false
	}
	switch head := arr[0].(type) {
	case []any:
		return true // 旧式下拉：候选值直接摆在第一位
	case map[string]any:
		return head["type"] == "COMBO"
	case string:
		return widgetInputTypes[head]
	}
	return false
}

// hasControlAfterGenerate 判断该控件后面是否跟着一个 control_after_generate 下拉框。
// 必须看定义里的显式标记，不能按名字（seed/noise_seed）猜——见文件头说明。
func hasControlAfterGenerate(spec any) bool {
	flag, _ := specOptions(spec)["control_after_generate"].(bool)
	return flag
}

// parseWidgetSlots 从 object_info 的 input 段还原控件顺序。
//
// 必须保序，所以这里用流式解码逐个读键，而不是先用 map[string]any 解析
// ——后者会丢键顺序，而位置映射完全依赖顺序。
func parseWidgetSlots(inputJSON []byte) ([]widgetSlot, error) {
	decoder := json.NewDecoder(bytes.NewReader(inputJSON))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("input section is not a JSON object")
	}

	slots := make([]widgetSlot, 0, 8)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		section, _ := keyToken.(string)
		if section != "required" && section != "optional" {
			// hidden 之类：里面全是非控件声明，整段跳过。
			var discard json.RawMessage
			if err := decoder.Decode(&discard); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := decoder.Token(); err != nil { // '{'
			return nil, err
		}
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, _ := nameToken.(string)
			var spec any
			if err := decoder.Decode(&spec); err != nil {
				return nil, err
			}
			if !isWidgetInput(spec) {
				continue
			}
			slots = append(slots, widgetSlot{Name: name, Control: hasControlAfterGenerate(spec)})
		}
		if _, err := decoder.Token(); err != nil { // '}'
			return nil, err
		}
	}
	return slots, nil
}

// expandWidgetSlots 把「控件位」展开成「位置数组的格子」：
// 带控制标记的控件后面补一格占位。
func expandWidgetSlots(slots []widgetSlot) []widgetSlot {
	expanded := make([]widgetSlot, 0, len(slots))
	for _, slot := range slots {
		expanded = append(expanded, slot)
		if slot.Control {
			expanded = append(expanded, controlSlot)
		}
	}
	return expanded
}

// applyPositionalWidgets 用节点定义把裸位置数组还原成「名字 → 值」。
//
// 取值规则与前端加载旧工作流时一致：按位置顺序填，数组短于控件位时多出来的
// 控件位保持默认（前端就是这么做前缀对齐的），长于控件位时超出的值丢弃。
// 返回是否真的填入了值，供调用方判断要不要退回别的读取路径。
func applyPositionalWidgets(inputs map[string]any, slots []widgetSlot, values []any) bool {
	if len(slots) == 0 || len(values) == 0 {
		return false
	}
	expanded := expandWidgetSlots(slots)
	// 老导出可能还没有控制位（位置数组里没那一格）。只有长度恰好对上时才这么解读，
	// 否则说明是「旧版本节点定义 + 现在的值」，按含控制位前缀对齐更贴近前端行为。
	if len(values) != len(expanded) {
		plain := make([]widgetSlot, 0, len(slots))
		for _, slot := range slots {
			plain = append(plain, widgetSlot{Name: slot.Name})
		}
		if len(values) == len(plain) {
			expanded = plain
		}
	}

	filled := false
	for index, slot := range expanded {
		if index >= len(values) {
			break
		}
		if slot.Name == "" { // 控制位占位，值不进载荷
			continue
		}
		if _, occupied := inputs[slot.Name]; occupied {
			continue // 已被连线或 widgets_values_named 占用，手动值不再生效
		}
		inputs[slot.Name] = expandDateTemplate(values[index])
		filled = true
	}
	return filled
}

// hasWidgetMarkers 判断节点的 inputs 里有没有带 widget 标记的条目。
// 有标记说明这份导出自己带着控件顺序，不需要去问 ComfyUI。
func hasWidgetMarkers(node map[string]any) bool {
	for _, raw := range asSlice(node["inputs"]) {
		def, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, marked := def["widget"]; marked {
			return true
		}
	}
	return false
}

// applyMarkedWidgets 按 inputs 里 widget 标记的顺序吃 widgets_values。
// 这是「中代」导出格式的读法：标记顺序就是位置数组的顺序，文件自带答案。
func applyMarkedWidgets(inputs map[string]any, node map[string]any, values []any) {
	widgetIdx := 0
	for _, raw := range asSlice(node["inputs"]) {
		def, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, marked := def["widget"]; !marked {
			continue
		}
		if widgetIdx >= len(values) {
			break
		}
		name, _ := def["name"].(string)
		value := values[widgetIdx]
		// 位置数组与标记一一对应：即使这一格已被连线占用（值不生效），
		// 下标也必须照常前进，否则后面所有控件都会错位。
		widgetIdx++
		if _, occupied := inputs[name]; occupied {
			continue
		}
		inputs[name] = expandDateTemplate(value)
	}
}

// widgetSlotCacheTTL 是节点定义的缓存时长。节点定义只随 ComfyUI 安装变化，
// 缓存久一点没关系；取它纯粹是为了老格式工作流，新格式一次网络都不用。
const widgetSlotCacheTTL = 10 * time.Minute

type widgetSlotCacheEntry struct {
	def nodeInputDef
	at  time.Time
}

var (
	widgetSlotCacheMu sync.Mutex
	widgetSlotCache   = map[string]widgetSlotCacheEntry{}
)

// lookupNodeInputDef 查询（带缓存）某节点类型的完整定义：字段声明 + 控件顺序。
//
// 缓存的是整份定义而不是只有控件顺序：一次 object_info 请求本来就同时给出这两样，
// 校验要字段声明、还原老格式要控件顺序、自动纠正（repair.go）两样都要，
// 分成两份缓存只会把同一个请求发三遍。
//
// 只缓存成功结果：失败可能是 ComfyUI 临时不可达，缓存下来会把恢复后的请求也挡掉。
func lookupNodeInputDef(ctx context.Context, baseURL, classType string) (nodeInputDef, bool) {
	key := baseURL + "\x00" + classType

	widgetSlotCacheMu.Lock()
	if entry, ok := widgetSlotCache[key]; ok && time.Since(entry.at) < widgetSlotCacheTTL {
		widgetSlotCacheMu.Unlock()
		return entry.def, true
	}
	widgetSlotCacheMu.Unlock()

	def, err := fetchNodeInputDef(ctx, baseURL, classType)
	if err != nil {
		// 拿不到定义会让老格式（C 类）工作流静默降级——还原不出任何控件值，
		// 最终只报出「所有必填都缺失」这种看不出因果的错误。现场必须留痕：
		// 到底是节点类型不存在（404）、还是 ComfyUI 连不上（超时）。
		debugEvent("node_def_missing", map[string]any{
			"class_type": classType,
			"url":        baseURL,
			"error":      err.Error(),
		})
		return nodeInputDef{}, false
	}

	widgetSlotCacheMu.Lock()
	// 上限兜底：这条缓存按 URL+类型增长，正常只会有一个 ComfyUI 地址，不该无限涨。
	if len(widgetSlotCache) > 4096 {
		widgetSlotCache = map[string]widgetSlotCacheEntry{}
	}
	widgetSlotCache[key] = widgetSlotCacheEntry{def: def, at: time.Now()}
	widgetSlotCacheMu.Unlock()
	return def, true
}

// lookupWidgetSlots 查询（带缓存）某节点类型的控件顺序。
func lookupWidgetSlots(ctx context.Context, baseURL, classType string) ([]widgetSlot, bool) {
	def, ok := lookupNodeInputDef(ctx, baseURL, classType)
	if !ok {
		return nil, false
	}
	return def.Slots, true
}

// newWidgetSlotLookup 造一个绑定到某个 ComfyUI 地址的控件顺序查询器。
// 提交路径（submitItem）用它；校验接口已经有节点定义在手，直接复用那份结果。
func newWidgetSlotLookup(baseURL string) widgetOrderLookup {
	return func(classType string) ([]widgetSlot, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return lookupWidgetSlots(ctx, baseURL, classType)
	}
}

// extractInputSection 从节点定义里抠出 input 段的原始 JSON（保持键顺序）。
func extractInputSection(nodeJSON json.RawMessage) (json.RawMessage, error) {
	var node struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(nodeJSON, &node); err != nil {
		return nil, err
	}
	return node.Input, nil
}
