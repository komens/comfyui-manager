package main

import (
	"encoding/json"
	"errors"
	"testing"
)

// ComfyUI 老前端（约 1.49.x）导出的 UI 格式：
// widget 节点在 inputs 里带一个 widget 标记条目，widgets_values 按顺序对应。
const legacyUIFixture = `{
  "nodes": [
    {"id": 1, "type": "UNETLoader",
     "inputs": [
       {"name": "unet_name", "type": "COMBO", "widget": {"name": "unet_name"}, "link": null},
       {"name": "weight_dtype", "type": "COMBO", "widget": {"name": "weight_dtype"}, "link": null}],
     "widgets_values": ["z-image-turbo-fp8.safetensors", "default"],
     "widgets_values_named": {"unet_name": "z-image-turbo-fp8.safetensors", "weight_dtype": "default"}},
    {"id": 2, "type": "KSampler",
     "inputs": [
       {"name": "seed", "type": "INT", "widget": {"name": "seed"}, "link": null},
       {"name": "steps", "type": "INT", "widget": {"name": "steps"}, "link": null},
       {"name": "cfg", "type": "FLOAT", "widget": {"name": "cfg"}, "link": null},
       {"name": "sampler_name", "type": "COMBO", "widget": {"name": "sampler_name"}, "link": null},
       {"name": "scheduler", "type": "COMBO", "widget": {"name": "scheduler"}, "link": null},
       {"name": "denoise", "type": "FLOAT", "widget": {"name": "denoise"}, "link": null},
       {"name": "model", "type": "MODEL", "link": 1}],
     "widgets_values": [530119756597005, "randomize", 8, 1, "euler", "simple", 1],
     "widgets_values_named": {"seed": 530119756597005, "control_after_generate": "randomize",
       "steps": 8, "cfg": 1, "sampler_name": "euler", "scheduler": "simple", "denoise": 1}},
    {"id": 3, "type": "EmptyLatentImage",
     "inputs": [
       {"name": "width", "type": "INT", "widget": {"name": "width"}, "link": null},
       {"name": "height", "type": "INT", "widget": {"name": "height"}, "link": null},
       {"name": "batch_size", "type": "INT", "widget": {"name": "batch_size"}, "link": null}],
     "widgets_values": [1280, 1280, 1],
     "widgets_values_named": {"width": 1280, "height": 1280, "batch_size": 1}}
  ],
  "links": [[1, 1, 0, 2, 6, "MODEL"]]
}`

// ComfyUI 新前端（1.52.x）导出的 UI 格式：
// inputs 里只剩连线输入，widget 标记条目整体消失，
// widget 值只保留在 widgets_values_named。
const modernUIFixture = `{
  "nodes": [
    {"id": 1, "type": "UNETLoader", "inputs": [],
     "widgets_values": ["anima-aesthetic-v1.1-int8convrot.safetensors", "default"],
     "widgets_values_named": {"unet_name": "anima-aesthetic-v1.1-int8convrot.safetensors", "weight_dtype": "default"}},
    {"id": 2, "type": "CLIPLoader", "inputs": [],
     "widgets_values": ["qwen3_0.6b_fp16.safetensors", "stable_diffusion", "default"],
     "widgets_values_named": {"clip_name": "qwen3_0.6b_fp16.safetensors", "type": "stable_diffusion", "device": "default"}},
    {"id": 3, "type": "CLIPTextEncode",
     "inputs": [{"name": "clip", "type": "CLIP", "link": 1}],
     "widgets_values": ["a red cube"],
     "widgets_values_named": {"text": "a red cube"}},
    {"id": 4, "type": "EmptyLatentImage", "inputs": [],
     "widgets_values": [960, 1280, 1],
     "widgets_values_named": {"width": 960, "height": 1280, "batch_size": 1}},
    {"id": 5, "type": "KSampler",
     "inputs": [
       {"name": "model", "type": "MODEL", "link": 2},
       {"name": "positive", "type": "CONDITIONING", "link": 3}],
     "widgets_values": [216107599362044, "randomize", 40, 4.5, "er_sde", "simple", 1],
     "widgets_values_named": {"seed": 216107599362044, "control_after_generate": "randomize",
       "steps": 40, "cfg": 4.5, "sampler_name": "er_sde", "scheduler": "simple", "denoise": 1}},
    {"id": 6, "type": "SaveImage",
     "inputs": [{"name": "images", "type": "IMAGE", "link": 4}],
     "widgets_values": ["anima/%date:yyyy-MM-dd%/%date:hhmmss%"],
     "widgets_values_named": {"filename_prefix": "anima/%date:yyyy-MM-dd%/%date:hhmmss%"}}
  ],
  "links": [[1, 2, 0, 3, 0, "CLIP"], [2, 1, 0, 5, 0, "MODEL"], [3, 3, 0, 5, 1, "CONDITIONING"], [4, 5, 0, 6, 0, "IMAGE"]]
}`

func convertFixture(t *testing.T, raw string) map[string]any {
	t.Helper()
	var ui map[string]any
	if err := json.Unmarshal([]byte(raw), &ui); err != nil {
		t.Fatalf("fixture 解析失败: %v", err)
	}
	api, err := uiToAPIFormat(ui)
	if err != nil {
		t.Fatalf("uiToAPIFormat 失败: %v", err)
	}
	return api
}

func inputsOf(t *testing.T, api map[string]any, nodeID string) map[string]any {
	t.Helper()
	node, ok := api[nodeID].(map[string]any)
	if !ok {
		t.Fatalf("节点 %s 不存在于转换结果", nodeID)
	}
	inputs, _ := node["inputs"].(map[string]any)
	if inputs == nil {
		t.Fatalf("节点 %s 没有 inputs", nodeID)
	}
	return inputs
}

func assertHas(t *testing.T, inputs map[string]any, nodeID string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := inputs[k]; !ok {
			t.Errorf("节点 %s 缺少 widget 输入 %q（会被 ComfyUI 判为 required_input_missing → HTTP 400）", nodeID, k)
		}
	}
}

// 新格式：widget 值必须全部来自 widgets_values_named，
// 否则载荷缺必填输入，ComfyUI 返回 400 prompt_outputs_failed_validation。
func TestUIToAPIFormatModernExport(t *testing.T) {
	api := convertFixture(t, modernUIFixture)

	assertHas(t, inputsOf(t, api, "1"), "1", "unet_name", "weight_dtype")
	assertHas(t, inputsOf(t, api, "2"), "2", "clip_name", "type")
	assertHas(t, inputsOf(t, api, "3"), "3", "text")
	assertHas(t, inputsOf(t, api, "4"), "4", "width", "height", "batch_size")
	assertHas(t, inputsOf(t, api, "5"), "5", "seed", "steps", "cfg", "sampler_name", "scheduler", "denoise")
	assertHas(t, inputsOf(t, api, "6"), "6", "filename_prefix")

	ks := inputsOf(t, api, "5")
	if _, bad := ks["control_after_generate"]; bad {
		t.Error("control_after_generate 是前端控件，不该注入提交载荷")
	}
	if ks["steps"] != float64(40) || ks["cfg"] != float64(4.5) {
		t.Errorf("steps/cfg 取值错误: %#v %#v", ks["steps"], ks["cfg"])
	}
	// 连线不能被同名的 widget 值覆盖
	if link, ok := ks["positive"].([]any); !ok || link[0] != "3" {
		t.Errorf("KSampler.positive 连线被破坏: %#v", ks["positive"])
	}
	// filename_prefix 的 %date% 模板必须展开，否则 ComfyUI 侧目录名非法
	prefix, _ := inputsOf(t, api, "6")["filename_prefix"].(string)
	if prefix == "" || prefix == "anima/%date:yyyy-MM-dd%/%date:hhmmss%" {
		t.Errorf("%%date%% 模板未展开: %q", prefix)
	}
	t.Logf("新格式转换结果 filename_prefix=%q", prefix)
}

// 老格式：inputs 带 widget 标记，转换结果必须与修复前一致（无回归）。
func TestUIToAPIFormatLegacyExport(t *testing.T) {
	api := convertFixture(t, legacyUIFixture)

	assertHas(t, inputsOf(t, api, "1"), "1", "unet_name", "weight_dtype")
	assertHas(t, inputsOf(t, api, "2"), "2", "seed", "steps", "cfg", "sampler_name", "scheduler", "denoise")
	assertHas(t, inputsOf(t, api, "3"), "3", "width", "height", "batch_size")

	ks := inputsOf(t, api, "2")
	if _, bad := ks["control_after_generate"]; bad {
		t.Error("control_after_generate 不该注入提交载荷")
	}
	if ks["seed"] != float64(530119756597005) || ks["steps"] != float64(8) || ks["cfg"] != float64(1) {
		t.Errorf("老格式 seed/steps/cfg 取值错误: %#v %#v %#v", ks["seed"], ks["steps"], ks["cfg"])
	}
	if ks["sampler_name"] != "euler" || ks["scheduler"] != "simple" {
		t.Errorf("老格式 sampler/scheduler 取值错误: %#v %#v", ks["sampler_name"], ks["scheduler"])
	}
	// scheduler/denoise 不能被 widgets_values 里的 control_after_generate 挤位错读
	if ks["denoise"] != float64(1) {
		t.Errorf("denoise 取值错误（widgets_values 顺序映射错位）: %#v", ks["denoise"])
	}
	if link, ok := ks["model"].([]any); !ok || link[0] != "1" {
		t.Errorf("KSampler.model 连线被破坏: %#v", ks["model"])
	}
}

// ============================================================================
// 与 ComfyUI 前端 graphToPrompt 对齐的三条规则
// ============================================================================

// 前端专用节点（Note / MarkdownNote / Reroute / PrimitiveNode）服务端没有这些类型，
// 前端会在生成载荷时把它们剔除。照原样提交必得
// 400 missing_node_type: Node 'MarkdownNote' not found —— 这正是「工作流在 ComfyUI 里
// 跑得好好的，在本系统过不了」的头号成因。
const frontendOnlyNodeFixture = `{
  "nodes": [
    {"id": 1, "type": "CLIPLoader", "inputs": [],
     "widgets_values": ["qwen3VL4BAbliteratedComfyui_v10_full_fp8.safetensors", "krea2", "default"],
     "widgets_values_named": {"clip_name": "qwen3VL4BAbliteratedComfyui_v10_full_fp8.safetensors", "type": "krea2", "device": "default"}},
    {"id": 2, "type": "CLIPTextEncode",
     "inputs": [{"name": "clip", "type": "CLIP", "link": 1}],
     "widgets_values": ["hello"], "widgets_values_named": {"text": "hello"}},
    {"id": 3, "type": "KSampler",
     "inputs": [{"name": "positive", "type": "CONDITIONING", "link": 2}],
     "widgets_values": [1, "randomize", 8, 1, "euler", "simple", 1],
     "widgets_values_named": {"seed": 1, "control_after_generate": "randomize", "steps": 8,
       "cfg": 1, "sampler_name": "euler", "scheduler": "simple", "denoise": 1}},
    {"id": 11, "type": "MarkdownNote", "inputs": [],
     "widgets_values": ["# 说明\n- 8 步草稿"], "widgets_values_named": {"text": "# 说明"}},
    {"id": 12, "type": "Note", "inputs": [], "widgets_values": ["随手记一笔"]}
  ],
  "links": [[1, 1, 0, 2, 0, "CLIP"], [2, 2, 0, 3, 0, "CONDITIONING"]]
}`

func TestUIToAPIFormatSkipsFrontendOnlyNodes(t *testing.T) {
	api := convertFixture(t, frontendOnlyNodeFixture)

	for _, bad := range []string{"11", "12"} {
		if _, exists := api[bad]; exists {
			t.Errorf("前端专用节点 %s 不该进载荷：服务端没有这个类型，提交必被 400 missing_node_type 拒掉", bad)
		}
	}
	// 其余节点必须原样保留，别把好节点一起误删
	for _, good := range []string{"1", "2", "3"} {
		if _, exists := api[good]; !exists {
			t.Errorf("普通节点 %s 被误删", good)
		}
	}
	if ks := inputsOf(t, api, "3"); ks["steps"] != float64(8) {
		t.Errorf("普通节点参数被破坏: steps=%#v", ks["steps"])
	}
}

// 旁路（mode=4）/ 静音（mode=2）的节点前端也会剔除，且下游要「穿过」它接到真正的上游，
// 否则下游会变成 required_input_missing。
const bypassedNodeFixture = `{
  "nodes": [
    {"id": 1, "type": "UNETLoader", "inputs": [],
     "widgets_values": ["krea-2-turbo-int8-convrot.safetensors", "default"],
     "widgets_values_named": {"unet_name": "krea-2-turbo-int8-convrot.safetensors", "weight_dtype": "default"}},
    {"id": 2, "type": "LoraLoader", "mode": 4,
     "inputs": [{"name": "model", "type": "MODEL", "link": 1},
                {"name": "clip", "type": "CLIP", "link": null},
                {"name": "lora_name", "type": "COMBO", "widget": {"name": "lora_name"}, "link": null}],
     "widgets_values": ["已删掉的lora.safetensors", 1, 1],
     "widgets_values_named": {"lora_name": "已删掉的lora.safetensors", "strength_model": 1, "strength_clip": 1}},
    {"id": 3, "type": "KSampler",
     "inputs": [{"name": "model", "type": "MODEL", "link": 2}],
     "widgets_values": [1, "randomize", 8, 1, "euler", "simple", 1],
     "widgets_values_named": {"seed": 1, "control_after_generate": "randomize", "steps": 8,
       "cfg": 1, "sampler_name": "euler", "scheduler": "simple", "denoise": 1}},
    {"id": 4, "type": "KSampler", "mode": 2, "inputs": [], "widgets_values": []}
  ],
  "links": [[1, 1, 0, 2, 0, "MODEL"], [2, 2, 0, 3, 0, "MODEL"]]
}`

func TestUIToAPIFormatSkipsBypassedNodesAndPassesLinksThrough(t *testing.T) {
	api := convertFixture(t, bypassedNodeFixture)

	if _, exists := api["2"]; exists {
		t.Error("旁路节点（mode=4）不该进载荷")
	}
	if _, exists := api["4"]; exists {
		t.Error("静音节点（mode=2）不该进载荷")
	}
	// 旁路节点被剔除后，下游要直连到它的上游（node 1），不能悬空
	link, ok := inputsOf(t, api, "3")["model"].([]any)
	if !ok || len(link) != 2 {
		t.Fatalf("KSampler.model 应为连线: %#v", inputsOf(t, api, "3")["model"])
	}
	if link[0] != "1" {
		t.Errorf("连线没有穿过被旁路的节点：期望接到节点 1，实际 %#v", link)
	}
}

// 指向「已被剔除且无上游可穿透」的连线，要按前端的收尾逻辑删掉，不能留悬空引用。
const danglingLinkFixture = `{
  "nodes": [
    {"id": 1, "type": "KSampler",
     "inputs": [{"name": "positive", "type": "CONDITIONING", "link": 5}],
     "widgets_values": [1, "randomize", 8, 1, "euler", "simple", 1],
     "widgets_values_named": {"seed": 1, "control_after_generate": "randomize", "steps": 8,
       "cfg": 1, "sampler_name": "euler", "scheduler": "simple", "denoise": 1}},
    {"id": 9, "type": "Reroute", "inputs": [{"name": "", "type": "*", "link": null}]}
  ],
  "links": [[5, 9, 0, 1, 0, "CONDITIONING"]]
}`

func TestUIToAPIFormatDropsDanglingLinks(t *testing.T) {
	api := convertFixture(t, danglingLinkFixture)

	if _, exists := api["9"]; exists {
		t.Error("Reroute 是前端虚拟节点，不该进载荷")
	}
	if _, exists := inputsOf(t, api, "1")["positive"]; exists {
		t.Error("指向已剔除节点的连线应被删除，否则载荷里是悬空引用")
	}
}

// 老格式里「被连线占用的 widget」也带 widget 标记。连线必须压过 widgets_values_named
// 里的旧值，否则会用过期数值覆盖真实上游。
const linkedWidgetFixture = `{
  "nodes": [
    {"id": 1, "type": "PrimitiveInt", "inputs": [], "widgets_values": [8],
     "widgets_values_named": {"value": 8}},
    {"id": 2, "type": "KSampler",
     "inputs": [
       {"name": "seed", "type": "INT", "widget": {"name": "seed"}, "link": 7},
       {"name": "steps", "type": "INT", "widget": {"name": "steps"}, "link": null}],
     "widgets_values": [999, 999, 8],
     "widgets_values_named": {"seed": 999, "control_after_generate": "randomize", "steps": 8}}
  ],
  "links": [[7, 1, 0, 2, 0, "INT"]]
}`

func TestUIToAPIFormatLinkBeatsStaleWidgetValue(t *testing.T) {
	api := convertFixture(t, linkedWidgetFixture)

	ks := inputsOf(t, api, "2")
	link, ok := ks["seed"].([]any)
	if !ok || len(link) != 2 || link[0] != "1" {
		t.Errorf("被连线占用的输入应以连线为准，实际 %#v", ks["seed"])
	}
	if ks["steps"] != float64(8) {
		t.Errorf("未被连线占用的 widget 仍要取 widgets_values_named: steps=%#v", ks["steps"])
	}
}

// 子图（Subgraph）是前端虚拟节点，提交前必须展开；本系统还不能展开，
// 必须明确报错——照原样发出去只会换来一句含糊的 400，静默丢弃会跑出错图。
const subgraphFixture = `{
  "definitions": {"subgraphs": [{"id": "9de1bd0a-0000-0000-0000-000000000001", "name": "MySubgraph"}]},
  "nodes": [
    {"id": 1, "type": "MySubgraph", "inputs": [], "widgets_values": []},
    {"id": 2, "type": "SaveImage", "inputs": [{"name": "images", "type": "IMAGE", "link": 1}],
     "widgets_values": ["out"], "widgets_values_named": {"filename_prefix": "out"}}
  ],
  "links": [[1, 1, 0, 2, 0, "IMAGE"]]
}`

func TestUIToAPIFormatRejectsSubgraph(t *testing.T) {
	var ui map[string]any
	if err := json.Unmarshal([]byte(subgraphFixture), &ui); err != nil {
		t.Fatalf("fixture 解析失败: %v", err)
	}
	_, err := uiToAPIFormat(ui)
	if err == nil {
		t.Fatal("含子图的工作流必须报错，不能悄悄产出一张缺节点的图")
	}
	if !errors.Is(err, errUnsupportedWorkflowFeature) {
		t.Errorf("应返回 errUnsupportedWorkflowFeature 以便与「工作流写错」区分开，实际: %v", err)
	}
	t.Logf("子图报错文案: %v", err)
}

// nodeSkipReason 是上面所有行为的判据，单独锁一遍。
func TestNodeSkipReason(t *testing.T) {
	cases := []struct {
		node   map[string]any
		reason bool
	}{
		{map[string]any{"type": "KSampler"}, false},
		{map[string]any{"type": "KSampler", "mode": float64(0)}, false},
		{map[string]any{"type": "KSampler", "mode": float64(2)}, true},
		{map[string]any{"type": "KSampler", "mode": float64(4)}, true},
		{map[string]any{"type": "Note"}, true},
		{map[string]any{"type": "MarkdownNote"}, true},
		{map[string]any{"type": "Reroute"}, true},
		{map[string]any{"type": "PrimitiveNode"}, true},
	}
	for _, c := range cases {
		got := nodeSkipReason(c.node) != ""
		if got != c.reason {
			t.Errorf("nodeSkipReason(%v) = %v, 期望 %v", c.node, got, c.reason)
		}
	}
}
