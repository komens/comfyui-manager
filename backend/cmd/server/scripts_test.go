package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestRunner 造一个不依赖 ENABLE_SCRIPTS 的执行器，只用手边的 sh。
// 不起 worker，测试里直接调 execute，免得断言等不到。
func newTestRunner(t *testing.T, a *app) *scriptRunner {
	t.Helper()
	runsDir := filepath.Join(a.dataDir, "scripts", "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &scriptRunner{
		app:      a,
		log:      newTestLogger(),
		runsDir:  runsDir,
		apiBase:  "http://127.0.0.1:0",
		timeout:  10 * time.Second,
		maxKeep:  scriptMaxKeepRuns,
		runtimes: map[string]scriptRuntime{"sh": {Language: "sh", Path: "/bin/sh", Version: "sh"}},
		queue:    make(chan int64, 4),
		cancels:  make(map[int64]chan struct{}),
		live:     make(map[int64]*liveScriptRun),
	}
	a.scripts = runner
	return runner
}

func enqueueTestRun(t *testing.T, a *app, language, code string) int64 {
	t.Helper()
	result, err := a.db.Exec(
		`INSERT INTO script_runs(title, language, code, args, status, created_at) VALUES(?,?,?,'','queued',?)`,
		"test", language, code, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}

// TestSplitScriptArgs 参数是「这次处理的数据不一样」的主要手段，切错就等于跑错数据。
func TestSplitScriptArgs(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a b c", []string{"a", "b", "c"}},
		{`--tag hello world`, []string{"--tag", "hello", "world"}},
		{`--path "/data/我的 图"`, []string{"--path", "/data/我的 图"}},
		{`--path '/data/a b' x`, []string{"--path", "/data/a b", "x"}},
		{"a\tb\nc", []string{"a", "b", "c"}},
		{`'未闭合`, []string{"未闭合"}},
	}
	for _, item := range cases {
		got := splitScriptArgs(item.raw)
		if len(got) != len(item.want) {
			t.Fatalf("splitScriptArgs(%q) = %v, 期望 %v", item.raw, got, item.want)
		}
		for i := range got {
			if got[i] != item.want[i] {
				t.Fatalf("splitScriptArgs(%q)[%d] = %q, 期望 %q", item.raw, i, got[i], item.want[i])
			}
		}
	}
}

func TestNormalizeScriptLanguage(t *testing.T) {
	cases := map[string]string{
		"": "python", "python": "python", "py": "python", "PYTHON3": "python",
		"sh": "sh", "shell": "sh", "Bash": "sh",
	}
	for input, want := range cases {
		got, ok := normalizeScriptLanguage(input)
		if !ok || got != want {
			t.Errorf("normalizeScriptLanguage(%q) = (%q,%v), 期望 %q", input, got, ok, want)
		}
	}
	if _, ok := normalizeScriptLanguage("ruby"); ok {
		t.Error("不认识的语言不该被接受")
	}
}

// TestExecuteShellRun 跑一条真实脚本：验证状态、输出、产出文件与 work dir 归属。
func TestExecuteShellRun(t *testing.T) {
	a := newTestApp(t)
	runner := newTestRunner(t, a)
	id := enqueueTestRun(t, a, "sh", "echo hello\nmkdir -p out\necho 数据 > out/result.txt\necho 错误信息 >&2\n")

	runner.execute(id)

	run, err := a.loadScriptRun(fmt.Sprint(id))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "succeeded" || run.ExitCode != 0 {
		t.Fatalf("status=%s exit=%d err=%s", run.Status, run.ExitCode, run.Error)
	}
	if !strings.Contains(run.Output, "hello") || !strings.Contains(run.Output, "错误信息") {
		t.Errorf("stdout 与 stderr 应该合并进同一份输出，实际 %q", run.Output)
	}
	if run.WorkDir == "" || !strings.HasPrefix(run.WorkDir, runner.runsDir) {
		t.Errorf("work dir 应落在 runs 目录下，实际 %q", run.WorkDir)
	}
	if _, err := os.Stat(filepath.Join(run.WorkDir, "out", "result.txt")); err != nil {
		t.Errorf("产出文件没写出来: %v", err)
	}
	// 脚本本体与 wfkit 都要落地，方便事后照着复现
	for _, name := range []string{"main.sh", "wfkit.py", "meta.json"} {
		if _, err := os.Stat(filepath.Join(run.WorkDir, name)); err != nil {
			t.Errorf("%s 未写入运行目录: %v", name, err)
		}
	}
	if run.DurationMS < 0 {
		t.Errorf("duration 不该为负: %d", run.DurationMS)
	}
}

// TestExecuteFailureExitCode 失败要留下真实退出码，不能一律报「失败」。
func TestExecuteFailureExitCode(t *testing.T) {
	a := newTestApp(t)
	runner := newTestRunner(t, a)
	id := enqueueTestRun(t, a, "sh", "echo boom >&2\nexit 7\n")

	runner.execute(id)

	run, err := a.loadScriptRun(fmt.Sprint(id))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" || run.ExitCode != 7 {
		t.Fatalf("status=%s exit=%d, 期望 failed/7", run.Status, run.ExitCode)
	}
	if !strings.Contains(run.Error, "7") {
		t.Errorf("错误说明里应带退出码，实际 %q", run.Error)
	}
}

// TestExecuteTimeout 超时必须判 timeout 并且真的把进程收掉。
func TestExecuteTimeout(t *testing.T) {
	a := newTestApp(t)
	runner := newTestRunner(t, a)
	runner.timeout = 500 * time.Millisecond
	id := enqueueTestRun(t, a, "sh", "echo start\nsleep 30\n")

	start := time.Now()
	runner.execute(id)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时没生效，等了 %s", elapsed)
	}

	run, err := a.loadScriptRun(fmt.Sprint(id))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "timeout" {
		t.Fatalf("status=%s, 期望 timeout", run.Status)
	}
	if !strings.Contains(run.Output, "start") {
		t.Errorf("超时前已有的输出应该保留，实际 %q", run.Output)
	}
}

// TestLiveRunBufferCap 输出必须有上限：一个死循环 print 不能把内存和库写爆。
func TestLiveRunBufferCap(t *testing.T) {
	live := &liveScriptRun{subs: make(map[chan []byte]struct{})}
	chunk := make([]byte, 64<<10)
	for i := 0; i < 64; i++ {
		_, _ = live.Write(chunk)
	}
	output, truncated := live.snapshot()
	if !truncated {
		t.Error("超过上限后应标记 truncated")
	}
	if len(output) != scriptOutputCapBytes {
		t.Errorf("缓冲长度 = %d, 期望 %d", len(output), scriptOutputCapBytes)
	}
}

// TestLiveRunSubscribeReplay 晚连上的订阅者要能拿到全部历史，且结束时只收到一帧 done。
func TestLiveRunSubscribeReplay(t *testing.T) {
	live := &liveScriptRun{subs: make(map[chan []byte]struct{})}
	_, _ = live.Write([]byte("first\n"))

	channel, tail, finished := live.subscribe(0)
	if finished {
		t.Fatal("运行中不该返回 finished")
	}
	if string(tail) != "first\n" {
		t.Errorf("回放内容 = %q", tail)
	}

	_, _ = live.Write([]byte("second\n"))
	frame := <-channel
	if !strings.Contains(string(frame), "second") {
		t.Errorf("增量帧 = %s", frame)
	}

	live.complete("succeeded", 0, time.Second, "", false)
	done := <-channel
	if !strings.Contains(string(done), `"type":"done"`) || !strings.Contains(string(done), "second") {
		t.Errorf("结束帧应带全文，实际 %s", done)
	}
	if _, open := <-channel; open {
		t.Error("结束帧之后通道应关闭")
	}
}

// TestExecutePrune 保留策略：超过 maxKeep 的老记录连目录一起清。
func TestExecutePrune(t *testing.T) {
	a := newTestApp(t)
	runner := newTestRunner(t, a)
	runner.maxKeep = 2
	ids := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		id := enqueueTestRun(t, a, "sh", "echo run\n")
		ids = append(ids, id)
		runner.execute(id)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM script_runs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("剩余记录 = %d, 期望 2", count)
	}
	for _, id := range ids[:2] {
		if _, err := os.Stat(filepath.Join(runner.runsDir, fmt.Sprint(id))); !os.IsNotExist(err) {
			t.Errorf("run %d 的目录应该被清掉", id)
		}
	}
}

// TestWfkitSourceCompiles 保证内嵌的 wfkit.py 至少是合法 Python。
// 它每次运行都会被写进工作目录，语法错了会让所有脚本在 import 时就挂掉。
func TestWfkitSourceCompiles(t *testing.T) {
	python := ""
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			python = path
			break
		}
	}
	if python == "" {
		t.Skip("没有 python 可执行文件，跳过")
	}
	file := filepath.Join(t.TempDir(), "wfkit.py")
	if err := os.WriteFile(file, []byte(wfkitSource), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", "import py_compile,sys; py_compile.compile(sys.argv[1], doraise=True)", file)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wfkit.py 语法错误: %v\n%s", err, output)
	}
}
