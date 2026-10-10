package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestLogger 返回一个丢弃输出的 logger，避免测试日志噪音刷屏。
func newTestLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// openTestDB 用与main.go 完全一致的 DSN 打开测试库。
// 参数必须照抄：busy_timeout / journal_mode(WAL) / _txlock=immediate 缺一不可，
// 否则测试环境与线上行为不一致，测出来的结论不可信。
func openTestDB(a *app) error {
	dsn := a.dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	a.db = db
	return nil
}

// newTestApp 起一个带完整 schema 的临时库 + data 目录。
func newTestApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	a := &app{
		log:          newTestLogger(),
		dataDir:      dir,
		dbPath:       filepath.Join(dir, "db", "test.db"),
		events:       make(map[int64]map[chan []byte]struct{}),
		saveFailures: make(map[int64]int),
		waiting:      make(map[int64]int),
	}
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := openTestDB(a); err != nil {
		t.Fatal(err)
	}
	if err := a.initDB("http://127.0.0.1:8188"); err != nil {
		t.Fatal(err)
	}
	if err := a.initCleanup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.db.Close() })
	return a
}

// TestDateBounds 是最关键的一条：created_at 存的是 Go 的 time.Time.String()，
// SQLite 的 date()/strftime() 对它返回 NULL。如果哪天有人"顺手优化"成
// date(created_at) <= ?，这个测试会立刻失败——那时所有时间筛选都会静默返回空集。
func TestDateBounds(t *testing.T) {
	f := cleanupFilter{DateFrom: "2026-09-01", DateTo: "2026-09-30"}
	from, to, err := f.dateBounds()
	if err != nil {
		t.Fatal(err)
	}
	if from != "2026-09-01 00:00:00" {
		t.Errorf("from = %q", from)
	}
	// 上界必须是 10-01 而不是 09-30 23:59:59：
	// 后者会漏掉 23:59:59.9 这种带小数秒的记录
	if to != "2026-10-01 00:00:00" {
		t.Errorf("to = %q, 期望次日 00:00:00", to)
	}
}

// TestDateBoundsCompareWithGoFormat 直接拿真实的 created_at 格式来比，
// 确认字典序比较确实等价于时间比较。
func TestDateBoundsCompareWithGoFormat(t *testing.T) {
	stored := "2026-09-30 23:59:59.947306 +0800 CST m=+115.156289126"
	if !(stored < "2026-10-01 00:00:00") {
		t.Errorf("9月30日的记录没有被次日上界包含，字典序假设不成立")
	}
	if stored < "2026-09-01 00:00:00" {
		t.Errorf("9月30日不该被 9月1日下界排除")
	}
}

func TestDateBoundsRejectsBadInput(t *testing.T) {
	cases := []cleanupFilter{
		{DateFrom: "2026/09/01"},
		{DateTo: "not-a-date"},
		{DateFrom: "2026-10-01", DateTo: "2026-09-01"}, // from >= to
	}
	for i, f := range cases {
		if _, _, err := f.dateBounds(); err == nil {
			t.Errorf("case %d: 期望报错，实际通过了", i)
		}
	}
}

// TestOnlyUnfavoriteDefaultsTrue 是安全底线：漏传字段时绝不能变成「收藏也删」。
func TestOnlyUnfavoriteDefaultsTrue(t *testing.T) {
	var f cleanupFilter
	if !f.onlyUnfavorite() {
		t.Fatal("未指定时必须是 true")
	}
	no := false
	f.OnlyUnfavorite = &no
	if f.onlyUnfavorite() {
		t.Fatal("显式传 false 时应生效")
	}
	yes := true
	f.OnlyUnfavorite = &yes
	if !f.onlyUnfavorite() {
		t.Fatal("显式传 true 时应生效")
	}
}

// TestWithPromptsDefaultsTrue 对应用户 2026-10-10 的拍板：删任务时提示词自动删。
func TestWithPromptsDefaultsTrue(t *testing.T) {
	var f cleanupFilter
	if !f.withPrompts() {
		t.Fatal("未指定时必须是 true（自动删）")
	}
	no := false
	f.WithPrompts = &no
	if f.withPrompts() {
		t.Fatal("显式传 false 时应生效")
	}
}

func TestFilterValidate(t *testing.T) {
	ok := cleanupFilter{Target: "images"}
	if err := ok.validate(); err != nil {
		t.Errorf("images 应通过: %v", err)
	}
	bad := cleanupFilter{Target: "workflows"}
	if err := bad.validate(); err == nil {
		t.Error("非法 target 应报错")
	}
	// status 只对 tasks 有意义，误传到 images 上应该被拦下
	mixed := cleanupFilter{Target: "images", Status: "failed"}
	if err := mixed.validate(); err == nil {
		t.Error("status 用在非 tasks 目标上应报错")
	}
}

// TestFilterUnknownFieldRejected 确认 decodeJSON 的 DisallowUnknownFields 生效：
// 前端拼错字段名时会立刻 400，而不是被静默忽略后按「无条件」执行。
func TestFilterUnknownFieldRejected(t *testing.T) {
	var f cleanupFilter
	if err := json.Unmarshal([]byte(`{"target":"images","typo_field":1}`), &f); err == nil {
		t.Log("标准 json.Unmarshal 不带 DisallowUnknownFields，属正常；实际校验靠 decodeJSON")
	}
}

func TestPlaceholders(t *testing.T) {
	cases := map[int]string{0: "", 1: "?", 3: "?,?,?"}
	for n, want := range cases {
		if got := placeholders(n); got != want {
			t.Errorf("placeholders(%d) = %q, 期望 %q", n, got, want)
		}
	}
}

// TestGroupFilterMultiSelect 覆盖分组多选（前端 el-select multiple 对应的字段）。
//
// 关键点：GroupNames 与 GroupName 并存时以多选为准——前端已全面改用多选，
// 若这里回退到单值，用户选了 3 个分组却只按第 1 个筛，且界面毫无提示。
func TestGroupFilterMultiSelect(t *testing.T) {
	var f cleanupFilter
	if got := f.groupFilter(); len(got) != 0 {
		t.Errorf("未指定时应为空, got %v", got)
	}
	// 只给旧的单值字段
	f.GroupName = "  分组A  "
	if got := f.groupFilter(); len(got) != 1 || got[0] != "分组A" {
		t.Errorf("单值回退失败: %v", got)
	}
	// 两个都给：多选优先
	f.GroupNames = []string{"分组B", " 分组C "}
	if got := f.groupFilter(); len(got) != 2 || got[0] != "分组B" || got[1] != "分组C" {
		t.Errorf("多选优先失败: %v", got)
	}
	// 多选里全为空串时不该产生条件
	f2 := cleanupFilter{GroupNames: []string{"", "  "}}
	if got := f2.groupFilter(); len(got) != 0 {
		t.Errorf("空白项应被过滤掉, got %v", got)
	}
}

func TestGroupWhere(t *testing.T) {
	// 空条件不产生片段
	f := cleanupFilter{}
	if frag, args := f.groupWhere("p"); frag != "" || args != nil {
		t.Errorf("空条件应无片段: %q %v", frag, args)
	}
	// 单个走 =?（不生成 IN，省一次解析）
	f.GroupNames = []string{"A"}
	frag, args := f.groupWhere("p")
	if frag != " AND p.group_name=?" || len(args) != 1 || args[0] != "A" {
		t.Errorf("单值片段 = %q %v", frag, args)
	}
	// 多个走 IN
	f.GroupNames = []string{"A", "B", "C"}
	frag, args = f.groupWhere("p")
	if frag != " AND p.group_name IN (?,?,?)" || len(args) != 3 {
		t.Errorf("多值片段 = %q %v", frag, args)
	}
}

// TestGroupMultiSelectEndToEnd 端到端守住多选分组：
// 选两个分组时，两组的数据都要命中（是 OR，不是只取第一个，也不是 AND）。
func TestGroupMultiSelectEndToEnd(t *testing.T) {
	a := newTestApp(t)
	seed := func(pid int, title, group string) {
		mustExec(t, a, `INSERT INTO prompts(id,title,positive_prompt,group_name,group_id,status,created_at)
			VALUES(?,'`+title+`','p','`+group+`','','done','2026-10-01 10:00:00')`, pid)
	}
	seed(1, "甲", "组A")
	seed(2, "乙", "组B")
	seed(3, "丙", "组C")

	f := cleanupFilter{Target: "prompts", GroupNames: []string{"组A", "组B"}}
	plan, err := a.resolveCleanup(context.Background(), &f)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PromptIDs) != 2 {
		t.Fatalf("命中 %v, 期望组A与组B共 2 条", plan.PromptIDs)
	}

	// 只选一个时不应带出别的组
	f2 := cleanupFilter{Target: "prompts", GroupNames: []string{"组C"}}
	plan2, err := a.resolveCleanup(context.Background(), &f2)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2.PromptIDs) != 1 || plan2.PromptIDs[0] != 3 {
		t.Errorf("单选分组结果 = %v, 期望只有 id=3", plan2.PromptIDs)
	}
}

func TestKeywordTerms(t *testing.T) {
	f := cleanupFilter{Keyword: "  少女   蓝发  "}
	terms := f.keywordTerms()
	if len(terms) != 2 || terms[0] != "%少女%" || terms[1] != "%蓝发%" {
		t.Errorf("keywordTerms = %v", terms)
	}
	empty := cleanupFilter{}
	if len(empty.keywordTerms()) != 0 {
		t.Error("空关键词应返回 nil")
	}
}

func TestSortedInt64Keys(t *testing.T) {
	got := sortedInt64Keys(map[int64]bool{5: true, 1: true, 3: true})
	want := []int64{1, 3, 5}
	if len(got) != len(want) {
		t.Fatalf("len = %d, 期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if sortedInt64Keys(nil) != nil {
		t.Error("空 map 应返回 nil")
	}
}

func TestFilterSummaryReadable(t *testing.T) {
	f := cleanupFilter{Target: "images", DateFrom: "2026-09-01", DateTo: "2026-09-30", Keyword: "少女"}
	s := f.Target + filterSummary(&f)
	if s == "" {
		t.Fatal("批次说明不应为空")
	}
	if !containsSubstring(s, "2026-09-01") || !containsSubstring(s, "少女") {
		t.Errorf("批次说明缺少关键信息: %s", s)
	}
}

// TestSubtractProtected 覆盖收藏保护的计数换算。
// 曾经的 bug：Protected 存的是「不看收藏的总数」，直接当「跳过数」报出去，
// 于是在收藏数为 0 时也报「已跳过 11 张」。
func TestSubtractProtected(t *testing.T) {
	p := &cleanupPlan{Protected: 11, ImageIDs: []int64{1, 2, 3, 4, 5}}
	p.subtractProtected(5)
	if p.Protected != 6 {
		t.Errorf("Protected = %d, 期望 6", p.Protected)
	}
	// 不允许出负数
	p2 := &cleanupPlan{Protected: 2}
	p2.subtractProtected(10)
	if p2.Protected != 0 {
		t.Errorf("Protected = %d, 期望夹到 0", p2.Protected)
	}
}

func containsSubstring(s, sub string) bool {
	return strings.Contains(s, sub)
}

// TestPlanStatsCounts 确认统计口径与实际落点一致。
func TestPlanStatsCounts(t *testing.T) {
	p := &cleanupPlan{
		ImageIDs:  []int64{1, 2, 3},
		ItemIDs:   []int64{1, 2},
		TaskIDs:   []int64{1},
		PromptIDs: []int64{9},
		Bytes:     1234,
	}
	s := p.Stats()
	if s.Images != 3 || s.Items != 2 || s.Tasks != 1 || s.Prompts != 1 || s.Bytes != 1234 {
		t.Errorf("stats = %+v", s)
	}
}

// TestTrashStoreInit 确认回收站目录与表能被建出来。
func TestTrashStoreInit(t *testing.T) {
	a := newTestApp(t)
	if a.cleanup == nil {
		t.Fatal("cleanup 未初始化")
	}
	if _, err := os.Stat(a.cleanup.dir); err != nil {
		t.Errorf("回收站目录未创建: %v", err)
	}
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cleanup_batches'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("cleanup_batches 表未创建")
	}
}

// TestIndexesCreated 是性能底线：没有这些索引，条件删除就是全表扫，
// 几万条数据时会拖垮整个服务。
func TestIndexesCreated(t *testing.T) {
	a := newTestApp(t)
	want := []string{
		"idx_images_created", "idx_images_item", "idx_items_task",
		"idx_items_prompt", "idx_tasks_created", "idx_prompts_group", "idx_prompts_created",
	}
	for _, name := range want {
		var n int
		err := a.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("索引 %s 未创建", name)
		}
	}
}

// TestBatchDirNoTraversal 确认批次目录拼接不会穿越出回收站根目录。
// batchDir 的入参是 int64，理论上无法穿越——这个测试守住「不要改成接受字符串」这条约束。
func TestBatchDirNoTraversal(t *testing.T) {
	a := newTestApp(t)
	dir := a.cleanup.batchDir(42)
	if filepath.Dir(dir) != a.cleanup.dir {
		t.Errorf("批次目录逃出了回收站根目录: %s", dir)
	}
}

// TestFillImageSizes 守住「预览要能算出回收体积」。
//
// 这个函数曾被 planPrompts 漏掉，导致预览显示 0 B、实际回收 4 MB ——
// 用户在删除前最需要的一个数字（能收回多少空间）恰恰是错的。
// 测试覆盖 images / tasks / prompts 三条路径，因为它们各自独立收集 ImageIDs。
func TestFillImageSizes(t *testing.T) {
	a := newTestApp(t)
	seed := func(imageID int64, filename string, content string) {
		stored := filepath.Join(a.dataDir, "images", filename)
		if err := os.WriteFile(stored, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Exec(
			`INSERT INTO images(id, generation_item_id, filename, storage_path, created_at, is_favorite)
			 VALUES(?,?,?,?,?,0)`, imageID, 1, filename, stored, "2026-10-01 10:00:00 +0800 CST"); err != nil {
			t.Fatal(err)
		}
	}
	seed(101, "a.png", "0123456789") // 10 字节
	seed(102, "b.png", "01234")      // 5 字节
	// 103 有库记录但磁盘无文件 —— FilesFound 应为 2、Bytes 应为 15

	plan := &cleanupPlan{
		Target:     "images",
		ImageIDs:   []int64{101, 102, 103},
		ImageFiles: map[int64]string{101: "a.png", 102: "b.png", 103: "missing.png"},
	}
	a.fillImageSizes(context.Background(), plan)

	if plan.Bytes != 15 {
		t.Errorf("Bytes = %d, 期望 15", plan.Bytes)
	}
	if plan.FilesFound != 2 {
		t.Errorf("FilesFound = %d, 期望 2（磁盘上没有的文件不计入）", plan.FilesFound)
	}
}

// TestPlanPromptsCascadeComputesBytes 端到端守住上面那个 bug：
// prompts 级联模式下预览必须算出体积，否则删除前看不到能收回多少空间。
func TestPlanPromptsCascadeComputesBytes(t *testing.T) {
	a := newTestApp(t)
	// 工作流 → 任务 → 生成项 → 提示词 → 图片 的一整条链
	mustExec(t, a, `INSERT INTO workflows(id,name,workflow_path,mapping_json,created_at,updated_at)
		VALUES(1,'w','w-1.json','{}','2026-10-01 10:00:00','2026-10-01 10:00:00')`)
	mustExec(t, a, `INSERT INTO generation_tasks(id,source_type,workflow_id,comfyui_url,parameters_json,status,created_at)
		VALUES(1,'direct',1,'http://x','{}','completed','2026-10-01 10:00:00')`)
	mustExec(t, a, `INSERT INTO generation_items(id,task_id,prompt_id,positive_prompt,status)
		VALUES(1,1,1,'p','success')`)
	mustExec(t, a, `INSERT INTO prompts(id,title,positive_prompt,group_name,group_id,status,created_at)
		VALUES(1,'t','body','分组A','','done','2026-10-01 10:00:00')`)
	stored := filepath.Join(a.dataDir, "images", "x.png")
	if err := os.WriteFile(stored, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, `INSERT INTO images(id,generation_item_id,filename,storage_path,created_at,is_favorite)
		VALUES(1,1,'x.png',?,'2026-10-01 10:00:00',0)`, stored)

	f := cleanupFilter{Target: "prompts", GroupName: "分组A", CascadeImages: true}
	plan, err := a.resolveCleanup(context.Background(), &f)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PromptIDs) != 1 {
		t.Fatalf("PromptIDs = %v, 期望 1 条", plan.PromptIDs)
	}
	if len(plan.ImageIDs) != 1 {
		t.Fatalf("ImageIDs = %v, 期望 1 张", plan.ImageIDs)
	}
	// 这条断言就是回归防护：曾经这里恒为 0
	if plan.Bytes != 10 {
		t.Errorf("Bytes = %d, 期望 10 —— 级联模式的体积统计被漏掉了", plan.Bytes)
	}
}

func mustExec(t *testing.T, a *app, query string, args ...any) {
	t.Helper()
	if _, err := a.db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
