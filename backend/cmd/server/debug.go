package main

// ============================================================================
// Debug 模式（总开关）
//
// 环境变量 DEBUG 控制，默认关闭。所有调试能力统一挂在这个开关下，
// 不再各自发明环境变量。
//
// DEBUG 取值：
//   - 不设置 / 空 / off / 0 / false / no → 关闭
//   - 1 / true / on / yes               → 开启，落盘到 <DATA_DIR>/debug/
//   - 其它任意值                         → 开启，且该值作为「提交载荷」的文件路径
//     （同目录下还会放其它调试文件）
//
// 落盘产物（都在 <DATA_DIR>/debug/，NAS/云端部署时即 ./data/debug/，
// 落在挂载卷上，宿主机直接可读）：
//
//	startup.json           启动快照，每次启动覆盖写：版本、监听地址、各个路径的
//	                       绝对路径与可写性、comfyui_url、journal_mode、各表行数、
//	                       关键列是否存在。专治「挂载点不对 / data 卷是空的 /
//	                       镜像与库结构对不上」—— 看这一个文件比猜快得多。
//
//	submit-payloads.jsonl  每次 POST /prompt 的完整请求体。写盘发生在请求发出之前，
//	                       所以被 400 拒掉的载荷同样看得到。
//
//	http.jsonl             所有出站请求。**非 2xx 与传输错误必然记录**（带响应体），
//	                       2xx 默认不记：/history 每 5 秒轮询一次，把成功的也记下来
//	                       会把真正的问题淹在几千行「ok」里。成功但需要留痕的
//	                       （下载了多少字节、拿到的 prompt_id），由调用方补一句 emit。
//
//	events.jsonl           语义事件：item 状态流转、ComfyUI 自己报执行错时的原始
//	                       history、找不到输出时的 outputs 原文、自动纠正的前后值、
//	                       拿不到节点定义、重启恢复结果。
//
// 为什么做这么全：这套服务部署在云端，加一行日志就要重新 build → save → load →
// compose up。排查能力必须一次装够，不能等下次出事再加。
//
// 输出上限：单文件超过 maxDebugFileBytes 滚成 .1（只留一代）。DEBUG 忘了关是常态，
// 没有上限会把 data 卷写满 —— 那里面还放着图库。
// 所有写盘失败一律吞掉，绝不影响出图；DEBUG 关闭时不建目录、不写文件、不额外发请求。
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

// maxDebugFileBytes 单个调试文件的滚动阈值。
const maxDebugFileBytes = 16 << 20 // 16MB

// maxDebugBodyBytes 记录响应体时的默认上限（超出部分截断并标记）。
const maxDebugBodyBytes = 64 << 10 // 64KB

var (
	debugOn          bool
	debugDir         string
	debugMu          sync.Mutex
	submitDumpPath   string
	debugHTTPPath    string
	debugEventPath   string
	debugStartupPath string
)

// initDebug 解析 DEBUG 开关并准备调试输出目录。在 main 里 app 建好之后调用一次。
func initDebug(dataDir string, logger *log.Logger) {
	value := strings.TrimSpace(os.Getenv("DEBUG"))
	customSubmitPath := ""
	switch strings.ToLower(value) {
	case "", "off", "0", "false", "no":
		return
	case "1", "true", "on", "yes":
		debugDir = filepath.Join(dataDir, "debug")
	default:
		// 兼容既有用法：自定义值就是提交载荷的文件路径，其余调试文件放它旁边
		customSubmitPath = value
		debugDir = filepath.Dir(value)
	}

	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		logger.Printf("debug: create dir %s failed: %v — debug disabled", debugDir, err)
		return
	}

	submitDumpPath = customSubmitPath
	if submitDumpPath == "" {
		submitDumpPath = filepath.Join(debugDir, "submit-payloads.jsonl")
	}
	debugHTTPPath = filepath.Join(debugDir, "http.jsonl")
	debugEventPath = filepath.Join(debugDir, "events.jsonl")
	debugStartupPath = filepath.Join(debugDir, "startup.json")
	debugOn = true

	// 启动日志本身就是「DEBUG 生效了没有」的唯一判据，逐条列清楚。
	logger.Printf("debug: ON（日志含完整提示词，排查完请去掉 DEBUG 后重启）")
	logger.Printf("debug:   dir            %s", debugDir)
	logger.Printf("debug:   submit bodies  %s", submitDumpPath)
	logger.Printf("debug:   outbound http  %s", debugHTTPPath)
	logger.Printf("debug:   events         %s", debugEventPath)
	logger.Printf("debug:   startup        %s", debugStartupPath)
}

// debugRecord 组装一条记录：时间 + 事件名 + 自定义字段。
// fields 里的同名键会覆盖 time/event，调用方别用这两个名字。
func debugRecord(kind string, fields map[string]any) map[string]any {
	record := make(map[string]any, len(fields)+2)
	record["time"] = time.Now().Format(time.RFC3339Nano)
	record["event"] = kind
	for key, value := range fields {
		record[key] = value
	}
	return record
}

// debugEvent 追加一条语义事件到 events.jsonl。DEBUG 关闭时是空操作。
func debugEvent(kind string, fields map[string]any) {
	if !debugOn {
		return
	}
	appendDebugLine(debugEventPath, debugRecord(kind, fields))
}

// dumpStartupSnapshot 覆盖写启动快照（只要最新一份，所以不追加）。
func dumpStartupSnapshot(snapshot map[string]any) {
	if !debugOn {
		return
	}
	if err := writeDebugFile(debugStartupPath, snapshot); err != nil {
		return
	}
}

// dumpSubmitPayload 追加一行「即将发出的 /prompt 请求体」。任何失败都只吞掉。
func dumpSubmitPayload(url, clientID string, payload []byte) {
	if !debugOn || submitDumpPath == "" {
		return
	}
	appendDebugLine(submitDumpPath, submitDumpRecord{
		Time:     time.Now().Format(time.RFC3339Nano),
		URL:      url,
		ClientID: clientID,
		Payload:  json.RawMessage(payload),
	})
}

type submitDumpRecord struct {
	Time     string          `json:"time"`
	URL      string          `json:"url"`
	ClientID string          `json:"client_id"`
	Payload  json.RawMessage `json:"payload"`
}

// appendDebugLine 追加一行 JSONL。加锁、轮转、吞错。
func appendDebugLine(path string, record any) {
	if path == "" {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	rotateDebugFile(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}

// writeDebugFile 覆盖写一份文件（启动快照这类「只要最新一份」的用）。
func writeDebugFile(path string, value any) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// rotateDebugFile 超过上限就滚成 .1（覆盖更早的那一代）。调用方持锁。
func rotateDebugFile(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxDebugFileBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}

// errorText 把 error 渲染成可以直接进日志的字符串，nil 给空串。
// 事件字段里的 error 一律过它，免得每次都要写一遍 nil 判断。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// describePath 描述一个路径：绝对路径、是否存在、是否可写。
// 启动快照用它。可写性靠真建一个临时文件来判断 —— DATA_DIR 挂错、
// 只读挂载、容器内 uid 不匹配，都只有实际写一次才看得出来。
func describePath(path string) map[string]any {
	out := map[string]any{"path": path}
	if path == "" {
		return out
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		out["abs"] = abs
	} else {
		out["abs"] = path
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		out["exists"] = false
		out["error"] = err.Error()
		return out
	case !info.IsDir():
		out["exists"] = true
		out["is_dir"] = false
		return out
	}
	out["exists"] = true
	out["is_dir"] = true
	probe, err := os.CreateTemp(path, ".write-test-*")
	if err != nil {
		out["writable"] = false
		out["write_error"] = err.Error()
		return out
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	out["writable"] = true
	return out
}
