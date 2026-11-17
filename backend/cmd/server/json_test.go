package main

import "testing"

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
