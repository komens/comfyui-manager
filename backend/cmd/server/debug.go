package main

// ============================================================================
// Debug 模式（总开关）
// 环境变量 DEBUG 控制，默认关闭。后续新增的调试能力统一挂在这个开关下，
// 不再各自发明环境变量。
//
// DEBUG 取值：
//   - 不设置 / 空 / off / 0 / false / no → 关闭
//   - 1 / true / on / yes               → 开启，各调试输出用默认路径
//   - 其它任意值                         → 开启，且值作为「提交载荷落盘」的文件路径
//     （相对路径相对启动目录；这是唯一需要自定义路径的子能力，故直接复用总开关的值）
//
// 当前包含的调试能力：
//   1. 提交载荷落盘：把每次发给 ComfyUI POST /prompt 的完整请求体按行写成 JSONL，
//      用于核对「实际提交了什么」——提示词是否被注入、种子是否随机化、
//      %date% 模板是否展开等问题的排查。
//      默认路径 <DATA_DIR>/debug/submit-payloads.jsonl（NAS 部署时即
//      ./data/debug/submit-payloads.jsonl，落在挂载卷上，宿主机直接可读）。
//      每行一条 {time, url, client_id, payload}；写盘发生在请求发出之前，
//      提交失败也能看到载荷；写盘失败只吞掉，绝不影响正常出图。
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
	debugOn        bool
	submitDumpPath string
	submitDumpMu   sync.Mutex
)

type submitDumpRecord struct {
	Time     string          `json:"time"`
	URL      string          `json:"url"`
	ClientID string          `json:"client_id"`
	Payload  json.RawMessage `json:"payload"`
}

// initDebug 解析 DEBUG 开关并准备调试输出目录。在 main 里 app 建好之后调用一次。
func initDebug(dataDir string, logger *log.Logger) {
	value := strings.TrimSpace(os.Getenv("DEBUG"))
	switch strings.ToLower(value) {
	case "", "off", "0", "false", "no":
		return
	case "1", "true", "on", "yes":
		debugOn = true
		submitDumpPath = filepath.Join(dataDir, "debug", "submit-payloads.jsonl")
	default:
		debugOn = true
		submitDumpPath = value
	}
	if err := os.MkdirAll(filepath.Dir(submitDumpPath), 0o755); err != nil {
		logger.Printf("debug: create dir failed: %v — submit payload dump disabled", err)
		submitDumpPath = ""
		return
	}
	logger.Printf("debug: ON, submit payload dump -> %s", submitDumpPath)
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
