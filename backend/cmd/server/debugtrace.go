package main

// ============================================================================
// 出站请求的统一留痕
//
// 本服务要跟 ComfyUI 说五件事：提交（POST /prompt）、查进度（GET /history）、
// 取图（GET /view）、查节点定义（GET /object_info）、取消（POST /queue）。
// 过去只有「提交」在 DEBUG 下落盘，而实际出问题时，现场往往在另外四个上：
//
//	卡在 submitted 不动      → /history 那边到底返回了什么，看不到
//	出图了但没保存下来        → 每张图的下载结果，看不到
//	「所有必填都缺失」        → /object_info 是不是根本没拉到，看不到
//	点取消没反应             → /queue 的返回码，看不到
//
// 所以这里做一个薄封装：所有出站请求都从 tracedDo 走，DEBUG 开启时把
// 「失败与非 2xx」自动记进 http.jsonl。成功的请求默认不记 —— /history 每 5 秒
// 轮询一次，把成功的也写下来等于把真正的问题埋进噪音里。
// ============================================================================

import (
	"bytes"
	"io"
	"net/http"
	"time"
)

// httpTrace 是一次出站请求的观察结果。
//
// DEBUG 关闭时 tracedDo 返回 nil，而 emit 容忍 nil receiver ——
// 调用点因此可以直接写 trace.emit(...)，不必到处判空，
// 关掉 DEBUG 的路径上也没有额外分配。
type httpTrace struct {
	Tag        string
	Method     string
	URL        string
	Status     int
	DurationMS int64
	Err        string
	Body       string
}

// emit 落一行到 http.jsonl。note/extra 用于「成功但需要留痕」的场景
// （比如下载完成记字节数、提交成功记 prompt_id）。
func (t *httpTrace) emit(note string, extra map[string]any) {
	if t == nil || !debugOn {
		return
	}
	fields := map[string]any{
		"tag":         t.Tag,
		"method":      t.Method,
		"url":         t.URL,
		"status":      t.Status,
		"duration_ms": t.DurationMS,
	}
	if t.Err != "" {
		fields["error"] = t.Err
	}
	if t.Body != "" {
		fields["body"] = t.Body
	}
	if note != "" {
		fields["note"] = note
	}
	for key, value := range extra {
		fields[key] = value
	}
	appendDebugLine(debugHTTPPath, debugRecord("http", fields))
}

// tracedDo 发请求，DEBUG 开启时把「传输错误与非 2xx」自动记进 http.jsonl。
//
// DEBUG 关闭时它退化成 client.Do(req)，trace 返回 nil，行为与引入它之前完全一致。
//
// errBodyLimit > 0 时，非 2xx 的响应体会被读进 trace（截断到该长度），
// 读完再装回 resp.Body，调用方照常读、不会少内容。
// 二进制响应（下载图片，errBodyLimit=0）不记响应体，免得把图片塞进日志。
func tracedDo(client *http.Client, req *http.Request, tag string, errBodyLimit int) (*http.Response, *httpTrace, error) {
	if !debugOn {
		response, err := client.Do(req)
		return response, nil, err
	}

	trace := &httpTrace{Tag: tag, Method: req.Method, URL: req.URL.String()}
	start := time.Now()
	response, err := client.Do(req)
	trace.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		trace.Err = err.Error()
		trace.emit("", nil)
		return nil, trace, err
	}

	trace.Status = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if errBodyLimit > 0 {
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, int64(errBodyLimit)))
			_ = response.Body.Close()
			if readErr == nil {
				trace.Body = string(raw)
			}
			// 放回去，让调用方还能按原来的方式读（比如 comfyRejectedError 需要原始结构）
			response.Body = io.NopCloser(bytes.NewReader(raw))
		}
		trace.emit("", nil)
	}
	return response, trace, nil
}

// comfyHistoryDigest 剪掉 /history 响应里体积大又没有排查价值的部分。
//
// 关键信息是 status（status_str 与 messages，节点报错就在这里）和 outputs。
// messages 会随节点执行逐条累积，长工作流能到几千条，整份存下来会把日志撑爆，
// 所以只保留末尾若干条 —— 报错一定在最后。
func comfyHistoryDigest(history map[string]any) map[string]any {
	digest := map[string]any{}
	if status, ok := history["status"].(map[string]any); ok {
		trimmed := make(map[string]any, len(status)+1)
		for key, value := range status {
			trimmed[key] = value
		}
		if messages, ok := status["messages"].([]any); ok && len(messages) > maxDebugHistoryMessages {
			trimmed["messages"] = messages[len(messages)-maxDebugHistoryMessages:]
			trimmed["messages_dropped"] = len(messages) - maxDebugHistoryMessages
		}
		digest["status"] = trimmed
	}
	if outputs, ok := history["outputs"]; ok {
		digest["outputs"] = outputs
	}
	return digest
}

// maxDebugHistoryMessages 保留的 history.messages 末尾条数。
const maxDebugHistoryMessages = 40
