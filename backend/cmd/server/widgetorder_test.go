package main

import (
	"encoding/json"
	"testing"
)

// objectInfoInput 是一段仿真的 object_info input 段，专门覆盖各代格式的分歧点：
//   - text / seed / steps 是控件
//   - clip 是连线型，不占位置
//   - forced 声明了 forceInput，前端只建插槽，不占位置
//   - seed 带 control_after_generate，前端会在它后面挂一个下拉框（占位置）
//   - scheduler 在 optional 段，仍要按顺序排在后面
//   - hidden 段不是控件
const objectInfoInput = `{
  "required": {
    "text": ["STRING", {"multiline": true}],
    "clip": ["CLIP"],
    "seed": ["INT", {"default": 0, "control_after_generate": true}],
    "forced": ["INT", {"default": 1, "forceInput": true}],
    "steps": ["INT", {"default": 20}]
  },
  "optional": {
    "scheduler": [["normal", "karras"], {"default": "normal"}]
  },
  "hidden": {"prompt": "PROMPT"}
}`

func TestParseWidgetSlotsKeepsDefinitionOrder(t *testing.T) {
	slots, err := parseWidgetSlots([]byte(objectInfoInput))
	if err != nil {
		t.Fatalf("parseWidgetSlots 失败: %v", err)
	}

	// 期望：required 段里保序取控件（clip / forced 出局），再接 optional 段。
	want := []struct {
		name    string
		control bool
	}{
		{"text", false},
		{"seed", true},
		{"steps", false},
		{"scheduler", false},
	}
	if len(slots) != len(want) {
		t.Fatalf("控件位数量应为 %d，实际 %d（%v）", len(want), len(slots), slots)
	}
	for i, expected := range want {
		if slots[i].Name != expected.name || slots[i].Control != expected.control {
			t.Errorf("第 %d 个控件位应为 %+v，实际 %+v", i, expected, slots[i])
		}
	}
}

// forceInput 的字段不能占用 widgets_values 的位置，否则后面所有值都会错位。
func TestParseWidgetSlotsSkipsForceInput(t *testing.T) {
	slots, err := parseWidgetSlots([]byte(objectInfoInput))
	if err != nil {
		t.Fatalf("parseWidgetSlots 失败: %v", err)
	}
	for _, slot := range slots {
		if slot.Name == "forced" || slot.Name == "clip" {
			t.Fatalf("%s 不该是控件位", slot.Name)
		}
	}
}

// 名字叫 seed 但定义里没有 control_after_generate 标记时，不能补占位——
// 目标机上有 59 个这样的节点，按名字猜会让它们的值整体前移。
func TestWidgetControlSlotFollowsDefinitionFlagNotName(t *testing.T) {
	spec := `{"required":{"seed":["INT",{"default":0}]}}`
	slots, err := parseWidgetSlots([]byte(spec))
	if err != nil {
		t.Fatalf("parseWidgetSlots 失败: %v", err)
	}
	if len(slots) != 1 || slots[0].Name != "seed" || slots[0].Control {
		t.Fatalf("无标记的 seed 不该带控制位，实际 %+v", slots)
	}
}

// 位置数组带控制位那一格时，必须整格跳过，否则后面的值全部前移一格
// （真实故障表现：KSampler.steps 读到 "randomize"）。
func TestApplyPositionalWidgetsSkipsControlSlot(t *testing.T) {
	slots := []widgetSlot{
		{Name: "seed", Control: true},
		{Name: "steps"},
		{Name: "cfg"},
	}
	values := []any{float64(12345), "fixed", float64(20), 7.5}
	inputs := map[string]any{}

	if !applyPositionalWidgets(inputs, slots, values) {
		t.Fatal("应该填入控件值")
	}
	if inputs["seed"] != float64(12345) {
		t.Errorf("seed 应为 12345，实际 %#v", inputs["seed"])
	}
	if inputs["steps"] != float64(20) {
		t.Errorf("控制位没被跳过：steps 应为 20，实际 %#v", inputs["steps"])
	}
	if inputs["cfg"] != 7.5 {
		t.Errorf("cfg 应为 7.5，实际 %#v", inputs["cfg"])
	}
	if _, leaked := inputs["control_after_generate"]; leaked {
		t.Error("控制位不该进载荷")
	}
}

// 旧版导出的工作流没有控制位那一格（值比控件位少），此时按位置顺序前缀对齐，
// 多出来的控件位保持默认——和前端加载旧工作流的行为一致。
func TestApplyPositionalWidgetsPrefixAlignsShortArrays(t *testing.T) {
	slots := []widgetSlot{
		{Name: "seed", Control: true},
		{Name: "steps"},
	}
	inputs := map[string]any{}
	values := []any{float64(7), float64(30)} // 2 个值：正好等于「不含控制位」的控件数

	if !applyPositionalWidgets(inputs, slots, values) {
		t.Fatal("应该填入控件值")
	}
	if inputs["seed"] != float64(7) || inputs["steps"] != float64(30) {
		t.Fatalf("前缀对齐结果不对：%#v", inputs)
	}
}

// 值比控件位还多（节点后来删了控件）时，只填前面对应得上的，超出的值丢，
// 且绝不能 panic。
func TestApplyPositionalWidgetsIgnoresSurplusValues(t *testing.T) {
	slots := []widgetSlot{{Name: "steps"}}
	inputs := map[string]any{}
	if !applyPositionalWidgets(inputs, slots, []any{float64(20), "多余", "也多余"}) {
		t.Fatal("应该填入控件值")
	}
	if inputs["steps"] != float64(20) {
		t.Fatalf("steps 应为 20，实际 %#v", inputs["steps"])
	}
	if len(inputs) != 1 {
		t.Fatalf("多余的值不该被塞进载荷：%#v", inputs)
	}
}

// 已被连线占用的控件位不写入值，但仍占位置（否则后面全部错位）。
func TestApplyPositionalWidgetsKeepsLinkedSlotsOccupied(t *testing.T) {
	slots := []widgetSlot{{Name: "text"}, {Name: "seed", Control: true}, {Name: "steps"}}
	inputs := map[string]any{"text": []any{"9", 0}} // text 被连线占用
	values := []any{"会被忽略", "fixed", float64(25)}

	applyPositionalWidgets(inputs, slots, values)
	if link, ok := inputs["text"].([]any); !ok || len(link) != 2 {
		t.Fatalf("text 应保持连线值：%#v", inputs["text"])
	}
	if inputs["steps"] != float64(25) {
		t.Fatalf("steps 应为 25（控制位占位不能少），实际 %#v", inputs["steps"])
	}
}

// 老格式工作流（inputs 里只有连线、没有 widget 标记、也没有 widgets_values_named）
// 在拿到节点定义后必须能把控件值还原出来——这正是「在 ComfyUI 里跑得通、
// 在本系统却过不了」的那批工作流。
const legacyUIWorkflow = `{
  "nodes": [
    {"id": 1, "type": "KSampler", "mode": 0,
     "widgets_values": [12345, "fixed", 20, 7.5, "euler", "normal", 1],
     "inputs": [
       {"name": "model", "type": "MODEL", "link": 1},
       {"name": "positive", "type": "CONDITIONING", "link": 2},
       {"name": "negative", "type": "CONDITIONING", "link": 3},
       {"name": "latent_image", "type": "LATENT", "link": 4}
     ]},
    {"id": 10, "type": "UNETLoader", "mode": 0, "inputs": [], "widgets_values": ["m.safetensors", "default"]},
    {"id": 5, "type": "CLIPTextEncode", "mode": 0, "inputs": [], "widgets_values": ["a cat"]},
    {"id": 6, "type": "CLIPTextEncode", "mode": 0, "inputs": [], "widgets_values": ["blurry"]},
    {"id": 3, "type": "EmptyLatentImage", "mode": 0, "inputs": [], "widgets_values": [832, 1216, 1]}
  ],
  "links": [
    [1, 10, 0, 1, 0, "MODEL"],
    [2, 5, 0, 1, 1, "CONDITIONING"],
    [3, 6, 0, 1, 2, "CONDITIONING"],
    [4, 3, 0, 1, 3, "LATENT"]
  ]
}`

func ksamplerSlots(classType string) ([]widgetSlot, bool) {
	if classType != "KSampler" {
		return nil, false
	}
	return []widgetSlot{
		{Name: "seed", Control: true},
		{Name: "steps"},
		{Name: "cfg"},
		{Name: "sampler_name"},
		{Name: "scheduler"},
		{Name: "denoise"},
	}, true
}

func TestUIToAPIFormatRebuildsLegacyWidgetValues(t *testing.T) {
	var ui map[string]any
	if err := json.Unmarshal([]byte(legacyUIWorkflow), &ui); err != nil {
		t.Fatalf("测试夹具不是合法 JSON: %v", err)
	}

	api, err := uiToAPIFormatWithOrder(ui, ksamplerSlots)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	raw, ok := api["1"].(map[string]any)
	if !ok {
		t.Fatalf("载荷里没有节点 1：%#v", api["1"])
	}
	inputs, _ := raw["inputs"].(map[string]any)

	for field, want := range map[string]any{
		"seed":         float64(12345),
		"steps":        float64(20),
		"cfg":          7.5,
		"sampler_name": "euler",
		"scheduler":    "normal",
		"denoise":      float64(1),
	} {
		if inputs[field] != want {
			t.Errorf("KSampler.%s 应为 %#v，实际 %#v", field, want, inputs[field])
		}
	}
	if _, leaked := inputs["control_after_generate"]; leaked {
		t.Error("control_after_generate 不该进载荷")
	}
	// 连线优先于控件值。
	if link, ok := inputs["model"].([]any); !ok || len(link) != 2 || link[0] != "10" {
		t.Errorf("model 连线被破坏：%#v", inputs["model"])
	}
}

// 没有节点定义时，老格式的控件值无从还原——这正是必须把定义喂进来的原因。
// 测试固定这一边界，避免以后有人把 order 参数当成可选的死代码删掉。
func TestUIToAPIFormatWithoutNodeDefsLeavesLegacyValuesUnread(t *testing.T) {
	var ui map[string]any
	if err := json.Unmarshal([]byte(legacyUIWorkflow), &ui); err != nil {
		t.Fatalf("测试夹具不是合法 JSON: %v", err)
	}

	api, err := uiToAPIFormatWithOrder(ui, nil)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	raw, _ := api["1"].(map[string]any)
	inputs, _ := raw["inputs"].(map[string]any)
	if _, exists := inputs["steps"]; exists {
		t.Fatalf("没有节点定义时不该凭空还原出控件值：%#v", inputs)
	}
}

// 「位置数组长度和节点定义对不上」要报出来，否则用户只会看到值对不上却不知原因。
func TestCollectWidgetValueMismatchesReportsStaleExport(t *testing.T) {
	// 夹具里 UNETLoader 有 3 个值的槽位定义，但文件只给了 1 个值（旧版导出）。
	defs := map[string]nodeInputDef{
		"UNETLoader": {
			Spec: map[string]map[string]any{"required": {"unet_name": nil, "weight_dtype": nil, "unused": nil}},
			Slots: []widgetSlot{
				{Name: "unet_name"},
				{Name: "weight_dtype"},
				{Name: "unused"},
			},
		},
	}
	issues := collectWidgetValueMismatches([]byte(legacyUIWorkflow), defs)
	if len(issues) != 1 {
		t.Fatalf("应报出 1 条不匹配，实际 %d 条：%+v", len(issues), issues)
	}
	if issues[0].NodeID != "10" || issues[0].Kind != "widget_values_mismatch" {
		t.Fatalf("报错信息不对：%+v", issues[0])
	}
}
