package main

// ============================================================================
// 【临时调试代码 — 用完即删】
// 把真正发给 ComfyUI POST /prompt 的请求体按行落盘，便于核对“实际提交了什么数据作图”。
//
// 移除步骤（共 3 处，无其它耦合）：
//   1. 删除本文件
//   2. 删掉 comfyui.go 的 submitComfy 里那行 dumpSubmitPayload(...)
//   3. 删掉 main.go 里那行 initSubmitDump(...)
//
// 开关：环境变量 SUBMIT_DUMP_FILE
//   - 不设置            → 默认写到 <dataDir>/debug/submit-payloads.jsonl（默认开启）
//   - off / 0 / false   → 关闭
//   - 其它任意值        → 当作文件路径（相对路径相对于启动目录）
//
// 输出格式：JSONL，每次提交一行，payload 字段即原样发给 ComfyUI 的请求体
// （形如 {"prompt":{...节点图...},"client_id":"comfyui-server-task-<taskID>"}）。
// task_id 需要时从 client_id 里取；写盘发生在请求发出之前，因此提交失败也能看到载荷。
// ============================================================================

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	submitDumpPath string
	submitDumpMu   sync.Mutex
)

type submitDumpRecord struct {
	Time     string          `json:"time"`
	URL      string          `json:"url"`
	ClientID string          `json:"client_id"`
	Payload  json.RawMessage `json:"payload"`
}

// initSubmitDump 解析开关并准备目录。放在 main 里、app 建好之后调用。
func initSubmitDump(dataDir string, logger *log.Logger) {
	value := strings.TrimSpace(os.Getenv("SUBMIT_DUMP_FILE"))
	switch strings.ToLower(value) {
	case "off", "0", "false", "no":
		logger.Printf("submit payload dump: disabled (SUBMIT_DUMP_FILE=%s)", value)
		return
	case "":
		submitDumpPath = filepath.Join(dataDir, "debug", "submit-payloads.jsonl")
	default:
		submitDumpPath = value
	}
	if err := os.MkdirAll(filepath.Dir(submitDumpPath), 0o755); err != nil {
		logger.Printf("submit payload dump: create dir failed: %v — dump disabled", err)
		submitDumpPath = ""
		return
	}
	logger.Printf("submit payload dump: enabled -> %s", submitDumpPath)
}

// dumpSubmitPayload 追加一行 JSONL。任何失败都只吞掉，绝不影响正常提交。
func dumpSubmitPayload(url, clientID string, payload []byte) {
	if submitDumpPath == "" {
		return
	}
	line, err := json.Marshal(submitDumpRecord{
		Time:     time.Now().Format(time.RFC3339Nano),
		URL:      url,
		ClientID: clientID,
		Payload:  json.RawMessage(payload),
	})
	if err != nil {
		return
	}
	submitDumpMu.Lock()
	defer submitDumpMu.Unlock()
	file, err := os.OpenFile(submitDumpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}
