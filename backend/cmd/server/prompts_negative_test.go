package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUpdatePromptNegativeThreeState 锁住 PUT /api/prompts/{id} 的三态语义：
// 不传字段 = 保留原值，传空串 = 清空，传非空 = 改写。
//
// 这条最容易被「顺手简化」写成 input.NegativePrompt 直接赋值——那样空串会退化成
// 「保留原值」，前端把负面框清空再保存就静默失效（看着保存成功，实际没清掉）。
// 同一套指针约定见 workflows.go 的 strOrFallback。
func TestUpdatePromptNegativeThreeState(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO prompts(id, title, positive_prompt, negative_prompt, group_name, group_id, status)
		VALUES(1, 't', 'p', 'orig', 'g', 'g', 'pending')`)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"不传该字段 -> 保留原值", `{"title":"t","positive_prompt":"p"}`, "orig"},
		{"传空串 -> 清空", `{"title":"t","positive_prompt":"p","negative_prompt":""}`, ""},
		{"传非空 -> 改写", `{"title":"t","positive_prompt":"p","negative_prompt":"new"}`, "new"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 每轮复位，避免前一个子用例的结果影响后一个
			mustExec(t, a, `UPDATE prompts SET negative_prompt='orig' WHERE id=1`)

			req := httptest.NewRequest(http.MethodPut, "/api/prompts/1", strings.NewReader(c.body))
			req.SetPathValue("id", "1")
			rec := httptest.NewRecorder()
			a.updatePrompt(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
			}

			var got string
			if err := a.db.QueryRow(`SELECT negative_prompt FROM prompts WHERE id=1`).Scan(&got); err != nil {
				t.Fatalf("读取负面失败: %v", err)
			}
			if got != c.want {
				t.Errorf("negative_prompt = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestCreatePromptNegativeDefaultsEmpty 确认新建时不传负面落空串（= 跟随工作流），
// 而不是落成 NULL —— 提交侧的 COALESCE / NULLIF 都按「空串 = 未设置」判断。
func TestCreatePromptNegativeDefaultsEmpty(t *testing.T) {
	a := newTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/api/prompts", strings.NewReader(`{"title":"t","positive_prompt":"p"}`))
	rec := httptest.NewRecorder()
	a.createPrompt(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201，body=%s", rec.Code, rec.Body.String())
	}

	var neg string
	var isNull int
	if err := a.db.QueryRow(`SELECT COALESCE(negative_prompt,'<NULL>'), negative_prompt IS NULL FROM prompts ORDER BY id DESC LIMIT 1`).Scan(&neg, &isNull); err != nil {
		t.Fatalf("读取负面失败: %v", err)
	}
	if isNull == 1 {
		t.Error("negative_prompt 落成了 NULL，应当为空串")
	}
	if neg != "" {
		t.Errorf("negative_prompt = %q，期望空串", neg)
	}
}

// TestGetImageNegativeFollowsPromptLikePositive 是外部平台同步图的回归：
// 这类图没有 generation_item（generation_item_id=0），只有 images.prompt_id。
// 正向早就走「提示词优先」，负向曾经跳过提示词那一级 → 同一张图「正向有、负向空」，
// 灯箱里看像是负面丢了。两边必须同序回落。
func TestGetImageNegativeFollowsPromptLikePositive(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO prompts(id, title, positive_prompt, negative_prompt, status)
		VALUES(1, 't', 'pos', 'from-prompt', 'completed')`)
	mustExec(t, a, `INSERT INTO images(id, generation_item_id, prompt_id, source_platform, filename, storage_path, created_at)
		VALUES(1, 0, 1, 'chatgpt', 'a.png', 'images/a.png', '2026-11-17 10:00:00')`)

	req := httptest.NewRequest(http.MethodGet, "/api/images/1", nil)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	a.getImage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		PositivePrompt string `json:"positive_prompt"`
		NegativePrompt string `json:"negative_prompt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.PositivePrompt != "pos" {
		t.Errorf("positive_prompt = %q，期望 pos", got.PositivePrompt)
	}
	if got.NegativePrompt != "from-prompt" {
		t.Errorf("negative_prompt = %q，期望 from-prompt（应取所属提示词的负面）", got.NegativePrompt)
	}
}

// TestTaskItemNegativeTrimsBlank：item 负面是纯空白时，提交侧会回落到工作流负面，
// 列表回显也必须当作「没写」——否则任务详情会出现一个空的「负面」块。
func TestTaskItemNegativeTrimsBlank(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO workflows(id, name, workflow_path, mapping_json, negative_prompt, created_at, updated_at)
		VALUES(1, 'wf', 'wf.json', '{}', 'wf-neg', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_tasks(id, source_type, workflow_id, comfyui_url, parameters_json, created_at)
		VALUES(1, 'direct', 1, 'http://127.0.0.1:8188', '{}', CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_items(id, task_id, positive_prompt, negative_prompt, status)
		VALUES(1, 1, 'p', '   ', 'pending')`)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/1", nil)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	a.getTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []struct {
			NegativePrompt string `json:"negative_prompt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items 长度 = %d，期望 1", len(got.Items))
	}
	if got.Items[0].NegativePrompt != "wf-neg" {
		t.Errorf("negative_prompt = %q，期望回落到工作流负面 wf-neg", got.Items[0].NegativePrompt)
	}
}
