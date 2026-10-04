package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalizeWorkflowJSON 覆盖 workflow_json 的两种送法：
// 前端给对象，脚本/别家工具给「把文件原文当字符串」的形态。后者多出的那层引号
// 如果原样落盘，工作流此后永远提交不了，而报错信息完全指不到根因。
func TestNormalizeWorkflowJSON(t *testing.T) {
	workflow := `{"nodes": [{"id": 1, "type": "KSampler"}]}`

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"对象原样通过", workflow, workflow},
		{"字符串包一层", strconvQuote(workflow), workflow},
		{"字符串包两层", strconvQuote(strconvQuote(workflow)), workflow},
		{"前后有空白", "  \n" + strconvQuote(workflow) + "\n ", workflow},
		{"字符串里不是 JSON", `"hello"`, `"hello"`},
		{"字符串里的 JSON 不合法", `"{\"nodes\": "`, `"{\"nodes\": "`},
		{"数字原样", `42`, `42`},
		{"空", ``, ``},
	}
	for _, tc := range cases {
		got := string(normalizeWorkflowJSON(json.RawMessage(tc.in)))
		if got != tc.want {
			t.Errorf("%s：期望 %s，实际 %s", tc.name, tc.want, got)
		}
	}
}

// strconvQuote 用 JSON 规则把一段文本编码成字符串字面量（含引号）。
func strconvQuote(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}

// TestDecodeWorkflowBodyRejectsNonObject 非对象一律 400：不能让它落盘成一个
// 每次提交都失败、却看不出原因的文件。
func TestDecodeWorkflowBodyRejectsNonObject(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
		wantOK  bool
	}{
		{"对象", `{"nodes":[]}`, "", true},
		{"字符串包的对象", strconvQuote(`{"nodes":[]}`), "", true},
		{"裸字符串", `"hello"`, "must be a JSON object", false},
		{"数组", `[1,2]`, "must be a JSON object", false},
		{"坏 JSON", `{`, "must be valid JSON", false},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		out, ok := decodeWorkflowBody(recorder, json.RawMessage(tc.raw), "workflow_json")
		if ok != tc.wantOK {
			t.Errorf("%s：ok 应为 %v，实际 %v（响应 %s）", tc.name, tc.wantOK, ok, recorder.Body.String())
			continue
		}
		if tc.wantOK {
			if !json.Valid(out) {
				t.Errorf("%s：归一化结果不是合法 JSON：%s", tc.name, out)
			}
			continue
		}
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s：应返回 400，实际 %d", tc.name, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), tc.wantSub) {
			t.Errorf("%s：响应里应说明 %q，实际 %s", tc.name, tc.wantSub, recorder.Body.String())
		}
	}
}
