package main

import (
	"reflect"
	"strings"
	"testing"
)

// 真实抓下来的 400 响应体（Krea2 工作流 clip_name 写错那次）。
const realRejectionBody = `{
  "error": {"type": "prompt_outputs_failed_validation", "message": "Prompt outputs failed validation", "details": "", "extra_info": {}},
  "node_errors": {
    "8": {
      "errors": [{
        "type": "value_not_in_list",
        "message": "Value not in list",
        "details": "clip_name: 'krea2' not in ['a.safetensors', 'b.safetensors']",
        "extra_info": {"input_name": "clip_name", "received_value": "krea2"}
      }],
      "dependent_outputs": ["11"],
      "class_type": "CLIPLoader"
    }
  }
}`

func TestDescribeComfyRejectionNodeErrors(t *testing.T) {
	got := describeComfyRejection(strings.NewReader(realRejectionBody))
	want := ": node 8 (CLIPLoader): clip_name: 'krea2' not in ['a.safetensors', 'b.safetensors']"
	if got != want {
		t.Fatalf("描述不符\n got: %q\nwant: %q", got, want)
	}
}

// 多个节点报错时按节点 id 数值排序，而不是字符串序（10 不能排在 2 前面）。
func TestDescribeComfyRejectionSortsByNumericNodeID(t *testing.T) {
	body := `{"node_errors":{
		"10":{"class_type":"B","errors":[{"details":"ten"}]},
		"2":{"class_type":"A","errors":[{"details":"two"}]}}}`
	got := describeComfyRejection(strings.NewReader(body))
	want := ": node 2 (A): two; node 10 (B): ten"
	if got != want {
		t.Fatalf("排序不符\n got: %q\nwant: %q", got, want)
	}
}

// 非逐节点错误（invalid_prompt 之类）走 error.message / details。
func TestDescribeComfyRejectionFallsBackToErrorMessage(t *testing.T) {
	body := `{"error":{"type":"invalid_prompt","message":"Cannot execute because a node is missing","details":"Node ID '#9'"}}`
	got := describeComfyRejection(strings.NewReader(body))
	want := ": Cannot execute because a node is missing: Node ID '#9'"
	if got != want {
		t.Fatalf("兜底文案不符\n got: %q\nwant: %q", got, want)
	}
}

// 认不出结构时退回原文——绝不因为解析失败就把信息丢掉。
func TestDescribeComfyRejectionKeepsRawBody(t *testing.T) {
	got := describeComfyRejection(strings.NewReader("upstream exploded"))
	if got != ": upstream exploded" {
		t.Fatalf("原文回退不符: %q", got)
	}
}

// 响应体为空时返回空串，让上层错误信息退化成原来的 "HTTP 400"。
func TestDescribeComfyRejectionEmptyBody(t *testing.T) {
	if got := describeComfyRejection(strings.NewReader("  ")); got != "" {
		t.Fatalf("空响应体应返回空串，实际: %q", got)
	}
}

// 超长详情按 rune 截断且不切断 UTF-8。
func TestDescribeComfyRejectionTruncatesLongDetail(t *testing.T) {
	long := strings.Repeat("模", maxComfyErrorDetail+50)
	got := describeComfyRejection(strings.NewReader(`{"node_errors":{"1":{"class_type":"X","errors":[{"details":"` + long + `"}]}}}`))
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长详情应以省略号结尾: %q", got[len(got)-10:])
	}
	if !strings.Contains(got, "node 1 (X): ") {
		t.Fatalf("前缀应保留: %q", got[:30])
	}
}

// combo 候选解析要同时认两代格式，且不把连线型声明误当成 combo。
func TestComboOptions(t *testing.T) {
	legacy := []any{[]any{"a", "b"}, map[string]any{"default": "a"}}
	if got := comboOptions(legacy); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("旧格式解析失败: %v", got)
	}
	modern := []any{"COMBO", map[string]any{"options": []any{"x", "y"}}}
	if got := comboOptions(modern); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Fatalf("新格式解析失败: %v", got)
	}
	// 连线型声明（首元素是类型名）不是 combo
	if got := comboOptions([]any{"MODEL"}); got != nil {
		t.Fatalf("连线型声明不该被当成 combo: %v", got)
	}
	if got := comboOptions([]any{"INT", map[string]any{"default": 1}}); got != nil {
		t.Fatalf("非 COMBO 标记不该返回候选: %v", got)
	}
}

func TestIsLinkValue(t *testing.T) {
	if !isLinkValue([]any{"8", float64(0)}) {
		t.Fatal("连线值应被识别")
	}
	if isLinkValue("krea2") {
		t.Fatal("字符串不该被当成连线")
	}
	if isLinkValue([]any{"a"}) {
		t.Fatal("长度不为 2 的数组不该被当成连线")
	}
}
