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
	mustExec(t, a, "INSERT INTO workflows(id, name, workflow_path, mapping_json, enabled, created_at, updated_at) VALUES(1,'Disabled','test.json','{}',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)")

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

	mustExec(t, a, "UPDATE workflows SET enabled=1 WHERE id=1")
	req := httptest.NewRequest(http.MethodPost, "/api/prompts/group-run", strings.NewReader("{\"group_name\":\"review\",\"workflow_id\":1}"))
	rec := httptest.NewRecorder()
	a.groupRunPrompts(rec, req)
	var result struct{ Created int }
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK || result.Created != 1 {
		t.Fatalf("status=%d body=%s error=%v", rec.Code, rec.Body, err)
	}
	var name string
	if err := a.db.QueryRow("SELECT workflow_name FROM generation_tasks").Scan(&name); err != nil || name != "Disabled" {
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
		a.checkAndDownload(1, 1, "mock-prompt", server.URL, make(map[string]comfyQueueSnapshot))
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
