package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 一个够用的工作流：两个 CLIPTextEncode（正/负）+ KSampler，落点显式给出后
// mergePromptMapping 不会改写它。
const testWorkflowJSON = `{"nodes":[{"id":3,"type":"KSampler"},{"id":6,"type":"CLIPTextEncode","widgets_values":["a cat"]},{"id":7,"type":"CLIPTextEncode","widgets_values":["blurry"]}],"links":[]}`

const testWorkflowMapping = `{"positive_prompt":{"node_id":"6","field":"text"},"negative_prompt":{"node_id":"7","field":"text"}}`

func writeTestWorkflowFile(t *testing.T, a *app, fileName, content string) {
	t.Helper()
	dir := filepath.Join(a.dataDir, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// zipEntry 从导出包里取一个成员的内容。
func zipEntry(t *testing.T, raw []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("导出包不是合法 ZIP: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", name, err)
		}
		defer rc.Close()
		content, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		return content
	}
	t.Fatalf("导出包里没有 %s", name)
	return nil
}

func multipartZip(t *testing.T, zipBytes []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "export-test.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zipBytes); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func runImport(t *testing.T, a *app, zipBytes []byte) map[string]any {
	t.Helper()
	body, contentType := multipartZip(t, zipBytes)
	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	a.importData(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("导入状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析导入响应失败: %v", err)
	}
	return out
}

// TestExportImportWorkflows 是「导出能不能替代重建工作流」的底线：
// 包里要带 JSON 本体，导进空库要生成一个 mapping 完好、文件名按新 id 重算的工作流。
func TestExportImportWorkflows(t *testing.T) {
	src := newTestApp(t)
	writeTestWorkflowFile(t, src, "wf-1.json", testWorkflowJSON)
	mustExec(t, src, `INSERT INTO workflows(id, name, workflow_path, mapping_json, negative_prompt, enabled, is_default, created_at, updated_at)
		VALUES(1, 'SDXL 基础', 'wf-1.json', ?, 'blurry', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, testWorkflowMapping)

	rec := httptest.NewRecorder()
	src.exportData(rec, httptest.NewRequest(http.MethodGet, "/api/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("导出状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}

	var exported []WorkflowRecord
	if err := json.Unmarshal(zipEntry(t, rec.Body.Bytes(), "workflows.json"), &exported); err != nil {
		t.Fatalf("解析 workflows.json 失败: %v", err)
	}
	if len(exported) != 1 {
		t.Fatalf("导出工作流数 = %d，期望 1", len(exported))
	}
	if exported[0].Name != "SDXL 基础" || !exported[0].Enabled || !exported[0].IsDefault {
		t.Errorf("导出的工作流字段不对：%+v", exported[0])
	}
	if !json.Valid(exported[0].WorkflowJSON) {
		t.Error("workflow_json 不是合法 JSON")
	}

	// 导进一个空库
	dst := newTestApp(t)
	out := runImport(t, dst, rec.Body.Bytes())
	if out["imported_workflows"] != float64(1) {
		t.Fatalf("imported_workflows = %v，期望 1（响应：%v）", out["imported_workflows"], out)
	}

	var id int64
	var name, path, mapping, negative string
	var enabled, isDefault int
	if err := dst.db.QueryRow(`SELECT id, name, workflow_path, mapping_json, COALESCE(negative_prompt,''), enabled, is_default FROM workflows`).
		Scan(&id, &name, &path, &mapping, &negative, &enabled, &isDefault); err != nil {
		t.Fatalf("目标库没有工作流: %v", err)
	}
	if name != "SDXL 基础" {
		t.Errorf("name = %q，期望 SDXL 基础", name)
	}
	// 文件名必须按新 id 重算，否则两份数据会指向同一个文件
	if path != workflowFileName(name, id) {
		t.Errorf("workflow_path = %q，期望 %q", path, workflowFileName(name, id))
	}
	var gotMapping promptMapping
	if err := json.Unmarshal([]byte(mapping), &gotMapping); err != nil {
		t.Fatalf("mapping 不是合法 JSON: %v", err)
	}
	if gotMapping.Positive.NodeID != "6" || gotMapping.Negative.NodeID != "7" {
		t.Errorf("落点没保住：%+v", gotMapping)
	}
	if negative != "blurry" {
		t.Errorf("默认负面 = %q，期望 blurry", negative)
	}
	if enabled != 1 {
		t.Errorf("enabled = %d，期望保留原值 1", enabled)
	}
	// 目标库原本没有默认工作流，但导入的一律不抢默认位：多一个默认会让选工作流的顺序变成实现细节
	if isDefault != 0 {
		t.Errorf("is_default = %d，期望强制 0", isDefault)
	}
	// JSON 本体要真的落盘，且内容与导出的一致
	onDisk, err := os.ReadFile(filepath.Join(dst.dataDir, "workflows", path))
	if err != nil {
		t.Fatalf("工作流文件没落盘: %v", err)
	}
	if workflowContentHash(onDisk) != workflowContentHash([]byte(testWorkflowJSON)) {
		t.Error("落盘的工作流 JSON 与导出的内容不一致")
	}
}

// TestImportWorkflowsDedupe 覆盖重复导入：同名同内容算重复跳过，同名不同内容加后缀共存。
func TestImportWorkflowsDedupe(t *testing.T) {
	src := newTestApp(t)
	writeTestWorkflowFile(t, src, "wf-1.json", testWorkflowJSON)
	mustExec(t, src, `INSERT INTO workflows(id, name, workflow_path, mapping_json, negative_prompt, enabled, created_at, updated_at)
		VALUES(1, 'SDXL 基础', 'wf-1.json', ?, '', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, testWorkflowMapping)

	rec := httptest.NewRecorder()
	src.exportData(rec, httptest.NewRequest(http.MethodGet, "/api/export", nil))

	dst := newTestApp(t)
	if out := runImport(t, dst, rec.Body.Bytes()); out["imported_workflows"] != float64(1) {
		t.Fatalf("首次导入 = %v，期望 1", out["imported_workflows"])
	}

	// 再导一次同一份包：同名同内容 → 跳过
	out := runImport(t, dst, rec.Body.Bytes())
	if out["imported_workflows"] != float64(0) || out["skipped_workflows"] != float64(1) {
		t.Errorf("重复导入应全部跳过，实际 imported=%v skipped=%v", out["imported_workflows"], out["skipped_workflows"])
	}
	var count int
	_ = dst.db.QueryRow(`SELECT COUNT(*) FROM workflows`).Scan(&count)
	if count != 1 {
		t.Errorf("重复导入后工作流数 = %d，期望 1", count)
	}

	// 同名不同内容：不能静默吞掉，应加后缀共存
	changed := []WorkflowRecord{{
		Name:         "SDXL 基础",
		Enabled:      true,
		WorkflowJSON: []byte(`{"nodes":[{"id":1,"type":"KSampler"}],"links":[]}`),
	}}
	tx, err := dst.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	imported, skipped := dst.importWorkflows(tx, changed)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if imported != 1 || skipped != 0 {
		t.Fatalf("同名不同内容 imported=%d skipped=%d，期望 1/0", imported, skipped)
	}
	var renamed string
	_ = dst.db.QueryRow(`SELECT name FROM workflows WHERE name<>?`, "SDXL 基础").Scan(&renamed)
	if renamed != "SDXL 基础 (2)" {
		t.Errorf("同名不同内容的名字 = %q，期望 SDXL 基础 (2)", renamed)
	}
}

// TestImportNormalizesRunningPromptStatus：源库里「生成中」的提示词，导到新库没有对应任务，
// 原样保留会永远转圈。
func TestImportNormalizesRunningPromptStatus(t *testing.T) {
	if got := importedPromptStatus("running"); got != "pending" {
		t.Errorf("running → %q，期望 pending", got)
	}
	if got := importedPromptStatus("completed"); got != "completed" {
		t.Errorf("completed 被改成了 %q", got)
	}
}

// TestBackupIncludesLatestWrites 是备份的正确性底线：库跑在 WAL 模式，
// 直接发主库文件会漏掉还没 checkpoint 的写入。VACUUM INTO 出来的快照必须能看到它。
func TestBackupIncludesLatestWrites(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO prompts(title, positive_prompt, status) VALUES('刚写的', 'p', 'pending')`)

	rec := httptest.NewRecorder()
	a.backupDatabase(rec, httptest.NewRequest(http.MethodGet, "/api/settings/backup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("备份状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}

	snapshotPath := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(snapshotPath, rec.Body.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", snapshotPath)
	if err != nil {
		t.Fatalf("快照打不开: %v", err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow(`SELECT title FROM prompts WHERE positive_prompt='p'`).Scan(&title); err != nil {
		t.Fatalf("快照里查不到刚插入的行（WAL 未合并的写入丢了）: %v", err)
	}
	if title != "刚写的" {
		t.Errorf("title = %q，期望 刚写的", title)
	}
}
