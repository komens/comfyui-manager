package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDebugSwitch(t *testing.T) {
	dataDir := t.TempDir()
	logger := log.New(io.Discard, "", 0)

	reset := func() { debugOn = false; submitDumpPath = "" }

	// 默认关闭
	t.Setenv("DEBUG", "")
	reset()
	initDebug(dataDir, logger)
	if debugOn || submitDumpPath != "" {
		t.Fatalf("default should be off, got on=%v path=%q", debugOn, submitDumpPath)
	}

	// 1 → 开启 + 默认路径
	t.Setenv("DEBUG", "1")
	reset()
	initDebug(dataDir, logger)
	if !debugOn || submitDumpPath != filepath.Join(dataDir, "debug", "submit-payloads.jsonl") {
		t.Fatalf("1 should enable default path, got on=%v path=%q", debugOn, submitDumpPath)
	}

	// 自定义路径 + 落盘内容
	custom := filepath.Join(dataDir, "custom", "dump.jsonl")
	t.Setenv("DEBUG", custom)
	reset()
	initDebug(dataDir, logger)
	if !debugOn || submitDumpPath != custom {
		t.Fatalf("custom path, got on=%v path=%q", debugOn, submitDumpPath)
	}
	dumpSubmitPayload("http://x/prompt", "cid-1", []byte(`{"prompt":{"4":{"class_type":"CLIPTextEncode"}}}`))
	dumpSubmitPayload("http://x/prompt", "cid-2", []byte(`{"prompt":{"4":{"class_type":"KSampler"}}}`))
	raw, err := os.ReadFile(custom)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 jsonl lines, got %d", len(lines))
	}
	var rec submitDumpRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("line not json: %v", err)
	}
	if rec.ClientID != "cid-1" || !strings.Contains(string(rec.Payload), "CLIPTextEncode") {
		t.Fatalf("bad record: %+v", rec)
	}

	// off 关闭
	t.Setenv("DEBUG", "off")
	reset()
	initDebug(dataDir, logger)
	if debugOn || submitDumpPath != "" {
		t.Fatalf("off should disable, got on=%v path=%q", debugOn, submitDumpPath)
	}
}
