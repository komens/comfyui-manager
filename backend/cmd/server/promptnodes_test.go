package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 一份刻意「不按套路出牌」的 API 格式工作流：
//   - 采样器叫 MyCustomSampler，不是 KSampler；
//   - 正向编码器叫 TextEncodeQwenImageEdit，而且中间还隔着
//     ConditioningKrea2Rebalance 这个透传节点；
//   - 负向编码器用 prompt 而不是 text 字段；
//   - 另有一个与采样器无关的文本节点（容易被纯启发式误伤）。
func sampleWorkflow() map[string]any {
	return map[string]any{
		"3": map[string]any{
			"class_type": "MyCustomSampler",
			"inputs": map[string]any{
				"positive": []any{"9", float64(0)},
				"negative": []any{"6", float64(0)},
				"steps":    float64(28),
			},
		},
		"9": map[string]any{
			"class_type": "ConditioningKrea2Rebalance",
			"inputs": map[string]any{
				"conditioning": []any{"5", float64(0)},
				"weight":       float64(1.2),
			},
		},
		"5": map[string]any{
			"class_type": "TextEncodeQwenImageEdit",
			"inputs": map[string]any{
				"text": "old positive",
				"clip": []any{"8", float64(0)},
			},
		},
		"6": map[string]any{
			"class_type": "SomeNegativeEncoder",
			"inputs": map[string]any{
				"prompt": "old negative",
			},
		},
		"7": map[string]any{
			"class_type": "CLIPTextEncode",
			"inputs": map[string]any{
				"text": "unrelated style node",
			},
		},
	}
}

// 同一节点上既有正向又有负向文本框：Image_EDIT_Qwen 的 TextEncodeQwenImage21
// 就是这个形状，它的两个输出分别接采样器的 positive / negative。
// 这种落点必须按**字段**区分，只记节点 id 会让负向把正向盖掉。
func sameNodeTwoFieldsWorkflow() map[string]any {
	return map[string]any{
		"11": map[string]any{
			"class_type": "KSampler",
			"inputs": map[string]any{
				"positive": []any{"8", float64(0)},
				"negative": []any{"8", float64(1)},
			},
		},
		"8": map[string]any{
			"class_type": "TextEncodeQwenImage21",
			"inputs": map[string]any{
				"prompt":          "",
				"negative_prompt": "",
				"resolution":      float64(1024),
			},
		},
	}
}

// Krea2 那份工作流就是这个形状：正负向共用 node 6，负向是 ConditioningZeroOut
// 从正向派生出来的，工作流里没有独立的负向文本。
func sharedFieldWorkflow() map[string]any {
	return map[string]any{
		"7": map[string]any{"class_type": "KSampler", "inputs": map[string]any{
			"positive": []any{"5", float64(0)},
			"negative": []any{"1", float64(0)},
		}},
		"5": map[string]any{"class_type": "ConditioningKrea2Rebalance", "inputs": map[string]any{
			"conditioning": []any{"6", float64(0)},
		}},
		"1": map[string]any{"class_type": "ConditioningZeroOut", "inputs": map[string]any{
			"conditioning": []any{"6", float64(0)},
		}},
		"6": map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "original"}},
	}
}

func inputValue(workflow map[string]any, nodeID, field string) any {
	node, ok := workflow[nodeID].(map[string]any)
	if !ok {
		return nil
	}
	return nodeInputs(node)[field]
}

func TestResolvePromptTargetsFollowsLinks(t *testing.T) {
	targets := resolvePromptTargets(sampleWorkflow())

	if targets.Positive == nil || targets.Positive.NodeID != "5" || targets.Positive.Field != "text" {
		t.Errorf("正向落点应为 {5 text}，实际 %+v", targets.Positive)
	}
	// 关键：不是靠字段名 text 找的，负向用 prompt 也能命中
	if targets.Negative == nil || targets.Negative.NodeID != "6" || targets.Negative.Field != "prompt" {
		t.Errorf("负向落点应为 {6 prompt}，实际 %+v", targets.Negative)
	}
	if targets.NegativeShared {
		t.Error("落在不同节点上不该判定为「负向与正向同源」")
	}
}

func TestInjectWritesBothSidesAndLeavesUnrelatedNodesAlone(t *testing.T) {
	workflow := sampleWorkflow()
	err := injectDirectPrompt(workflow, "{}", map[string]any{
		"positive_prompt": "new positive",
		"negative_prompt": "new negative",
	})
	if err != nil {
		t.Fatalf("注入失败：%v", err)
	}

	if got := inputValue(workflow, "5", "text"); got != "new positive" {
		t.Errorf("正向编码器未被写入：%v", got)
	}
	if got := inputValue(workflow, "6", "prompt"); got != "new negative" {
		t.Errorf("负向编码器未被写入：%v", got)
	}
	// 结构解析成功时不该再跑全局启发式，否则无关节点会被覆盖成正向
	if got := inputValue(workflow, "7", "text"); got != "unrelated style node" {
		t.Errorf("无关文本节点被误改：%v", got)
	}
}

func TestInjectKeepsWorkflowNegativeWhenEmpty(t *testing.T) {
	workflow := sampleWorkflow()
	if err := injectDirectPrompt(workflow, "{}", map[string]any{"positive_prompt": "only positive"}); err != nil {
		t.Fatalf("注入失败：%v", err)
	}

	if got := inputValue(workflow, "6", "prompt"); got != "old negative" {
		t.Errorf("negative 为空时应保留工作流自带负向，实际被改成 %v", got)
	}
}

// 同节点、两个字段：正负向必须各写各的字段。这是「只记 node_id」会踩的坑。
func TestInjectSameNodeDifferentFields(t *testing.T) {
	workflow := sameNodeTwoFieldsWorkflow()
	targets := resolvePromptTargets(workflow)
	if targets.Positive == nil || targets.Positive.Field != "prompt" {
		t.Fatalf("正向应落到 prompt 字段，实际 %+v", targets.Positive)
	}
	if targets.Negative == nil || targets.Negative.Field != "negative_prompt" {
		t.Fatalf("负向应落到 negative_prompt 字段，实际 %+v", targets.Negative)
	}
	if targets.NegativeShared {
		t.Fatal("两个字段不同，不该判定为同源")
	}

	if err := injectDirectPrompt(workflow, "{}", map[string]any{
		"positive_prompt": "P",
		"negative_prompt": "N",
	}); err != nil {
		t.Fatalf("注入失败：%v", err)
	}
	if got := inputValue(workflow, "8", "prompt"); got != "P" {
		t.Errorf("prompt 字段应为 P，实际 %v", got)
	}
	if got := inputValue(workflow, "8", "negative_prompt"); got != "N" {
		t.Errorf("negative_prompt 字段应为 N，实际 %v", got)
	}
}

// 同节点同一字段：只写一次，且负向不能把正向盖掉。
func TestInjectSharedFieldIsWrittenOnce(t *testing.T) {
	workflow := sharedFieldWorkflow()
	targets := resolvePromptTargets(workflow)
	if targets.Positive == nil || targets.Negative == nil {
		t.Fatalf("两侧都应解析到节点 6，实际 正向=%+v 负向=%+v", targets.Positive, targets.Negative)
	}
	if !targets.NegativeShared {
		t.Fatal("正负向落在同一节点同一字段，应判定为同源")
	}

	if err := injectDirectPrompt(workflow, "{}", map[string]any{
		"positive_prompt": "the positive",
		"negative_prompt": "the negative",
	}); err != nil {
		t.Fatalf("注入失败：%v", err)
	}
	if got := inputValue(workflow, "6", "text"); got != "the positive" {
		t.Errorf("共用字段应保留正向，实际 %v", got)
	}
}

// 编辑器里显式选了节点时以选择为准，即便自动识别指向别处。
func TestInjectHonorsPinnedTarget(t *testing.T) {
	workflow := sampleWorkflow()
	mapping := `{"negative_prompt":{"node_id":"7","field":"inputs.text"}}`
	if err := injectDirectPrompt(workflow, mapping, map[string]any{
		"positive_prompt": "P",
		"negative_prompt": "N",
	}); err != nil {
		t.Fatalf("注入失败：%v", err)
	}
	if got := inputValue(workflow, "7", "text"); got != "N" {
		t.Errorf("显式选中的节点 7 应被写入，实际 %v", got)
	}
	if got := inputValue(workflow, "6", "prompt"); got != "old negative" {
		t.Errorf("没被选中的节点 6 不该再被写，实际 %v", got)
	}
}

// 选中的节点不在这份工作流里（工作流被换过）要退回自动解析，而不是报错或乱写。
func TestInjectFallsBackWhenPinnedNodeMissing(t *testing.T) {
	workflow := sampleWorkflow()
	mapping := `{"positive_prompt":{"node_id":"999","field":"text"}}`
	if err := injectDirectPrompt(workflow, mapping, map[string]any{"positive_prompt": "P"}); err != nil {
		t.Fatalf("注入失败：%v", err)
	}
	if got := inputValue(workflow, "5", "text"); got != "P" {
		t.Errorf("应退回自动解析写入节点 5，实际 %v", got)
	}
}

// 找不到落点时必须报错。静默跳过会让人以为提示词生效了，出来的却是工作流自带的旧内容。
func TestInjectErrorsWhenTargetUnresolved(t *testing.T) {
	workflow := map[string]any{
		"1": map[string]any{"class_type": "KSampler", "inputs": map[string]any{
			"conditioning": []any{"2", float64(0)},
		}},
		"2": map[string]any{"class_type": "ConditioningZeroOut", "inputs": map[string]any{
			"conditioning": []any{"1", float64(0)},
		}},
	}
	err := injectDirectPrompt(workflow, "{}", map[string]any{"positive_prompt": "P"})
	if err == nil {
		t.Fatal("解析不出落点时应返回错误")
	}
	if !strings.Contains(err.Error(), "正向提示词落点") {
		t.Errorf("错误信息应指出是落点问题，实际：%v", err)
	}
}

func TestAutoInjectSeedCoversEverySeedInput(t *testing.T) {
	workflow := map[string]any{
		"1": map[string]any{"class_type": "MyCustomSampler", "inputs": map[string]any{"seed": float64(1)}},
		"2": map[string]any{"class_type": "Noise", "inputs": map[string]any{"seed": float64(2)}},
		"3": map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "x"}},
		// seed 是连线：值由上游决定，不能被覆盖
		"4": map[string]any{"class_type": "SamplerCustom", "inputs": map[string]any{"seed": []any{"2", float64(0)}}},
	}
	autoInjectSeed(workflow, float64(999))

	if got := inputValue(workflow, "1", "seed"); got != float64(999) {
		t.Errorf("自定义采样器的 seed 未被写入：%v", got)
	}
	if got := inputValue(workflow, "2", "seed"); got != float64(999) {
		t.Errorf("Noise 节点的 seed 未被写入：%v", got)
	}
	if got := inputValue(workflow, "4", "seed"); !reflect.DeepEqual(got, []any{"2", float64(0)}) {
		t.Errorf("连线型 seed 不应被覆盖，实际 %#v", got)
	}
}

// ---------------------------------------------------------------------------
// UI 导出格式（离线路径）
// ---------------------------------------------------------------------------

// 一份 UI 导出格式的工作流：ComfyUI 连不上时也要能算出落点。
func uiFormatWorkflow() map[string]any {
	return map[string]any{
		"nodes": []any{
			map[string]any{
				"id": float64(1), "type": "UNETLoader",
				"widgets_values_named": map[string]any{"unet_name": "z-image.safetensors", "weight_dtype": "default"},
			},
			map[string]any{
				"id": float64(5), "type": "CLIPTextEncode",
				"widgets_values_named": map[string]any{"text": "a girl"},
				"inputs":               []any{map[string]any{"name": "clip", "link": float64(1)}},
			},
			map[string]any{
				"id": float64(6), "type": "CLIPTextEncode",
				"widgets_values_named": map[string]any{"text": "worst quality"},
			},
			map[string]any{
				"id": float64(8), "type": "KSampler",
				"inputs": []any{
					map[string]any{"name": "positive", "link": float64(6)},
					map[string]any{"name": "negative", "link": float64(7)},
				},
			},
		},
		"links": []any{
			[]any{float64(6), float64(5), float64(0), float64(8), float64(1), "CONDITIONING"},
			[]any{float64(7), float64(6), float64(0), float64(8), float64(2), "CONDITIONING"},
		},
	}
}

func TestResolvePromptTargetsFromUIFormat(t *testing.T) {
	targets := resolvePromptTargetsFromUI(uiFormatWorkflow())
	if targets.Positive == nil || targets.Positive.NodeID != "5" || targets.Positive.Field != "text" {
		t.Errorf("UI 格式正向落点应为 {5 text}，实际 %+v", targets.Positive)
	}
	if targets.Negative == nil || targets.Negative.NodeID != "6" || targets.Negative.Field != "text" {
		t.Errorf("UI 格式负向落点应为 {6 text}，实际 %+v", targets.Negative)
	}
}

// 老格式（没有 widgets_values_named）离线时解析不出字段名，但也不该编出一个错的。
func TestResolvePromptTargetsFromOldUIFormatIsEmpty(t *testing.T) {
	workflow := map[string]any{
		"nodes": []any{
			map[string]any{"id": float64(2), "type": "CLIPTextEncode", "widgets_values": []any{"a girl"}},
			map[string]any{"id": float64(3), "type": "CLIPTextEncode", "widgets_values": []any{"bad"}},
			map[string]any{
				"id": float64(5), "type": "KSampler",
				"inputs": []any{
					map[string]any{"name": "positive", "link": float64(3)},
					map[string]any{"name": "negative", "link": float64(4)},
				},
			},
		},
		"links": []any{
			[]any{float64(3), float64(2), float64(0), float64(5), float64(1), "CONDITIONING"},
			[]any{float64(4), float64(3), float64(0), float64(5), float64(2), "CONDITIONING"},
		},
	}
	targets := resolvePromptTargetsFromUI(workflow)
	if targets.Positive != nil || targets.Negative != nil {
		t.Errorf("老格式离线不该硬猜落点，实际 %+v / %+v", targets.Positive, targets.Negative)
	}
}

// 候选列表要能列出可选的提示词节点，老格式至少列得出节点（字段名未知）。
func TestCollectPromptCandidates(t *testing.T) {
	candidates := collectPromptCandidates(uiFormatWorkflow())
	byKey := map[string]promptCandidate{}
	for _, candidate := range candidates {
		byKey[candidate.NodeID+"/"+candidate.Field] = candidate
	}
	if _, ok := byKey["5/text"]; !ok {
		t.Fatalf("候选里应有节点 5 的 text 字段，实际 %+v", candidates)
	}
	if byKey["5/text"].Role != "positive" {
		t.Errorf("节点 5 应被标成正向，实际 %q", byKey["5/text"].Role)
	}
	if byKey["6/text"].Role != "negative" {
		t.Errorf("节点 6 应被标成负向，实际 %q", byKey["6/text"].Role)
	}
	// 模型加载器的 unet_name 不是提示词字段，不该混进候选
	if _, ok := byKey["1/unet_name"]; ok {
		t.Error("unet_name 不该被当成提示词候选")
	}

	old := collectPromptCandidates(map[string]any{
		"nodes": []any{
			map[string]any{"id": float64(2), "type": "CLIPTextEncode", "widgets_values": []any{"a girl"}},
		},
	})
	if len(old) != 1 || old[0].NodeID != "2" || old[0].Field != "" {
		t.Errorf("老格式应列出节点且字段名留空，实际 %+v", old)
	}
	if old[0].Preview != "a girl" {
		t.Errorf("老格式候选应带控件原文，实际 %q", old[0].Preview)
	}
}

// 前端专用节点（Note / Reroute）、静音（mode=2）与旁路（mode=4）节点都不会进提交载荷，
// 把它们列进下拉等于给一个「写了也白写」的位置，必须剔除。
func TestCollectPromptCandidatesSkipsFrontendOnlyNodes(t *testing.T) {
	ui := map[string]any{
		"nodes": []any{
			map[string]any{"id": float64(2), "type": "Note",
				"widgets_values": []any{"这是备注，不是提示词"}},
			map[string]any{"id": float64(3), "type": "Reroute"},
			map[string]any{"id": float64(4), "type": "KSampler", "mode": float64(4),
				"widgets_values": []any{"旁路节点上的文本"}},
			map[string]any{"id": float64(5), "type": "CLIPTextEncode",
				"widgets_values_named": map[string]any{"text": "a girl"}},
		},
	}
	candidates := collectPromptCandidates(ui)
	if len(candidates) != 1 || candidates[0].NodeID != "5" {
		t.Fatalf("UI 格式只应剩下节点 5，实际 %+v", candidates)
	}

	// API 格式的节点没有 type/mode，只认得出前端专用节点的类名
	apiCandidates := collectPromptCandidates(map[string]any{
		"2": map[string]any{"class_type": "Note", "inputs": map[string]any{"text": "备注"}},
		"5": map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "a girl"}},
	})
	if len(apiCandidates) != 1 || apiCandidates[0].NodeID != "5" {
		t.Fatalf("API 格式只应剩下节点 5，实际 %+v", apiCandidates)
	}
}

// ---------------------------------------------------------------------------
// 字段挑选
// ---------------------------------------------------------------------------

func TestPromptFieldsOfAndPickPromptField(t *testing.T) {
	fields := promptFieldsOf(map[string]any{"prompt": "a", "negative_prompt": "b", "resolution": float64(1024)})
	if !reflect.DeepEqual(fields, []string{"prompt", "negative_prompt"}) {
		t.Fatalf("候选字段应为 [prompt negative_prompt]，实际 %v", fields)
	}
	if got := pickPromptField(fields, rolePositive); got != "prompt" {
		t.Errorf("正向应挑 prompt，实际 %q", got)
	}
	if got := pickPromptField(fields, roleNegative); got != "negative_prompt" {
		t.Errorf("负向应挑 negative_prompt，实际 %q", got)
	}

	one := promptFieldsOf(map[string]any{"text": "x", "steps": float64(20)})
	if !reflect.DeepEqual(one, []string{"text"}) {
		t.Fatalf("候选字段应为 [text]，实际 %v", one)
	}
	if got := pickPromptField(one, roleNegative); got != "text" {
		t.Errorf("没有负向字段时应退回正向字段（表示同源），实际 %q", got)
	}

	if got := promptFieldsOf(map[string]any{"text": float64(3)}); got != nil {
		t.Errorf("非字符串的 text 不算文本框，实际 %v", got)
	}
	if got := promptFieldsOf(map[string]any{"image": "a.png"}); got != nil {
		t.Errorf("image 不是文本框，实际 %v", got)
	}
}

// 前端控件名必须剔除，否则会作为未知输入发给 ComfyUI。
func TestFrontendOnlyWidgetNamesCoverUpload(t *testing.T) {
	if !frontendOnlyWidgetNames["control_after_generate"] {
		t.Error("control_after_generate 必须剔除")
	}
	if !frontendOnlyWidgetNames["upload"] {
		t.Error("LoadImage 的 upload 按钮控件必须剔除")
	}
	if frontendOnlyWidgetNames["format.bit_depth"] {
		t.Error("format.bit_depth 是节点真实输入，不能被误剔")
	}
}

func TestNodeSkipReasonHonorsVirtualNodeFlag(t *testing.T) {
	// 类型名清单里没有的新虚拟节点，靠标记也能认出来
	if got := nodeSkipReason(map[string]any{"type": "SomeNewReroute", "isVirtualNode": true}); got == "" {
		t.Error("带 isVirtualNode 标记的节点应被跳过")
	}
	if got := nodeSkipReason(map[string]any{"type": "Note"}); got == "" {
		t.Error("老导出的 Note 应被跳过")
	}
	if got := nodeSkipReason(map[string]any{"type": "KSampler", "mode": float64(4)}); got == "" {
		t.Error("旁路节点应被跳过")
	}
	if got := nodeSkipReason(map[string]any{"type": "KSampler"}); got != "" {
		t.Errorf("普通节点不该被跳过，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 保存时的映射合并
// ---------------------------------------------------------------------------

// 「留空 = 自动」必须成立：清空选择后不能还留着旧节点，
// 否则编辑器显示「自动」而实际写的是上次选的节点。
func TestMergePromptMappingFillsUnselectedSides(t *testing.T) {
	raw, err := json.Marshal(uiFormatWorkflow())
	if err != nil {
		t.Fatalf("序列化测试工作流失败：%v", err)
	}

	// 两侧都没选 → 全部用自动识别的结果
	merged := mergePromptMapping(json.RawMessage(`{}`), raw)
	var mapping promptMapping
	if err := json.Unmarshal(merged, &mapping); err != nil {
		t.Fatalf("合并结果不是合法 JSON：%v", err)
	}
	if mapping.Positive.NodeID != "5" || mapping.Negative.NodeID != "6" {
		t.Errorf("两侧都应填上自动识别结果，实际 %+v", mapping)
	}

	// 只选了正向 → 负向仍由自动识别补
	merged = mergePromptMapping(json.RawMessage(`{"positive_prompt":{"node_id":"9","field":"text"}}`), raw)
	if err := json.Unmarshal(merged, &mapping); err != nil {
		t.Fatalf("合并结果不是合法 JSON：%v", err)
	}
	if mapping.Positive.NodeID != "9" {
		t.Errorf("显式选择必须保留，实际 %+v", mapping.Positive)
	}
	if mapping.Negative.NodeID != "6" {
		t.Errorf("未选的一侧应由自动识别补上，实际 %+v", mapping.Negative)
	}
}

// 老格式离线解析不出字段名，但也不该把映射写成空——节点 id 还是拿得到时照常存。
func TestAutoDetectPromptMappingReturnsNilWhenUnknown(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"nodes": []any{
			map[string]any{"id": float64(2), "type": "CLIPTextEncode", "widgets_values": []any{"a girl"}},
			map[string]any{
				"id": float64(5), "type": "KSampler",
				"inputs": []any{
					map[string]any{"name": "positive", "link": float64(3)},
					map[string]any{"name": "negative", "link": float64(4)},
				},
			},
		},
		"links": []any{
			[]any{float64(3), float64(2), float64(0), float64(5), float64(1), "CONDITIONING"},
			[]any{float64(4), float64(3), float64(0), float64(5), float64(2), "CONDITIONING"},
		},
	})
	if err != nil {
		t.Fatalf("序列化测试工作流失败：%v", err)
	}
	if detected := autoDetectPromptMapping(raw); len(detected) != 0 {
		t.Errorf("解析不出落点时应返回空，实际 %s", string(detected))
	}
}
