package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// kreaClipLoaderInput 是 CLIPLoader 的 input 段（照 192.168.10.34:8188 上的候选写）。
// 两条路径共用它：本地直接构造 nodeInputDef，以及 httptest 里冒充 /object_info。
// 文件名的先后顺序就是 ComfyUI 列出来的顺序 —— 没写 default 的下拉取第一项当默认值，
// 所以这个顺序直接决定了自动纠正补出来的是哪个文本编码器。
const kreaClipLoaderInput = `{
  "required": {
    "clip_name": ["COMBO", {"options": [
      "qwen3VL4BAbliteratedComfyui_v10_full_fp8.safetensors",
      "qwen3_0.6b_fp16.safetensors",
      "qwen_2.5_vl_7b_fp8_scaled.safetensors"
    ]}],
    "type": ["COMBO", {"options": [
      "stable_diffusion", "qwen_image", "krea2"
    ], "default": "stable_diffusion"}]
  },
  "optional": {
    "device": ["COMBO", {"options": ["default", "cpu"], "default": "default"}]
  }
}`

func kreaClipLoaderDef() nodeInputDef {
	def := nodeInputDef{Spec: map[string]map[string]any{}}
	if err := json.Unmarshal([]byte(kreaClipLoaderInput), &def.Spec); err != nil {
		panic(err)
	}
	slots, err := parseWidgetSlots([]byte(kreaClipLoaderInput))
	if err != nil {
		panic(err)
	}
	def.Slots = slots
	return def
}

// kreaRejection 是 ComfyUI 对「clip_name: 'krea2' not in [...]」的真实拒绝体结构
// （内容照 2026-10-01 云端 Krea2 工作流的实际响应还原）。
const kreaRejection = `{
  "error": {"type": "prompt_outputs_failed_validation", "message": "Prompt outputs failed validation", "details": ""},
  "node_errors": {
    "8": {
      "class_type": "CLIPLoader",
      "errors": [{
        "type": "value_not_in_list",
        "message": "Value not in list",
        "details": "clip_name: 'krea2' not in ['qwen3VL4BAbliteratedComfyui_v10_full_fp8.safetensors', 'qwen3_0.6b_fp16.safetensors', 'qwen_2.5_vl_7b_fp8_scaled.safetensors']",
        "extra_info": {"input_name": "clip_name", "input_value": "krea2"}
      }]
    }
  }
}`

// kreaPrompt 是一份「值串位」的提交载荷：clip_name 拿到了 type 的值，
// type 停在默认值上，真正的文本编码器在这份文件里已经丢了。
func kreaPrompt() map[string]any {
	return map[string]any{
		"8": map[string]any{
			"class_type": "CLIPLoader",
			"inputs": map[string]any{
				"clip_name": "krea2",
				"type":      "stable_diffusion",
				"device":    "default",
			},
		},
	}
}

func fixedLookup(classType string) (nodeInputDef, bool) {
	if classType == "CLIPLoader" {
		return kreaClipLoaderDef(), true
	}
	return nodeInputDef{}, false
}

// TestPlanNodeRepairsKreaClipLoader 是这次改动的靶心：串位的值被挪回 type，
// 腾空的 clip_name 用候选第一项补上 —— 正好就是本来的文本编码器。
func TestPlanNodeRepairsKreaClipLoader(t *testing.T) {
	inputs := kreaPrompt()["8"].(map[string]any)["inputs"].(map[string]any)
	before := cloneInputs(inputs)

	repairs := planNodeRepairs("8", "CLIPLoader", inputs, []string{"clip_name"}, kreaClipLoaderDef().Spec)

	if len(repairs) != 1 {
		t.Fatalf("应恰好算出 1 处纠正，实际 %d 处：%+v", len(repairs), repairs)
	}
	repair := repairs[0]
	if repair.Field != "clip_name" || repair.Target != "type" {
		t.Errorf("落点不对：字段 %q → 目标 %q", repair.Field, repair.Target)
	}
	if repair.Value != "krea2" {
		t.Errorf("该挪走的值应为 krea2，实际 %q", repair.Value)
	}
	if repair.Replaced != "stable_diffusion" {
		t.Errorf("应记录被覆盖的原值 stable_diffusion，实际 %q", repair.Replaced)
	}
	if repair.Filled != "qwen3VL4BAbliteratedComfyui_v10_full_fp8.safetensors" {
		t.Errorf("clip_name 应补候选第一项，实际 %q", repair.Filled)
	}

	// planNodeRepairs 只算不改：调用方没应用之前，载荷必须原封不动。
	for name, value := range before {
		if inputs[name] != value {
			t.Errorf("planNodeRepairs 不该改动载荷：%s 从 %v 变成 %v", name, value, inputs[name])
		}
	}

	applyRepairs(inputs, repairs)
	if inputs["type"] != "krea2" {
		t.Errorf("应用后 type 应为 krea2，实际 %v", inputs["type"])
	}
	if inputs["clip_name"] != "qwen3VL4BAbliteratedComfyui_v10_full_fp8.safetensors" {
		t.Errorf("应用后 clip_name 应补上文本编码器，实际 %v", inputs["clip_name"])
	}
	if inputs["device"] != "default" {
		t.Errorf("无关字段 device 不该被动：%v", inputs["device"])
	}
}

// TestPlanRepairsIgnoresUnownedValue 值已经无处可放（模型被删）时一个字都不改：
// 静默换成别的模型比直接报错更糟。
func TestPlanRepairsIgnoresUnownedValue(t *testing.T) {
	prompt := map[string]any{
		"12": map[string]any{
			"class_type": "LoraLoader",
			"inputs": map[string]any{
				"lora_name":      "已删掉的lora.safetensors",
				"strength_model": 0.9,
				"strength_clip":  0.9,
			},
		},
	}
	def := nodeInputDef{Spec: map[string]map[string]any{
		"required": {
			"lora_name":      []any{"COMBO", map[string]any{"options": []any{"a.safetensors", "b.safetensors"}}},
			"strength_model": []any{"FLOAT", map[string]any{"default": 1.0}},
		},
		"optional": {
			"strength_clip": []any{"FLOAT", map[string]any{"default": 1.0}},
		},
	}}
	lookup := func(classType string) (nodeInputDef, bool) { return def, classType == "LoraLoader" }

	repairs := planRepairs(prompt, rejectionFor("12", "LoraLoader", "lora_name"), lookup)
	if len(repairs) != 0 {
		t.Fatalf("删除的模型没有归属，不该被改：%+v", repairs)
	}
	inputs := prompt["12"].(map[string]any)["inputs"].(map[string]any)
	if inputs["lora_name"] != "已删掉的lora.safetensors" {
		t.Errorf("载荷被改动了：%v", inputs["lora_name"])
	}
}

// TestPlanNodeRepairsSkipsAmbiguousTarget 同一个值有多个字段都接受时不动手：
// 硬选一个等于替用户猜。
func TestPlanNodeRepairsSkipsAmbiguousTarget(t *testing.T) {
	spec := map[string]map[string]any{
		"required": {
			"first":  []any{"COMBO", map[string]any{"options": []any{"shared", "x"}}},
			"second": []any{"COMBO", map[string]any{"options": []any{"shared", "y"}}},
			"third":  []any{"COMBO", map[string]any{"options": []any{"shared", "z"}}},
		},
	}
	inputs := map[string]any{"first": "shared", "second": "s", "third": "t"}

	if repairs := planNodeRepairs("1", "Ambiguous", inputs, []string{"first"}, spec); len(repairs) != 0 {
		t.Fatalf("两个字段都接受 shared，应放弃纠正，实际 %+v", repairs)
	}
}

// TestPlanNodeRepairsOneTargetOneClaim 两个错位值抢同一个落点时只放一个，
// 否则后写的会把先写的顶掉。
func TestPlanNodeRepairsOneTargetOneClaim(t *testing.T) {
	spec := map[string]map[string]any{
		"required": {
			"a": []any{"COMBO", map[string]any{"options": []any{"va", "x"}}},
			"b": []any{"COMBO", map[string]any{"options": []any{"vb", "y"}}},
			"t": []any{"COMBO", map[string]any{"options": []any{"va", "vb", "t0"}}},
		},
	}
	inputs := map[string]any{"a": "vb", "b": "va", "t": "t0"}

	repairs := planNodeRepairs("1", "Two", inputs, []string{"a", "b"}, spec)
	if len(repairs) != 1 {
		t.Fatalf("只允许一处纠正落在 t 上，实际 %d 处：%+v", len(repairs), repairs)
	}
	// a 排序在前：先把 a 的 vb 挪到 t；轮到 b 时 t 已被占用，放弃。
	if repairs[0].Field != "a" || repairs[0].Target != "t" {
		t.Errorf("按字段名排序应先处理 a → t，实际 %s → %s", repairs[0].Field, repairs[0].Target)
	}
}

// TestOffendingFieldFromDetails 老版 ComfyUI 没有 extra_info，只能从 details 抠字段名。
func TestOffendingFieldFromDetails(t *testing.T) {
	cases := []struct {
		name   string
		detail comfyErrorDetail
		want   string
	}{
		{"extra_info 优先", comfyErrorDetail{
			Details: "whatever", ExtraInfo: struct {
				InputName string `json:"input_name"`
			}{InputName: "clip_name"},
		}, "clip_name"},
		{"从 details 抠", comfyErrorDetail{
			Details: "clip_name: 'krea2' not in ['a']",
		}, "clip_name"},
		{"details 不是字段名形态", comfyErrorDetail{Details: "Prompt outputs failed validation"}, ""},
		{"空", comfyErrorDetail{}, ""},
		{"带引号说明抠错了", comfyErrorDetail{Details: "'krea2' not in ..."}, ""},
	}
	for _, tc := range cases {
		if got := offendingField(tc.detail); got != tc.want {
			t.Errorf("%s：期望 %q，实际 %q", tc.name, tc.want, got)
		}
	}
}

// TestPlanRepairsIgnoresOtherErrorKinds 只认「取值不在列表」。
// 节点不存在、必填缺失这类错误去改值只会越改越乱。
func TestPlanRepairsIgnoresOtherErrorKinds(t *testing.T) {
	var rejection comfyRejection
	if err := json.Unmarshal([]byte(`{"node_errors":{"8":{"class_type":"CLIPLoader",
	  "errors":[{"type":"required_input_missing","details":"clip_name: required input is missing"}]}}}`), &rejection); err != nil {
		t.Fatal(err)
	}
	if repairs := planRepairs(kreaPrompt(), rejection, fixedLookup); len(repairs) != 0 {
		t.Fatalf("required_input_missing 不该触发纠正：%+v", repairs)
	}
}

// TestPlanRepairsSkipsUnknownNodeType 拿不到节点定义就没法判断该挪去哪，跳过。
func TestPlanRepairsSkipsUnknownNodeType(t *testing.T) {
	noLookup := func(string) (nodeInputDef, bool) { return nodeInputDef{}, false }
	if repairs := planRepairs(kreaPrompt(), rejectionFor("8", "CLIPLoader", "clip_name"), noLookup); len(repairs) != 0 {
		t.Fatalf("没有节点定义时应放弃，实际 %+v", repairs)
	}
}

// TestDefaultOption 显式 default 优先，否则取候选第一项（ComfyUI 对新拖出的节点就是
// 「下拉里第一项」）。
func TestDefaultOption(t *testing.T) {
	explicit := []any{"COMBO", map[string]any{"options": []any{"a", "b"}, "default": "b"}}
	if got, ok := defaultOption(explicit); !ok || got != "b" {
		t.Errorf("显式 default 应生效，实际 %q ok=%v", got, ok)
	}
	implicit := []any{"COMBO", map[string]any{"options": []any{"first", "second"}}}
	if got, ok := defaultOption(implicit); !ok || got != "first" {
		t.Errorf("无 default 时应取第一项，实际 %q ok=%v", got, ok)
	}
	if _, ok := defaultOption([]any{"MODEL"}); ok {
		t.Error("连线型声明不该有默认值")
	}
}

// rejectionFor 造一个只有单条 value_not_in_list 的拒绝体，字段名走 details 路径
// （不带 extra_info），顺便覆盖老版 ComfyUI 的形态。
func rejectionFor(nodeID, classType, field string) comfyRejection {
	return comfyRejection{NodeErrors: map[string]comfyNodeError{
		nodeID: {
			ClassType: classType,
			Errors:    []comfyErrorDetail{{Type: "value_not_in_list", Details: field + ": 'x' not in ['y']"}},
		},
	}}
}

// TestSubmitComfyRepairingRetriesOnce 端到端：第一次被 400 拒，自动纠正后重试成功，
// 返回的 notes 说明改了什么。这是云端那次 Krea2 报错的完整重放。
func TestSubmitComfyRepairingRetriesOnce(t *testing.T) {
	var posts int32
	var bodyMu sync.Mutex
	var secondBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/object_info/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"CLIPLoader": map[string]any{
				"input": json.RawMessage(kreaClipLoaderInput),
			}})
		case r.URL.Path == "/prompt":
			if atomic.AddInt32(&posts, 1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(kreaRejection))
				return
			}
			var payload struct {
				Prompt map[string]any `json:"prompt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			encoded, _ := json.Marshal(payload.Prompt["8"])
			bodyMu.Lock()
			secondBody = encoded
			bodyMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": "retried-ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	promptID, notes, err := submitComfyRepairing(context.Background(), server.URL, kreaPrompt(), "comfyui-server-task-1")
	if err != nil {
		t.Fatalf("自动纠正后应提交成功，实际 %v", err)
	}
	if promptID != "retried-ok" {
		t.Errorf("应返回重试后的 prompt_id，实际 %q", promptID)
	}
	if len(notes) != 1 {
		t.Fatalf("应记录 1 条纠正说明，实际 %d 条：%v", len(notes), notes)
	}
	if !strings.Contains(notes[0], "krea2") || !strings.Contains(notes[0], "type") {
		t.Errorf("说明里要讲清楚挪了什么，实际 %q", notes[0])
	}
	bodyMu.Lock()
	sent := string(secondBody)
	bodyMu.Unlock()
	if !strings.Contains(sent, `"type":"krea2"`) {
		t.Errorf("重试载荷里 type 应为 krea2，实际 %s", sent)
	}
	if !strings.Contains(sent, "qwen3VL4BAbliteratedComfyui_v10_full_fp8.safetensors") {
		t.Errorf("重试载荷里 clip_name 应补上文本编码器，实际 %s", sent)
	}
	if atomic.LoadInt32(&posts) != 2 {
		t.Errorf("只应重试一次，实际提交 %d 次", posts)
	}
}

// TestSubmitComfyRepairingKeepsFirstErrorWhenNothingToFix 修不了就保持原样报错，
// 不能让用户看到一句「已自动纠正」却什么都没变。
func TestSubmitComfyRepairingKeepsFirstErrorWhenNothingToFix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/object_info/") {
			_, _ = w.Write([]byte(`{"CLIPLoader": {"input": {"required": {}}}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(kreaRejection))
	}))
	defer server.Close()

	_, notes, err := submitComfyRepairing(context.Background(), server.URL, kreaPrompt(), "c")
	if err == nil {
		t.Fatal("没有可纠正之处时应报错")
	}
	if len(notes) != 0 {
		t.Errorf("不该产出纠正说明：%v", notes)
	}
	if !strings.Contains(err.Error(), "clip_name") {
		t.Errorf("错误里应带上 ComfyUI 的原话，实际 %v", err)
	}
}

// TestSubmitComfyRepairingLeavesTransportErrorsAlone 连不上 ComfyUI 时不该去查节点定义、
// 更不该报成「取值问题」。
func TestSubmitComfyRepairingLeavesTransportErrorsAlone(t *testing.T) {
	promptID, notes, err := submitComfyRepairing(context.Background(), "http://127.0.0.1:9", kreaPrompt(), "c")
	if err == nil {
		t.Fatal("连不上应当报错")
	}
	if promptID != "" || len(notes) != 0 {
		t.Errorf("连接失败不该产生纠正：id=%q notes=%v", promptID, notes)
	}
	if strings.Contains(err.Error(), "value_not_in_list") {
		t.Errorf("连接错误被误报成取值问题：%v", err)
	}
}
