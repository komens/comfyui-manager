package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func testPNG(t *testing.T, width int) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, width, 1))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestImportImagesSkipsMissingAndKeepsDifferentSameNameImages(t *testing.T) {
	a := newTestApp(t)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	first, second := testPNG(t, 1), testPNG(t, 2)
	for _, entry := range []struct {
		name string
		body []byte
	}{
		{"images.json", []byte("[{\"filename\":\"sample.png\",\"archive_name\":\"first.png\"},{\"filename\":\"sample.png\",\"archive_name\":\"second.png\"},{\"filename\":\"missing.png\"}]")},
		{"images/first.png", first},
		{"images/second.png", second},
	} {
		file, err := writer.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	result := runImport(t, a, archive.Bytes())
	reasons := result["skipped_image_reasons"].(map[string]any)
	if result["imported_images"] != float64(2) || reasons["missing"] != float64(1) {
		t.Fatalf("unexpected import result: %#v", result)
	}
	for index, expected := range [][]byte{first, second} {
		var path string
		if err := a.db.QueryRow("SELECT storage_path FROM images WHERE id=? AND filename='sample.png'", index+1).Scan(&path); err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("image %d: unexpected stored bytes, error=%v", index+1, err)
		}
	}

	result = runImport(t, a, archive.Bytes())
	reasons = result["skipped_image_reasons"].(map[string]any)
	if result["imported_images"] != float64(0) || reasons["duplicate"] != float64(2) {
		t.Fatalf("unexpected repeat import result: %#v", result)
	}
}

func TestTaskCreationChecksWorkflowForAllEntryPoints(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, "INSERT INTO prompts(id, positive_prompt, group_name) VALUES(1,'test','review')")
	mustExec(t, a, "INSERT INTO workflows(id, name, workflow_path, mapping_json, enabled, created_at, updated_at) VALUES(1,'Workflow','test.json','{}',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)")
	disableReq := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"enabled":false}`))
	disableReq.SetPathValue("id", "1")
	disableRec := httptest.NewRecorder()
	a.setWorkflowEnabled(disableRec, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable workflow: %d %s", disableRec.Code, disableRec.Body)
	}

	for _, entry := range []struct {
		name    string
		handler http.HandlerFunc
		body    string
	}{
		{"direct", a.createDirectTask, "{\"positive_prompt\":\"test\",\"workflow_id\":1}"},
		{"single", a.runPrompt, "{\"workflow_id\":1}"},
		{"batch", a.batchRunPrompts, "{\"ids\":[1],\"workflow_id\":1}"},
		{"group", a.groupRunPrompts, "{\"group_name\":\"review\",\"workflow_id\":1}"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			for _, body := range []string{entry.body, strings.Replace(entry.body, "\"workflow_id\":1", "\"workflow_id\":99", 1)} {
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				req.SetPathValue("id", "1")
				rec := httptest.NewRecorder()
				entry.handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
				}
			}
		})
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM generation_tasks").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected tasks=%d error=%v", count, err)
	}

	enableReq := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"enabled":true}`))
	enableReq.SetPathValue("id", "1")
	enableRec := httptest.NewRecorder()
	a.setWorkflowEnabled(enableRec, enableReq)
	if enableRec.Code != http.StatusOK {
		t.Fatalf("enable workflow: %d %s", enableRec.Code, enableRec.Body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/prompts/group-run", strings.NewReader("{\"group_name\":\"review\",\"workflow_id\":1}"))
	rec := httptest.NewRecorder()
	a.groupRunPrompts(rec, req)
	var result struct{ Created int }
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK || result.Created != 1 {
		t.Fatalf("status=%d body=%s error=%v", rec.Code, rec.Body, err)
	}
	var name string
	if err := a.db.QueryRow("SELECT workflow_name FROM generation_tasks").Scan(&name); err != nil || name != "Workflow" {
		t.Fatalf("workflow_name=%q error=%v", name, err)
	}
}

func TestOutputSaveRetriesOnlyMissingImages(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, "INSERT INTO generation_tasks(id, source_type, workflow_id, comfyui_url, parameters_json, status, created_at) VALUES(1,'direct',1,'','{}','running',CURRENT_TIMESTAMP)")
	mustExec(t, a, "INSERT INTO generation_items(id, task_id, positive_prompt, comfy_prompt_id, status) VALUES(1,1,'test','mock-prompt','submitted')")

	pngData := testPNG(t, 1)
	var firstDownloads, secondDownloads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /history/mock-prompt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{\"mock-prompt\":{\"outputs\":{\"save\":{\"images\":[{\"filename\":\"first.png\"},{\"filename\":\"second.png\"}]}}}}"))
	})
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("filename") {
		case "first.png":
			firstDownloads.Add(1)
		case "second.png":
			if secondDownloads.Add(1) == 1 {
				http.Error(w, "temporary image server error", http.StatusServiceUnavailable)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(pngData)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	for _, want := range []struct {
		status string
		saved  int
	}{{"submitted", 1}, {"success", 2}} {
		a.checkAndDownload(1, 1, "mock-prompt", server.URL, &comfyQueueCache{})
		var status string
		var saved, total, records int
		if err := a.db.QueryRow("SELECT status, saved_output_count, output_count, (SELECT COUNT(*) FROM images) FROM generation_items WHERE id=1").Scan(&status, &saved, &total, &records); err != nil {
			t.Fatal(err)
		}
		if status != want.status || saved != want.saved || records != want.saved || total != 2 {
			t.Fatalf("status=%q saved=%d total=%d records=%d, want %+v", status, saved, total, records, want)
		}
	}
	if firstDownloads.Load() != 1 || secondDownloads.Load() != 2 {
		t.Fatalf("unexpected downloads: first=%d second=%d", firstDownloads.Load(), secondDownloads.Load())
	}
}

// TestRetryKeepsExistingImages 钉住「重试不删图」这条约定：重试可以换工作流，
// 换了就等于重新生成另一批图，上一版的图属于用户资产，重试不承担清理职责。
// 图片只在图库手动删除或整理页清理，收藏与否都一样。
func TestRetryKeepsExistingImages(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO workflows(id, name, workflow_path, mapping_json, created_at, updated_at)
		VALUES(1,'wf','wf.json','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_tasks(id, source_type, workflow_id, comfyui_url, parameters_json, status, created_at)
		VALUES(1,'direct',1,'http://127.0.0.1:8188','{}','failed',CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_items(id, task_id, positive_prompt, comfy_prompt_id, status, output_count, saved_output_count)
		VALUES(1,1,'p','old-prompt','failed',2,1)`)

	for path, fav := range map[string]bool{"images/first.png": false, "images/favorite.png": true} {
		if err := os.WriteFile(filepath.Join(a.dataDir, path), testPNG(t, 4), 0o644); err != nil {
			t.Fatal(err)
		}
		mustExec(t, a, `INSERT INTO images(generation_item_id, filename, storage_path, is_favorite, created_at)
			VALUES(1,?,?,?,'2026-11-17 10:00:00')`, filepath.Base(path), path, fav)
	}

	if err := a.retryTaskByID(context.Background(), 1); err != nil {
		t.Fatalf("重试失败: %v", err)
	}

	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM images WHERE generation_item_id=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("重试后图片行数 = %d，期望 2（重试不删图）", n)
	}
	for _, name := range []string{"first.png", "favorite.png"} {
		if _, err := os.Stat(filepath.Join(a.dataDir, "images", name)); err != nil {
			t.Errorf("重试不应删除原图 %s: %v", name, err)
		}
	}
	var status, promptID string
	if err := a.db.QueryRow(`SELECT status, comfy_prompt_id FROM generation_items WHERE id=1`).Scan(&status, &promptID); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || promptID != "" {
		t.Errorf("重试后 item = (%q,%q)，期望 (pending,\"\")", status, promptID)
	}
}

// TestDeleteTaskRemovesImageFilesKeepsFavorites 盯的是「删任务只删库行、文件留在盘上」。
// 非收藏图必须行+原图+缩略图一起清；收藏图是用户在图库里明确留的，行、原图、缩略图都得在。
func TestDeleteTaskRemovesImageFilesKeepsFavorites(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO workflows(id, name, workflow_path, mapping_json, created_at, updated_at)
		VALUES(1,'wf','wf.json','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_tasks(id, source_type, workflow_id, comfyui_url, parameters_json, status, created_at)
		VALUES(1,'direct',1,'http://127.0.0.1:8188','{}','completed',CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_items(id, task_id, positive_prompt, comfy_prompt_id, status)
		VALUES(1,1,'p','cup','success')`)

	ids := map[string]int64{"images/plain.png": 1, "images/favorite.png": 2}
	for path, id := range ids {
		if err := os.WriteFile(filepath.Join(a.dataDir, path), testPNG(t, 4), 0o644); err != nil {
			t.Fatal(err)
		}
		fav := id == 2
		mustExec(t, a, `INSERT INTO images(id, generation_item_id, filename, storage_path, is_favorite, created_at)
			VALUES(?,1,?,?,?,'2026-11-17 10:00:00')`, id, filepath.Base(path), path, fav)
		// 缩略图落盘，才能验证 removeThumbs 真的被调到了
		for _, spec := range allThumbSpecs {
			thumb := a.thumbFile(id, spec)
			if err := os.MkdirAll(filepath.Dir(thumb), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(thumb, []byte("thumb"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := a.deleteTaskByID(context.Background(), 1); err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}

	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM images WHERE generation_item_id=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("剩余图片行 = %d，期望 1（只剩收藏的那张）", n)
	}
	if _, err := os.Stat(filepath.Join(a.dataDir, "images", "plain.png")); !os.IsNotExist(err) {
		t.Errorf("非收藏图的原文件应被删除，stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.dataDir, "images", "favorite.png")); err != nil {
		t.Errorf("收藏图的原文件应保留: %v", err)
	}
	for _, spec := range allThumbSpecs {
		if _, err := os.Stat(a.thumbFile(1, spec)); !os.IsNotExist(err) {
			t.Errorf("非收藏图的缩略图应被删除: %v", err)
			break
		}
		if _, err := os.Stat(a.thumbFile(2, spec)); err != nil {
			t.Errorf("收藏图的缩略图应保留: %v", err)
			break
		}
	}
	var items, tasks int
	a.db.QueryRow(`SELECT COUNT(*) FROM generation_items WHERE task_id=1`).Scan(&items)
	a.db.QueryRow(`SELECT COUNT(*) FROM generation_tasks WHERE id=1`).Scan(&tasks)
	if items != 0 || tasks != 0 {
		t.Errorf("任务与生成项应被删除，实际 items=%d tasks=%d", items, tasks)
	}
}

func TestQueueUsesCapturedWorkflowAndTarget(t *testing.T) {
	a := newTestApp(t)
	submissions := make(chan map[string]any, 1)
	oldTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var submitted struct{ Prompt map[string]any }
		if r.URL.Path != "/prompt" || json.NewDecoder(r.Body).Decode(&submitted) != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		submissions <- submitted.Prompt
		_, _ = w.Write([]byte(`{"prompt_id":"queued-prompt"}`))
	}))
	defer oldTarget.Close()
	mustExec(t, a, "UPDATE settings SET value=? WHERE key='comfyui_url'", oldTarget.URL)

	original := `{"1":{"class_type":"CLIPTextEncode","inputs":{"text":"old-positive"}},
		"2":{"class_type":"CLIPTextEncode","inputs":{"text":"old-negative"}},
		"3":{"class_type":"KSampler","inputs":{"positive":["1",0],"negative":["2",0],"steps":20}}}`
	if err := writeAtomic(a.workflowFilePath("workflow.json"), []byte(original)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, `INSERT INTO workflows(id,name,workflow_path,mapping_json,created_at,updated_at)
		VALUES(1,'workflow','workflow.json','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	rec := httptest.NewRecorder()
	a.createDirectTask(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"workflow_id":1,"positive_prompt":"new-positive"}`)))
	if rec.Code != 202 {
		t.Fatalf("queue: %d %s", rec.Code, rec.Body)
	}

	mustExec(t, a, "UPDATE settings SET value='http://127.0.0.1:1' WHERE key='comfyui_url'")
	updated := strings.ReplaceAll(original, "old-negative", "edited-negative")
	req := httptest.NewRequest("PUT", "/", strings.NewReader(`{"negative_prompt":"edited-negative","workflow_json":`+updated+`}`))
	req.SetPathValue("id", "1")
	rec = httptest.NewRecorder()
	a.updateWorkflow(rec, req)
	if rec.Code != 200 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	a.submitPendingItems()
	var storedURL, negative, status string
	if err := a.db.QueryRow("SELECT submitted_comfyui_url,effective_negative_prompt,status FROM generation_items").
		Scan(&storedURL, &negative, &status); err != nil {
		t.Fatal(err)
	}
	if storedURL != oldTarget.URL || negative != "old-negative" || status != "submitted" {
		t.Fatalf("unexpected submission: %q %q %q", storedURL, negative, status)
	}
	submitted := <-submissions
	if got := inputsOf(t, submitted, "1")["text"]; got != "new-positive" {
		t.Fatalf("positive prompt = %v", got)
	}
	if got := inputsOf(t, submitted, "2")["text"]; got != "old-negative" {
		t.Fatalf("negative prompt = %v", got)
	}
}

func TestWorkflowUpdateFailureKeepsCurrentRevision(t *testing.T) {
	a := newTestApp(t)
	original := []byte(`{"1":{"class_type":"Old","inputs":{}}}`)
	path := a.workflowFilePath("test-1.json")
	if err := writeAtomic(path, original); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, `INSERT INTO workflows(id,name,workflow_path,mapping_json,created_at,updated_at)
		VALUES(1,'test','test-1.json','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	mustExec(t, a, `CREATE TRIGGER reject_workflow_update BEFORE UPDATE ON workflows
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`)
	req := httptest.NewRequest("PUT", "/", strings.NewReader(`{"workflow_json":{"2":{"class_type":"New","inputs":{}}}}`))
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	a.updateWorkflow(rec, req)
	content, err := os.ReadFile(path)
	if err != nil || rec.Code != 500 || !bytes.Equal(content, original) {
		t.Fatalf("status=%d, file=%s, error=%v", rec.Code, content, err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected revision files: %v, %v", files, err)
	}
}

func TestRetryCompletedTaskOnlyResetsFailures(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO workflows(id,name,workflow_path,mapping_json,created_at,updated_at)
		VALUES(1,'test','test-1.json','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_tasks(id,source_type,workflow_id,comfyui_url,parameters_json,status,total_count,created_at)
		VALUES(1,'direct',1,'http://localhost:8188','{}','running',2,CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_items(id,task_id,positive_prompt,status)
		VALUES(1,1,'p','success'),(2,1,'p','running')`)
	if changed, err := a.persistItemOutcome(2, 1, "failed", "test"); err != nil || !changed {
		t.Fatalf("cannot record outcome: %v", err)
	}
	if err := a.retryTaskByID(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	var task, success, failure string
	err := a.db.QueryRow(`SELECT t.status,s.status,f.status FROM generation_tasks t
		JOIN generation_items s ON s.id=1 JOIN generation_items f ON f.id=2 WHERE t.id=1`).Scan(&task, &success, &failure)
	if err != nil || task != "pending" || success != "success" || failure != "pending" {
		t.Fatalf("task=%q, success=%q, failure=%q, err=%v", task, success, failure, err)
	}
}

func TestDeletePendingTaskClearsPromptRunning(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO workflows(id,name,workflow_path,mapping_json,created_at,updated_at)
		VALUES(1,'workflow','test-1.json','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	rec := httptest.NewRecorder()
	a.createDirectTask(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"workflow_id":1,"positive_prompt":"test"}`)))
	if rec.Code != 202 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if err := a.deleteTaskByID(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := a.db.QueryRow("SELECT status FROM prompts WHERE id=1").Scan(&status); err != nil || status != "pending" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestRemoteCancellationScopesJobAndHandlesLegacyVersions(t *testing.T) {
	for _, test := range []struct {
		name, version         string
		scoped, wantInterrupt bool
	}{
		{name: "job endpoint", scoped: true},
		{name: "targeted interrupt", version: "0.3.60", wantInterrupt: true},
		{name: "older global interrupt", version: "0.3.50"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var interrupted atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "POST /api/jobs/target/cancel":
					if !test.scoped {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write([]byte(`{"cancelled":true}`))
				case "POST /queue":
					var body struct{ Delete []string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Delete) != 1 || body.Delete[0] != "target" {
						t.Errorf("unexpected queue mutation %+v, %v", body, err)
					}
				case "GET /queue":
					_, _ = w.Write([]byte(`{"queue_running":[[1,"target"]],"queue_pending":[]}`))
				case "GET /system_stats":
					_ = json.NewEncoder(w).Encode(map[string]any{"system": map[string]string{"comfyui_version": test.version}})
				case "POST /interrupt":
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["prompt_id"] != "target" {
						t.Errorf("unexpected interrupt %+v, %v", body, err)
					}
					interrupted.Store(true)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			err := cancelComfyJob(context.Background(), server.URL, "target")
			if (err == nil) != (test.scoped || test.wantInterrupt) || interrupted.Load() != test.wantInterrupt {
				t.Fatalf("interrupt=%v, error=%v", interrupted.Load(), err)
			}
		})
	}
}

func TestHistoryConnectionErrorClearsWhenQueueReturns(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO generation_tasks(id,source_type,workflow_id,comfyui_url,parameters_json,status,created_at)
		VALUES(1,'direct',1,'http://localhost:8188','{}','running',CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_items(id,task_id,positive_prompt,comfy_prompt_id,status)
		VALUES(1,1,'p','target','submitted')`)
	var connected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !connected.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path == "/history/target" {
			_, _ = w.Write([]byte(`{}`))
		} else {
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[[1,"target"]]}`))
		}
	}))
	defer server.Close()
	for _, isConnected := range []bool{false, true} {
		connected.Store(isConnected)
		a.checkAndDownload(1, 1, "target", server.URL, &comfyQueueCache{})
		var since int64
		var message, status string
		err := a.db.QueryRow("SELECT remote_failure_since,error_message,status FROM generation_items WHERE id=1").Scan(&since, &message, &status)
		if err != nil || status != "submitted" || (message == "") != isConnected || (since == 0) != isConnected {
			t.Fatalf("connected=%v, since=%d, message=%q, status=%q, err=%v", isConnected, since, message, status, err)
		}
	}
}

func TestWorkflowEnableAndDefaultState(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO workflows(id,name,workflow_path,mapping_json,enabled,is_default,created_at,updated_at)
		VALUES(1,'original','one.json','{}',1,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
		      (2,'other','two.json','{}',1,0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	call := func(handler http.HandlerFunc, id, body string, expected int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != expected {
			t.Fatalf("workflow %s, request %s: got %d, expected %d (%s)", id, body, rec.Code, expected, rec.Body)
		}
	}
	check := func(id int, wantEnabled, wantDefault bool) {
		t.Helper()
		var enabled, isDefault bool
		if err := a.db.QueryRow("SELECT enabled,is_default FROM workflows WHERE id=?", id).Scan(&enabled, &isDefault); err != nil || enabled != wantEnabled || isDefault != wantDefault {
			t.Fatalf("workflow %d: enabled=%v default=%v error=%v", id, enabled, isDefault, err)
		}
	}

	call(a.setWorkflowEnabled, "1", `{}`, 400)
	call(a.setWorkflowEnabled, "99", `{"enabled":false}`, 404)
	call(a.setDefaultWorkflow, "2", `{"is_default":false}`, 200)
	check(1, true, true)
	call(a.setWorkflowEnabled, "1", `{"enabled":false}`, 200)
	check(1, false, false)
	call(a.setDefaultWorkflow, "2", `{"is_default":true}`, 200)
	call(a.setDefaultWorkflow, "1", `{"is_default":true}`, 409)
	check(2, true, true)
	call(a.setWorkflowEnabled, "1", `{"enabled":true}`, 200)
	check(1, true, false)
	check(2, true, true)
}

func TestWorkflowListFiltersBeforePagination(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO workflows(id,name,workflow_path,mapping_json,enabled,is_default,created_at,updated_at)
		VALUES(1,'Portrait A','one.json','{}',1,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
			(2,'Portrait B','two.json','{}',0,0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
			(3,'Landscape 100%','three.json','{}',1,0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	for _, test := range []struct {
		query     string
		total, id int
	}{
		{"q=portrait&page_size=1", 2, 2},
		{"q=portrait&status=enabled", 1, 1},
		{"status=disabled", 1, 2},
		{"status=default", 1, 1},
		{"q=%25", 1, 3},
	} {
		rec := httptest.NewRecorder()
		a.listWorkflows(rec, httptest.NewRequest("GET", "/api/workflows?"+test.query, nil))
		var result struct {
			Total int
			Items []struct{ ID int }
		}
		err := json.Unmarshal(rec.Body.Bytes(), &result)
		if rec.Code != 200 || err != nil || result.Total != test.total || len(result.Items) != 1 || result.Items[0].ID != test.id {
			t.Errorf("%s: status=%d body=%s err=%v", test.query, rec.Code, rec.Body, err)
		}
	}
}

func TestTaskListIncludesRecentFailureAndConnectionReason(t *testing.T) {
	a := newTestApp(t)
	mustExec(t, a, `INSERT INTO generation_tasks(id,source_type,workflow_id,comfyui_url,parameters_json,status,created_at)
		VALUES(1,'prompt',1,'http://localhost:8188','{}','completed',CURRENT_TIMESTAMP),
			(2,'direct',1,'http://localhost:8188','{}','running',CURRENT_TIMESTAMP)`)
	mustExec(t, a, `INSERT INTO generation_items(id,task_id,positive_prompt,status,error_message,remote_failure_since)
		VALUES(1,1,'p','failed','missing model',0),
			(2,2,'p','submitted','connection lost',1),
			(3,2,'p','failed','older failure',0)`)
	mustExec(t, a, `INSERT INTO images(id,generation_item_id,filename,storage_path,created_at)
		VALUES(11,2,'cover.png','cover.png',CURRENT_TIMESTAMP),
			(12,3,'second.png','second.png',CURRENT_TIMESTAMP)`)
	rec := httptest.NewRecorder()
	a.listTasks(rec, httptest.NewRequest("GET", "/api/tasks", nil))
	var result struct {
		Items []struct {
			ID         int
			Reason     string `json:"failure_reason"`
			Connection bool   `json:"connection_issue"`
			ImageID    int    `json:"image_id"`
			ImageCount int    `json:"image_count"`
		}
	}
	err := json.Unmarshal(rec.Body.Bytes(), &result)
	if rec.Code != 200 || err != nil || len(result.Items) != 2 {
		t.Fatalf("status=%d body=%s err=%v", rec.Code, rec.Body, err)
	}
	if result.Items[0].ID != 2 || !result.Items[0].Connection || result.Items[0].Reason != "connection lost" ||
		result.Items[0].ImageID != 11 || result.Items[0].ImageCount != 2 || result.Items[1].ImageCount != 0 || result.Items[1].Reason != "missing model" {
		t.Fatalf("unexpected task summaries: %+v", result.Items)
	}
}
