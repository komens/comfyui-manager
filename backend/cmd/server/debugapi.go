package main

// ============================================================================
// 调试产物的查看/下载/清空接口
//
// 只在 DEBUG 开启时注册（见 main.go）。关闭时这些路由根本不存在，
// 请求落到静态文件兜底上返回 404 —— 不构成任何对外开放的面。
//
// 存在的理由：服务跑在云端，日志落在 data 卷里，要从宿主机翻文件并不方便。
// 有了这三个接口，用浏览器或者 curl 就能看和拉，不用 docker exec 进容器。
// ============================================================================

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// debugAllowedFiles 是允许访问的文件白名单（滚动副本 .1 由后缀推导）。
// 白名单而不是「拼路径后判断前缀」：路径拼接一旦写错就是任意文件读取，
// 而这个接口未来也许会被暴露到内网之外。
var debugAllowedFiles = []string{
	"startup.json",
	"events.jsonl",
	"http.jsonl",
	"submit-payloads.jsonl",
}

// validDebugFileName 校验请求的文件名是否是已知调试文件（含滚动副本）。
func validDebugFileName(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return false
	}
	base := strings.TrimSuffix(name, ".1")
	for _, allowed := range debugAllowedFiles {
		if base == allowed {
			return true
		}
	}
	return false
}

// describeDebugFile 列出白名单文件的实际落盘情况（没生成过的显示 bytes=0）。
func describeDebugFile(name string) map[string]any {
	entry := map[string]any{"name": name}
	info, err := os.Stat(filepath.Join(debugDir, name))
	if err != nil {
		entry["bytes"] = 0
		entry["exists"] = false
		return entry
	}
	entry["bytes"] = info.Size()
	entry["exists"] = true
	entry["modified"] = info.ModTime().Format(time.RFC3339)
	entry["truncated"] = info.Size() >= maxDebugFileBytes
	return entry
}

func (a *app) debugStatus(w http.ResponseWriter, r *http.Request) {
	if !debugOn {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	files := make([]map[string]any, 0, len(debugAllowedFiles)*2)
	for _, name := range debugAllowedFiles {
		files = append(files, describeDebugFile(name))
		// 滚动副本一并列出来，否则出问题时最新的一代被滚走了会以为日志丢了
		if _, err := os.Stat(filepath.Join(debugDir, name+".1")); err == nil {
			files = append(files, describeDebugFile(name+".1"))
		}
	}
	abs, err := filepath.Abs(debugDir)
	if err != nil {
		abs = debugDir
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"on":    true,
		"dir":   abs,
		"files": files,
	})
}

func (a *app) debugDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !debugOn || !validDebugFileName(name) {
		writeError(w, http.StatusNotFound, "unknown debug file")
		return
	}
	path := filepath.Join(debugDir, name)
	if !fileExists(path) {
		writeError(w, http.StatusNotFound, "debug file not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	http.ServeFile(w, r, path)
}

// debugClear 删掉某个调试文件（下次写入会自动重建）。
// 轮转副本一起删：留着一代旧日志只会让人分不清看的是哪一份。
func (a *app) debugClear(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !debugOn || !validDebugFileName(name) {
		writeError(w, http.StatusNotFound, "unknown debug file")
		return
	}
	base := strings.TrimSuffix(name, ".1")
	removed := make([]string, 0, 2)
	for _, candidate := range []string{base, base + ".1"} {
		err := os.Remove(filepath.Join(debugDir, candidate))
		if err == nil {
			removed = append(removed, candidate)
			continue
		}
		if !os.IsNotExist(err) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": removed})
}
