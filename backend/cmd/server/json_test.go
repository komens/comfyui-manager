package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 覆盖 JSON 导入的负面词优先级：逐条 negative 优先、全局 negative 兜底、数组形态不改动。
func TestParseEntriesNegativeResolution(t *testing.T) {
	t.Run("全局兜底填空缺，逐条非空不被覆盖", func(t *testing.T) {
		doc := `{"negative":"GLOBAL","entries":[` +
			`{"id":"a","positive":"pa"},` +
			`{"id":"b","positive":"pb","negative":"OWN"}]}`
		entries, err := parseEntries([]byte(doc))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("期望 2 条，实际 %d", len(entries))
		}
		if entries[0].Negative != "GLOBAL" {
			t.Errorf("空 negative 应由全局兜底为 GLOBAL，实际 %q", entries[0].Negative)
		}
		if entries[1].Negative != "OWN" {
			t.Errorf("逐条 negative 不应被全局覆盖，期望 OWN，实际 %q", entries[1].Negative)
		}
	})

	t.Run("无全局时保持逐条原样", func(t *testing.T) {
		doc := `{"entries":[{"id":"a","positive":"pa","negative":"OWN"},{"id":"b","positive":"pb"}]}`
		entries, err := parseEntries([]byte(doc))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if entries[0].Negative != "OWN" {
			t.Errorf("期望 OWN，实际 %q", entries[0].Negative)
		}
		if entries[1].Negative != "" {
			t.Errorf("无全局兜底时应留空(提交回落工作流)，实际 %q", entries[1].Negative)
		}
	})

	t.Run("全局为纯空白视为缺省", func(t *testing.T) {
		doc := `{"negative":"   ","entries":[{"id":"a","positive":"pa"}]}`
		entries, err := parseEntries([]byte(doc))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if entries[0].Negative != "" {
			t.Errorf("纯空白全局不应写入，实际 %q", entries[0].Negative)
		}
	})

	t.Run("数组形态不受影响", func(t *testing.T) {
		arr := `[{"id":"a","positive":"pa","negative":"OWN"},{"id":"b","positive":"pb"}]`
		entries, err := parseEntries([]byte(arr))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if entries[0].Negative != "OWN" || entries[1].Negative != "" {
			t.Errorf("数组形态应按原样解析，实际 [%q %q]", entries[0].Negative, entries[1].Negative)
		}
	})
}

// 导入返回的 ID 必须对应本次新增的提示词，提交作图会直接使用这些 ID。
func TestImportJSONEntriesWritesRow(t *testing.T) {
	a := newTestApp(t)
	entries, err := parseEntries([]byte(`{"negative":"GLOBAL","entries":[{"id":"a","positive":"pa"}]}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	stats, err := a.importJSONEntries(context.Background(), []byte(`{}`), entries, "测试组")
	if err != nil {
		t.Fatalf("入库失败: %v", err)
	}
	if stats.Inserted != 1 {
		t.Fatalf("Inserted = %d，期望 1（INSERT 失败会静默跳过）", stats.Inserted)
	}
	if len(stats.InsertedPromptIDs) != 1 {
		t.Fatalf("导入返回的 ID：%v，期望只有一条", stats.InsertedPromptIDs)
	}
	var positive, negative, group string
	if err := a.db.QueryRow(`SELECT positive_prompt, negative_prompt, group_name FROM prompts WHERE id=?`, stats.InsertedPromptIDs[0]).Scan(&positive, &negative, &group); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if positive != "pa" || negative != "GLOBAL" || group != "测试组" {
		t.Errorf("落库内容不对：positive=%q negative=%q group=%q", positive, negative, group)
	}
	entries = append(entries, jsonEntry{Positive: "pb"}, jsonEntry{Positive: "pb"})
	next, err := a.importJSONEntries(context.Background(), []byte(`{}`), entries, "测试组")
	if err != nil {
		t.Fatalf("再次导入失败: %v", err)
	}
	if next.Skipped != 2 || next.Inserted != 1 || len(next.InsertedPromptIDs) != 1 {
		t.Fatalf("再次导入统计错误: %+v", next)
	}
	if err := a.db.QueryRow(`SELECT positive_prompt FROM prompts WHERE id=?`, next.InsertedPromptIDs[0]).Scan(&positive); err != nil || positive != "pb" {
		t.Fatalf("返回的 ID 应对应本次新增的 pb，查询得到 %q，错误 %v", positive, err)
	}
}

func TestImportJSONPreservesExistingBackup(t *testing.T) {
	a := newTestApp(t)
	group := "test_20261120_140000"
	for _, content := range []string{`[{"positive":"first"}]`, `[{"positive":"second"}]`} {
		entries, err := parseEntries([]byte(content))
		if err != nil {
			t.Fatal(err)
		}
		stats, err := a.importJSONEntries(context.Background(), []byte(content), entries, group)
		if err != nil || stats.Inserted != 1 || stats.GroupName != group {
			t.Fatalf("import: %+v, %v", stats, err)
		}
	}
	for filename, want := range map[string]string{
		group + ".json":   `[{"positive":"first"}]`,
		group + "_2.json": `[{"positive":"second"}]`,
	} {
		content, err := os.ReadFile(filepath.Join(a.dataDir, "json", filename))
		if err != nil || string(content) != want {
			t.Fatalf("%s = %q, %v", filename, content, err)
		}
	}
}
