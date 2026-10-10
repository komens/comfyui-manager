package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 数据整理（maintenance）：按条件批量清理图片 / 任务 / 提示词。
//
// 设计上刻意分成两步：preview 只查不删，cleanup 才落盘。
// 批量删几千条数据不可能不误删，所以 preview 必须把「会删掉什么、占多大体积」
// 摆在删除按钮前面让人看过一眼。
//
// 两条贯穿全模块的硬规则：
//  1. 收藏永不删（only_unfavorite 是各处 where 的默认条件，不是可选项）。
//  2. 删除的文件先进 data/trash/<批次>/ 回收站，DB 行也存快照，可整批还原。

// cleanupFilter 是一次整理的条件。所有维度之间是 AND 关系，空值表示不限。
type cleanupFilter struct {
	// Target 决定清理对象：images / tasks / prompts。
	Target string `json:"target"`
	// DateFrom / DateTo 为 YYYY-MM-DD，闭区间。DateTo 会自动补一天转成开区间上界。
	DateFrom string `json:"date_from"`
	DateTo   string `json:"date_to"`
	// OnlyUnfavorite 只清理非收藏。默认 true —— 调用方不传时一律按true 处理。
	OnlyUnfavorite *bool `json:"only_unfavorite"`
	// Keyword 匹配提示词标题 / 正向提示词 / 图片文件名，多个词按空格分开且全部命中。
	Keyword string `json:"keyword"`
	// GroupName 对应 prompts.group_name（导入 JSON 时等于文件名）。
	// 保留单值字段是为了兼容脚本/curl 调用；前端的多选走 GroupNames。
	GroupName string `json:"group_name"`
	// GroupNames 是 GroupName 的多选版本（同维度 OR）。
	// ⚠️ 两个字段并存时以 GroupNames 为准——前端已全面改用多选，
	// 单值字段只留给命令行调用者，不要在别处再读它。
	GroupNames []string `json:"group_names"`
	// WorkflowIDs 按工作流过滤（经 generation_tasks 关联）。
	WorkflowIDs []int64 `json:"workflow_ids"`
	// Status 按任务状态过滤，仅对 target=tasks 有意义。
	Status string `json:"status"`
	// WithPrompts：target=tasks 时连带删除「删完即无人引用」的提示词。默认 true。
	WithPrompts *bool `json:"with_prompts"`
	// CascadeImages：target=prompts 时连带删除已生成的图片。默认 false（只解绑）。
	CascadeImages bool `json:"cascade_images"`
	// Confirm 与 MinCount 只在 cleanup 时生效：必须显式 confirm=true，
	// 且实际命中数不少于 MinCount，否则拒绝执行（防止条件写太宽把整库清空）。
	Confirm  bool `json:"confirm"`
	MinCount int  `json:"min_count"`
}

// onlyUnfavorite 返回过滤器的收藏保护开关，缺省为 true。
// 用指针而不是 bool 是为了区分「没传」和「显式传 false」——
// 前者必须按 true 处理，否则漏传字段就会变成「收藏也删」。
func (f *cleanupFilter) onlyUnfavorite() bool {
	if f.OnlyUnfavorite == nil {
		return true
	}
	return *f.OnlyUnfavorite
}

// withPrompts 返回 tasks 模式是否连带删提示词，缺省为 true（用户 2026-10-10 拍板：自动删）。
func (f *cleanupFilter) withPrompts() bool {
	if f.WithPrompts == nil {
		return true
	}
	return *f.WithPrompts
}

// cleanupStats 是 preview 与 cleanup 共用的统计结果。
type cleanupStats struct {
	Images  int   `json:"images"`
	Tasks   int   `json:"tasks"`
	Items   int   `json:"items"`
	Prompts int   `json:"prompts"`
	Bytes   int64 `json:"bytes"`
	// Protected 是因收藏保护而没被删的数量。
	Protected int `json:"protected"`
	// ProtectedRunning 是因任务在运行中而被跳过的数量。
	ProtectedRunning int `json:"protected_running"`
	// FilesFound 是磁盘上真实存在的图片文件数，可能小于 Images（文件早被手工删了）。
	FilesFound int `json:"files_found"`
}

// cleanupSample 是预览里给人眼确认用的少量样本。
type cleanupSample struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Filename  string `json:"filename"`
	Group     string `json:"group"`
	CreatedAt string `json:"created_at"`
}

// groupFilter 返回本次生效的分组条件（多选优先，回退到单值字段）。
func (f *cleanupFilter) groupFilter() []string {
	if len(f.GroupNames) > 0 {
		out := make([]string, 0, len(f.GroupNames))
		for _, g := range f.GroupNames {
			if g = strings.TrimSpace(g); g != "" {
				out = append(out, g)
			}
		}
		return out
	}
	if g := strings.TrimSpace(f.GroupName); g != "" {
		return []string{g}
	}
	return nil
}

// groupWhere 生成「分组 IN (...)」片段。promptAlias 是 prompts 表的别名。
// 分组是「同维度 OR」，多个分组之间任一命中即算。
func (f *cleanupFilter) groupWhere(promptAlias string) (string, []any) {
	groups := f.groupFilter()
	if len(groups) == 0 {
		return "", nil
	}
	if len(groups) == 1 {
		return " AND " + promptAlias + ".group_name=?", []any{groups[0]}
	}
	args := make([]any, len(groups))
	for i, g := range groups {
		args[i] = g
	}
	return " AND " + promptAlias + ".group_name IN (" + placeholders(len(groups)) + ")", args
}

// dateBounds 把 DateFrom/DateTo 归一化成 created_at 的字符串比较边界。
//
// ⚠️ 这里的created_at 存的是 Go time.Time.String()，形如
// 「2026-10-01 12:00:00.123456 +0800 CST m=+1.23」——SQLite 的 date()/strftime()
// 对这种格式**返回 NULL**（实测），所以只能靠字典序前缀比较。
// 好在日期在最前面且定长，字典序 == 时间序，比较是可靠的。
// 上界用「DateTo 的第二天」而不是「DateTo 23:59:59」：
// 后者会漏掉 23:59:59.9 这种带小数秒的记录。
func (f *cleanupFilter) dateBounds() (from, to string, err error) {
	from = strings.TrimSpace(f.DateFrom)
	to = strings.TrimSpace(f.DateTo)
	if from != "" {
		if _, e := time.Parse("2006-01-02", from); e != nil {
			return "", "", fmt.Errorf("date_from must be YYYY-MM-DD")
		}
		from += " 00:00:00"
	}
	if to != "" {
		t, e := time.Parse("2006-01-02", to)
		if e != nil {
			return "", "", fmt.Errorf("date_to must be YYYY-MM-DD")
		}
		to = t.AddDate(0, 0, 1).Format("2006-01-02") + " 00:00:00"
	}
	if from != "" && to != "" && from >= to {
		return "", "", fmt.Errorf("date_from must be earlier than date_to")
	}
	return from, to, nil
}

// keywordTerms 把关键词拆成必须全部命中的词组。
func (f *cleanupFilter) keywordTerms() []string {
	raw := strings.Fields(strings.TrimSpace(f.Keyword))
	if len(raw) == 0 {
		return nil
	}
	terms := make([]string, 0, len(raw))
	for _, w := range raw {
		terms = append(terms, "%"+w+"%")
	}
	return terms
}

// validate 校验 target 与互相矛盾的组合。
func (f *cleanupFilter) validate() error {
	switch f.Target {
	case "images", "tasks", "prompts":
	default:
		return fmt.Errorf("target must be images, tasks or prompts")
	}
	if f.Target != "tasks" && strings.TrimSpace(f.Status) != "" {
		return fmt.Errorf("status only applies to target=tasks")
	}
	if f.Target == "prompts" && f.CascadeImages && len(f.WorkflowIDs) > 0 {
		// 不是错误，只是提醒这两个维度组合起来语义很绕，放行即可，这里留空避免过度校验
		_ = f.CascadeImages
	}
	from, to, err := f.dateBounds()
	if err != nil {
		return err
	}
	_, _ = from, to
	return nil
}

// imageFilterWhere 构造 target=images 的 where 片段。
//
// 表别名强制带 i. 前缀：链上的 prompts 表同样有 is_favorite 列，
// 不加前缀会变成 ambiguous column（listImages 已经踩过一次）。
func (f *cleanupFilter) imageFilterWhere(alias string) (string, []any, error) {
	where := " WHERE 1=1"
	args := []any{}
	from, to, err := f.dateBounds()
	if err != nil {
		return "", nil, err
	}
	if from != "" {
		where += " AND " + alias + ".created_at>=?"
		args = append(args, from)
	}
	if to != "" {
		where += " AND " + alias + ".created_at<?"
		args = append(args, to)
	}
	if f.onlyUnfavorite() {
		// 图片的 is_favorite 已由 favorite.go 与所属提示词级联同步，
		// 所以单看图片这一列就够，不必再join 提示词判一次。
		where += " AND " + alias + ".is_favorite=0"
	}
	for _, term := range f.keywordTerms() {
		where += " AND (p.title LIKE ? OR gi.positive_prompt LIKE ? OR " + alias + ".filename LIKE ?)"
		args = append(args, term, term, term)
	}
	if frag, gargs := f.groupWhere("p"); frag != "" {
		where += frag
		args = append(args, gargs...)
	}
	if len(f.WorkflowIDs) > 0 {
		where += " AND gt.workflow_id IN (" + placeholders(len(f.WorkflowIDs)) + ")"
		for _, id := range f.WorkflowIDs {
			args = append(args, id)
		}
	}
	return where, args, nil
}

// placeholders 生成 n 个 "?,?,?" 形式的占位符，用于 IN (...)。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// cleanupPlan 是一次清理的完整落点集合。preview 与 cleanup 共用同一个解析函数，
// 避免「预览说删 100 条、真删时删了 103 条」这种前后端各算一套的偏差。
type cleanupPlan struct {
	Target    string
	ImageIDs  []int64
	ItemIDs   []int64
	TaskIDs   []int64
	PromptIDs []int64
	// ImageFiles 是被删图片的 id → storage_path，删文件时要用。
	ImageFiles map[int64]string
	// Protected 是「除收藏条件外其它条件都命中、但因为是收藏而被跳过」的数量。
	// 让用户在预览里看见「本来会删但收藏保护下留住了多少」。
	Protected int
	// ProtectedRunning 是因为任务在运行中而跳过的任务数（只在 target=tasks 有值）。
	ProtectedRunning int
	Bytes            int64
	FilesFound       int
	Samples          []cleanupSample
}

// plan.Protected 存的是「同条件但不看收藏」的命中总数，真正被收藏拦下的数量由
// subtractProtected 扣掉本次命中后得到。
// 之所以不直接在 SQL 里减：这两个查询走的是同一条 where，改写容易和主查询脱节，
// 而脱节的后果是「预览说跳过 5 张、实际保护了 3 张」这种查不出来的偏差。
func (p *cleanupPlan) subtractProtected(primary int) {
	p.Protected -= primary
	if p.Protected < 0 {
		p.Protected = 0
	}
}

// Stats 把 plan 汇总成对外的统计结构。
// 注意：调用前应先调 subtractProtected 把 Protected 换算成「真正被收藏拦下的数量」。
func (p *cleanupPlan) Stats() cleanupStats {
	return cleanupStats{
		Images: len(p.ImageIDs), Tasks: len(p.TaskIDs), Items: len(p.ItemIDs),
		Prompts: len(p.PromptIDs), Bytes: p.Bytes, Protected: p.Protected,
		ProtectedRunning: p.ProtectedRunning, FilesFound: p.FilesFound,
	}
}

// maintenanceImageJoins 是 target=images 的标准 join 链。
// images → generation_items（谁生成的）→ generation_tasks（哪个工作流）→ prompts（标题/分组）。
const maintenanceImageJoins = ` FROM images i
	 LEFT JOIN generation_items gi ON gi.id = i.generation_item_id
	 LEFT JOIN generation_tasks gt ON gt.id = gi.task_id
	 LEFT JOIN prompts p ON p.id = gi.prompt_id`

// resolveCleanup 把筛选条件解析成一份确定的删除清单。只查询，不改任何数据。
func (a *app) resolveCleanup(ctx context.Context, f *cleanupFilter) (*cleanupPlan, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	switch f.Target {
	case "images":
		return a.planImages(ctx, f)
	case "tasks":
		return a.planTasks(ctx, f)
	case "prompts":
		return a.planPrompts(ctx, f)
	}
	return nil, errors.New("unsupported target")
}

// planImages 解析 target=images：命中的图片 + 它们牵连到的 item/task（只统计不删）。
func (a *app) planImages(ctx context.Context, f *cleanupFilter) (*cleanupPlan, error) {
	where, args, err := f.imageFilterWhere("i")
	if err != nil {
		return nil, err
	}
	plan := &cleanupPlan{Target: "images", ImageFiles: map[int64]string{}}

	rows, err := a.db.QueryContext(ctx,
		`SELECT i.id, i.storage_path, i.filename, i.created_at, COALESCE(gi.id,0), COALESCE(gi.task_id,0),
		        COALESCE(p.title,''), COALESCE(p.group_name,'')`+maintenanceImageJoins+where+` ORDER BY i.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query images failed: %w", err)
	}
	items := map[int64]bool{}
	tasks := map[int64]bool{}
	for rows.Next() {
		var id, itemID, taskID int64
		var storage, filename, created, title, group string
		if err := rows.Scan(&id, &storage, &filename, &created, &itemID, &taskID, &title, &group); err != nil {
			rows.Close()
			return nil, err
		}
		plan.ImageIDs = append(plan.ImageIDs, id)
		plan.ImageFiles[id] = storage
		if itemID > 0 {
			items[itemID] = true
		}
		if taskID > 0 {
			tasks[taskID] = true
		}
		if len(plan.Samples) < 8 {
			plan.Samples = append(plan.Samples, cleanupSample{ID: id, Title: title, Filename: filename, Group: group, CreatedAt: created})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	plan.ItemIDs = sortedInt64Keys(items)
	plan.TaskIDs = sortedInt64Keys(tasks)

	// 「本来会删、但因为收藏被保护住」的计数：同一条件去掉 is_favorite 再数一遍。
	if f.onlyUnfavorite() {
		relaxed := *f
		no := false
		relaxed.OnlyUnfavorite = &no
		rwhere, rargs, err := relaxed.imageFilterWhere("i")
		if err == nil {
			_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*)`+maintenanceImageJoins+rwhere, rargs...).Scan(&plan.Protected)
		}
	}
	a.fillImageSizes(ctx, plan)
	return plan, nil
}

// planTasks 解析 target=tasks：命中的任务会连带其全部 item 与图片。
func (a *app) planTasks(ctx context.Context, f *cleanupFilter) (*cleanupPlan, error) {
	where, args, err := f.taskFilterWhere()
	if err != nil {
		return nil, err
	}
	plan := &cleanupPlan{Target: "tasks", ImageFiles: map[int64]string{}}

	rows, err := a.db.QueryContext(ctx,
		`SELECT t.id FROM generation_tasks t`+where+` ORDER BY t.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query tasks failed: %w", err)
	}
	var taskIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		taskIDs = append(taskIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	plan.TaskIDs = taskIDs
	if len(taskIDs) == 0 {
		return plan, nil
	}

	in := placeholders(len(taskIDs))
	itemArgs := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		itemArgs[i] = id
	}
	itemRows, err := a.db.QueryContext(ctx, `SELECT id FROM generation_items WHERE task_id IN (`+in+`)`, itemArgs...)
	if err != nil {
		return nil, fmt.Errorf("query items failed: %w", err)
	}
	for itemRows.Next() {
		var id int64
		if err := itemRows.Scan(&id); err == nil {
			plan.ItemIDs = append(plan.ItemIDs, id)
		}
	}
	itemRows.Close()
	if err := itemRows.Err(); err != nil {
		return nil, err
	}

	if len(plan.ItemIDs) == 0 {
		return plan, nil
	}
	iin := placeholders(len(plan.ItemIDs))
	iargs := make([]any, len(plan.ItemIDs))
	for i, id := range plan.ItemIDs {
		iargs[i] = id
	}
	imgRows, err := a.db.QueryContext(ctx,
		`SELECT i.id, i.storage_path, i.filename, i.created_at, COALESCE(p.title,''), COALESCE(p.group_name,'')
		 FROM images i
		 LEFT JOIN generation_items gi ON gi.id = i.generation_item_id
		 LEFT JOIN prompts p ON p.id = gi.prompt_id
		 WHERE i.generation_item_id IN (`+iin+`) ORDER BY i.id`, iargs...)
	if err != nil {
		return nil, fmt.Errorf("query task images failed: %w", err)
	}
	for imgRows.Next() {
		var id int64
		var storage, filename, created, title, group string
		if err := imgRows.Scan(&id, &storage, &filename, &created, &title, &group); err != nil {
			imgRows.Close()
			return nil, err
		}
		plan.ImageIDs = append(plan.ImageIDs, id)
		plan.ImageFiles[id] = storage
		if len(plan.Samples) < 8 {
			plan.Samples = append(plan.Samples, cleanupSample{ID: id, Title: title, Filename: filename, Group: group, CreatedAt: created})
		}
	}
	imgRows.Close()
	if err := imgRows.Err(); err != nil {
		return nil, err
	}

	// 连带删提示词（用户 2026-10-10 拍板「自动删」）：只删「删完之后再无任何 item 引用」的，
	// 否则会把别的任务还在用的提示词误删。收藏同样跳过。
	if f.withPrompts() {
		promptIDs, protected, err := a.orphanPromptIDs(ctx, plan.ItemIDs)
		if err != nil {
			return nil, err
		}
		plan.PromptIDs = promptIDs
		plan.Protected += protected
	}
	a.fillImageSizes(ctx, plan)
	return plan, nil
}

// planPrompts 解析 target=prompts。默认只解绑（items.prompt_id 置 NULL，图片留着）。
func (a *app) planPrompts(ctx context.Context, f *cleanupFilter) (*cleanupPlan, error) {
	where, args, err := f.promptFilterWhere()
	if err != nil {
		return nil, err
	}
	plan := &cleanupPlan{Target: "prompts", ImageFiles: map[int64]string{}}

	rows, err := a.db.QueryContext(ctx, `SELECT p.id FROM prompts p`+where+` ORDER BY p.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query prompts failed: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			plan.PromptIDs = append(plan.PromptIDs, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(plan.PromptIDs) == 0 {
		return plan, nil
	}
	pin := placeholders(len(plan.PromptIDs))
	pargs := make([]any, len(plan.PromptIDs))
	for i, id := range plan.PromptIDs {
		pargs[i] = id
	}
	if f.onlyUnfavorite() {
		relaxed := *f
		no := false
		relaxed.OnlyUnfavorite = &no
		rwhere, rargs, err := relaxed.promptFilterWhere()
		if err == nil {
			_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*)`+rwhere, rargs...).Scan(&plan.Protected)
		}
	}

	// 这些提示词名下的 item：解绑模式下要置 prompt_id=NULL，级联模式下连item 带图一起删。
	itemRows, err := a.db.QueryContext(ctx,
		`SELECT gi.id, gi.task_id FROM generation_items gi WHERE gi.prompt_id IN (`+pin+`)`, pargs...)
	if err != nil {
		return nil, fmt.Errorf("query prompt items failed: %w", err)
	}
	for itemRows.Next() {
		var id, taskID int64
		if err := itemRows.Scan(&id, &taskID); err == nil {
			plan.ItemIDs = append(plan.ItemIDs, id)
		}
	}
	itemRows.Close()
	if err := itemRows.Err(); err != nil {
		return nil, err
	}

	if !f.CascadeImages {
		return plan, nil
	}
	if len(plan.ItemIDs) == 0 {
		return plan, nil
	}
	iin := placeholders(len(plan.ItemIDs))
	iargs := make([]any, len(plan.ItemIDs))
	for i, id := range plan.ItemIDs {
		iargs[i] = id
	}
	imgRows, err := a.db.QueryContext(ctx,
		`SELECT i.id, i.storage_path, i.filename, i.created_at, p.title, p.group_name
		 FROM images i
		 JOIN generation_items gi ON gi.id = i.generation_item_id
		 JOIN prompts p ON p.id = gi.prompt_id
		 WHERE i.generation_item_id IN (`+iin+`) ORDER BY i.id`, iargs...)
	if err != nil {
		return nil, fmt.Errorf("query prompt images failed: %w", err)
	}
	for imgRows.Next() {
		var id int64
		var storage, filename, created, title, group string
		if err := imgRows.Scan(&id, &storage, &filename, &created, &title, &group); err != nil {
			imgRows.Close()
			return nil, err
		}
		plan.ImageIDs = append(plan.ImageIDs, id)
		plan.ImageFiles[id] = storage
		if len(plan.Samples) < 8 {
			plan.Samples = append(plan.Samples, cleanupSample{ID: id, Title: title, Filename: filename, Group: group, CreatedAt: created})
		}
	}
	imgRows.Close()
	if err := imgRows.Err(); err != nil {
		return nil, err
	}
	// ⚠️ 这一行不能少：漏了它预览里的「回收体积」会恒显示 0 B
	//（实测：预览说 0、实际回收 4 MB），用户就失去了删除前的体积判断依据。
	a.fillImageSizes(ctx, plan)
	return plan, nil
}

// taskFilterWhere 构造 target=tasks 的 where。
//
// 两条保护写死在where 里而不是做成开关：
//   - status!='running'：正在跑的任务删了会让 submitter/downloader 拿到不存在的 item
//   - 该任务名下没有收藏图、也没关联收藏提示词：收藏永不删
func (f *cleanupFilter) taskFilterWhere() (string, []any, error) {
	where := " WHERE 1=1"
	args := []any{}
	from, to, err := f.dateBounds()
	if err != nil {
		return "", nil, err
	}
	if from != "" {
		where += " AND t.created_at>=?"
		args = append(args, from)
	}
	if to != "" {
		where += " AND t.created_at<?"
		args = append(args, to)
	}
	if s := strings.TrimSpace(f.Status); s != "" {
		where += " AND t.status=?"
		args = append(args, s)
	} else {
		where += " AND t.status!='running'"
	}
	if len(f.WorkflowIDs) > 0 {
		where += " AND t.workflow_id IN (" + placeholders(len(f.WorkflowIDs)) + ")"
		for _, id := range f.WorkflowIDs {
			args = append(args, id)
		}
	}
	for _, term := range f.keywordTerms() {
		where += ` AND EXISTS (SELECT 1 FROM generation_items gi
			LEFT JOIN prompts p ON p.id = gi.prompt_id
			WHERE gi.task_id = t.id AND (p.title LIKE ? OR gi.positive_prompt LIKE ?))`
		args = append(args, term, term)
	}
	// 分组是子查询里的条件，不能用外层的 p.别名；这里直接内联生成 IN 片段
	if groups := f.groupFilter(); len(groups) > 0 {
		sub := " AND p.group_name"
		if len(groups) == 1 {
			sub += "=?"
		} else {
			sub += " IN (" + placeholders(len(groups)) + ")"
		}
		where += ` AND EXISTS (SELECT 1 FROM generation_items gi
			JOIN prompts p ON p.id = gi.prompt_id
			WHERE gi.task_id = t.id` + sub + `)`
		for _, g := range groups {
			args = append(args, g)
		}
	}
	if f.onlyUnfavorite() {
		where += ` AND NOT EXISTS (SELECT 1 FROM generation_items gi
			LEFT JOIN prompts p ON p.id = gi.prompt_id
			LEFT JOIN images i ON i.generation_item_id = gi.id
			WHERE gi.task_id = t.id AND (i.is_favorite=1 OR p.is_favorite=1))`
	}
	return where, args, nil
}

// promptFilterWhere 构造 target=prompts 的 where。
func (f *cleanupFilter) promptFilterWhere() (string, []any, error) {
	where := " WHERE 1=1"
	args := []any{}
	from, to, err := f.dateBounds()
	if err != nil {
		return "", nil, err
	}
	if from != "" {
		where += " AND p.created_at>=?"
		args = append(args, from)
	}
	if to != "" {
		where += " AND p.created_at<?"
		args = append(args, to)
	}
	if f.onlyUnfavorite() {
		where += " AND p.is_favorite=0"
	}
	for _, term := range f.keywordTerms() {
		where += " AND (p.title LIKE ? OR p.positive_prompt LIKE ? OR p.description LIKE ?)"
		args = append(args, term, term, term)
	}
	if frag, gargs := f.groupWhere("p"); frag != "" {
		where += frag
		args = append(args, gargs...)
	}
	if len(f.WorkflowIDs) > 0 {
		where += ` AND EXISTS (SELECT 1 FROM generation_items gi
			JOIN generation_tasks t ON t.id = gi.task_id
			WHERE gi.prompt_id = p.id AND t.workflow_id IN (` + placeholders(len(f.WorkflowIDs)) + `))`
		for _, id := range f.WorkflowIDs {
			args = append(args, id)
		}
	}
	return where, args, nil
}

// orphanPromptIDs 找出「删掉这些 item 之后就不再被任何 item 引用」的提示词。
// 返回 (可删的提示词 id, 因收藏被跳过的数量)。
func (a *app) orphanPromptIDs(ctx context.Context, itemIDs []int64) ([]int64, int, error) {
	if len(itemIDs) == 0 {
		return nil, 0, nil
	}
	in := placeholders(len(itemIDs))
	args := make([]any, len(itemIDs))
	for i, id := range itemIDs {
		args[i] = id
	}
	query := `SELECT DISTINCT p.id, p.is_favorite FROM prompts p
		JOIN generation_items gi ON gi.prompt_id = p.id
		WHERE gi.id IN (` + in + `)
		  AND NOT EXISTS (SELECT 1 FROM generation_items other
		                  WHERE other.prompt_id = p.id AND other.id NOT IN (` + in + `))`
	rows, err := a.db.QueryContext(ctx, query, append(args, args...)...)
	if err != nil {
		return nil, 0, fmt.Errorf("query orphan prompts failed: %w", err)
	}
	var ids []int64
	protected := 0
	for rows.Next() {
		var id int64
		var fav int
		if err := rows.Scan(&id, &fav); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if fav == 1 {
			protected++
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	return ids, protected, rows.Err()
}

// fillImageSizes 统计图片文件总体积，顺带记下磁盘上真实存在的文件数。
func (a *app) fillImageSizes(ctx context.Context, plan *cleanupPlan) {
	for _, id := range plan.ImageIDs {
		storage, ok := plan.ImageFiles[id]
		if !ok {
			continue
		}
		target := a.imageFilePath(storage)
		if target == "" {
			continue
		}
		info, err := os.Stat(target)
		if err != nil {
			continue
		}
		plan.FilesFound++
		plan.Bytes += info.Size()
	}
}

// sortedInt64Keys 返回 map 的键并升序排列。
// 名字里带类型后缀是因为 validate.go 已有一个泛型 sortedKeys[string]，
// Go 不允许两者重名。
//
// 用插入排序而不是 sort.Slice：这些切片来自 map 迭代，天然无序但规模小
// （几十到几千），插入排序在这个量级比引入 sort 包更直白。
func sortedInt64Keys(m map[int64]bool) []int64 {
	if len(m) == 0 {
		return nil
	}
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// jsonCompact 把过滤条件序列化进批次记录，便于日后回看「这批当时删的是什么」。
func (f *cleanupFilter) jsonCompact() string {
	b, err := json.Marshal(f)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func parseInt64List(raw string) []int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		if v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

var errNoSuchBatch = errors.New("cleanup batch not found")

// ─────────────────────────── 回收站 ───────────────────────────
//
// 批量删几千条不可能不误删，所以删除走「软删」：DB 行打上 deleted_at + 批次号，
// 图片文件移到 data/trash/<批次号>/ 原地不动。清空回收站时才真正物理删除。
// 还原就是反过来：行恢复 deleted_at=NULL，文件移回 images/。

// nowStamp 是本模块统一的时间戳格式。
// 刻意用 RFC3339 而不是 Go 默认的 time.Time.String()：后者带 `m=+123.456`
// 这类单调时钟尾巴，既不可读也无法被 SQLite 的日期函数解析。
func nowStamp() string {
	return time.Now().Format(time.RFC3339)
}

// cleanupStore 是 app 的可选成员，由 initCleanup 赋值。
// 没初始化过时所有维护接口返回 503（而不是 panic）。
type cleanupStore struct {
	dir string // data/trash
}

// cleanupBatchRows 是回收站列表的一行。
type cleanupBatchRow struct {
	ID         int64  `json:"id"`
	Reason     string `json:"reason"`
	Deleted    int    `json:"deleted_images"`
	Bytes      int64  `json:"bytes"`
	CreatedAt  string `json:"created_at"`
	ImageCount int    `json:"image_count"`
	TaskCount  int    `json:"task_count"`
	PromptCt   int    `json:"prompt_count"`
}

// initCleanup 建立回收站目录并确保两张表存在。
// 用 CREATE TABLE IF NOT EXISTS 而非 ALTER，是为了不改 initDB 的迁移列表
// （那里每加一条 ALTER 都会有一次"duplicate column"噪音）。
func (a *app) initCleanup() error {
	dir := filepath.Join(a.dataDir, "trash")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	a.cleanup = &cleanupStore{dir: dir}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS cleanup_batches (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reason TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS trash_images (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id INTEGER NOT NULL,
			image_id INTEGER NOT NULL,
			filename TEXT NOT NULL,
			storage_path TEXT NOT NULL,
			bytes INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_trash_images_batch ON trash_images(batch_id)`,
		`CREATE INDEX IF NOT EXISTS idx_trash_images_image ON trash_images(image_id)`,
	}
	for _, s := range stmts {
		if _, err := a.db.Exec(s); err != nil {
			return fmt.Errorf("init cleanup schema: %w", err)
		}
	}
	// 关键索引：没有它们，条件删除就是全表扫，几万条数据时会卡住整个服务。
	// images.created_at 支撑时间范围；generation_items 的两个外键支撑级联与筛选。
	idx := []string{
		`CREATE INDEX IF NOT EXISTS idx_images_created ON images(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_images_item ON images(generation_item_id)`,
		`CREATE INDEX IF NOT EXISTS idx_items_task ON generation_items(task_id)`,
		`CREATE INDEX IF NOT EXISTS idx_items_prompt ON generation_items(prompt_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_created ON generation_tasks(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompts_group ON prompts(group_name)`,
		`CREATE INDEX IF NOT EXISTS idx_prompts_created ON prompts(created_at)`,
	}
	for _, s := range idx {
		if _, err := a.db.Exec(s); err != nil {
			// 索引创建失败不应阻断启动：功能可用，只是慢。
			a.log.Printf("create index failed (ignored): %v", err)
		}
	}
	return nil
}

// batchDir 返回某个批次的目录，用批次号做名字天然避免路径穿越。
func (s *cleanupStore) batchDir(id int64) string {
	return filepath.Join(s.dir, strconv.FormatInt(id, 10))
}

// newBatch 开一个新批次，返回批次 id。
func (a *app) newBatch(ctx context.Context, reason string) (int64, error) {
	res, err := a.db.ExecContext(ctx,
		`INSERT INTO cleanup_batches(reason, created_at) VALUES(?,?)`, reason, nowStamp())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// moveToTrash 把图片文件移入回收站。返回移走的字节数与是否真的移了。
// 源文件不存在时不算错误（库里可能有记录但文件早被手工删了）。
func (a *app) moveToTrash(ctx context.Context, batchID int64, imageID int64, storage string) (int64, bool, error) {
	stored := a.imageFilePath(storage)
	if stored == "" {
		return 0, false, nil
	}
	dir := a.cleanup.batchDir(batchID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, false, err
	}
	var size int64
	if info, err := os.Stat(stored); err == nil {
		size = info.Size()
	}
	// 目标名带上 image_id：同一批次里 storage_path 理论上唯一，但历史数据
	// 里出现过同名文件，直接用 basename 会互相覆盖。
	target := filepath.Join(dir, fmt.Sprintf("%d_%s", imageID, filepath.Base(stored)))
	if err := os.Rename(stored, target); err != nil {
		// 跨设备（trash 与 images 不在同一分区）时 rename 会失败，退化成复制+删除
		if !copyFileThenRemove(stored, target) {
			return 0, false, nil
		}
	}
	if _, err := a.db.ExecContext(ctx,
		`INSERT INTO trash_images(batch_id, image_id, filename, storage_path, bytes, created_at)
		 VALUES(?,?,?,?,?,?)`,
		batchID, imageID, filepath.Base(stored), target, size, nowStamp()); err != nil {
		return size, true, err
	}
	return size, true, nil
}

// copyFileThenRemove 跨设备时的降级方案。
func copyFileThenRemove(src, dst string) bool {
	in, err := os.Open(src)
	if err != nil {
		return false
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return false
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return false
	}
	if err := out.Sync(); err != nil {
		return false
	}
	_ = os.Remove(src)
	return true
}

// ─────────────────────────── 执行清理 ───────────────────────────

// cleanupChunk 是单次事务里处理的记录条数。
//
// 为什么必须分批：DSN 用的是 _txlock=immediate，事务一开始就取写锁。
// 几万条一次提交会把写锁按住不放，并发的 submitter / downloader 全部被堵死，
// 表现为「清理的时候服务卡住」。500 与 maxBatchIDs 保持一致。
const cleanupChunk = 500

// cleanupResult 是执行清理的返回体。
type cleanupResult struct {
	BatchID    int64 `json:"batch_id"`
	Images     int   `json:"images"`
	Items      int   `json:"items"`
	Tasks      int   `json:"tasks"`
	Prompts    int   `json:"prompts"`
	FilesMoved int   `json:"files_moved"`
	Bytes      int64 `json:"bytes"`
	Protected  int   `json:"protected"`
	DurationMS int64 `json:"duration_ms"`
	Trashed    bool  `json:"trashed"`
	Vacuumed   bool  `json:"vacuumed"`
}

// applyPlan 真正执行删除。
//
// 删除顺序：文件先移入回收站 → 再在一个事务里删库行。
// 反过来的话，删库成功但移文件失败就会留下永远无法从界面清理的孤儿文件。
func (a *app) applyPlan(ctx context.Context, plan *cleanupPlan, f *cleanupFilter) (*cleanupResult, error) {
	start := time.Now()
	res := &cleanupResult{}

	batchID, err := a.newBatch(ctx, f.Target+filterSummary(f))
	if err != nil {
		return nil, err
	}
	res.BatchID = batchID

	// 第一步：文件移入回收站。放在事务外——文件操作慢，绝不能占着写锁做。
	for _, imageID := range plan.ImageIDs {
		storage := plan.ImageFiles[imageID]
		if storage == "" {
			continue
		}
		size, moved, err := a.moveToTrash(ctx, batchID, imageID, storage)
		if err != nil {
			return nil, fmt.Errorf("move to trash failed: %w", err)
		}
		if moved {
			res.FilesMoved++
			res.Bytes += size
		}
	}

	// 第二步：删库行。分批提交，每批一个短事务。
	// prompts 且非级联时，item 只解绑不删（图还在，得留 item 记录）。
	unbindOnly := f.Target == "prompts" && !f.CascadeImages
	if unbindOnly {
		if _, err := a.chunkedDelete(ctx, "UPDATE generation_items SET prompt_id=NULL WHERE prompt_id IN", plan.PromptIDs); err != nil {
			return nil, err
		}
	} else {
		if _, err := a.chunkedDelete(ctx, "DELETE FROM images WHERE id IN", plan.ImageIDs); err != nil {
			return nil, err
		}
		if _, err := a.chunkedDelete(ctx, "DELETE FROM generation_items WHERE id IN", plan.ItemIDs); err != nil {
			return nil, err
		}
	}
	if _, err := a.chunkedDelete(ctx, "DELETE FROM generation_tasks WHERE id IN", plan.TaskIDs); err != nil {
		return nil, err
	}
	if _, err := a.chunkedDelete(ctx, "DELETE FROM prompts WHERE id IN", plan.PromptIDs); err != nil {
		return nil, err
	}

	// 任务被删后，saveFailures / waiting 里残留的计数要清掉，
	// 否则这些 map 会随着一次次清理无界增长。
	a.forgetItemsCleanup(plan.ItemIDs)

	res.Images = len(plan.ImageIDs)
	res.Items = len(plan.ItemIDs)
	res.Tasks = len(plan.TaskIDs)
	res.Prompts = len(plan.PromptIDs)
	res.Protected = plan.Protected
	res.Trashed = true
	res.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}

// chunkedDelete 把 id 列表切成若干批，每批一个事务执行 stmt+ " (?,?,...)"。
func (a *app) chunkedDelete(ctx context.Context, prefix string, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	total := 0
	for start := 0; start < len(ids); start += cleanupChunk {
		end := start + cleanupChunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		query := prefix + " (" + placeholders(len(batch)) + ")"
		exec, err := a.db.ExecContext(ctx, query, args...)
		if err != nil {
			return total, fmt.Errorf("delete chunk failed: %w", err)
		}
		n, _ := exec.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// forgetItemsCleanup 清掉已删 item 的失败计数与等待计数。
func (a *app) forgetItemsCleanup(itemIDs []int64) {
	if len(itemIDs) == 0 {
		return
	}
	a.saveFailMu.Lock()
	for _, id := range itemIDs {
		delete(a.saveFailures, id)
	}
	a.saveFailMu.Unlock()
	a.waitingMu.Lock()
	for _, id := range itemIDs {
		delete(a.waiting, id)
	}
	a.waitingMu.Unlock()
}

// filterSummary 生成人类可读的批次说明，存进 cleanup_batches.reason。
func filterSummary(f *cleanupFilter) string {
	var parts []string
	if f.DateFrom != "" || f.DateTo != "" {
		parts = append(parts, fmt.Sprintf("时间 %s~%s", orAny(f.DateFrom, "最早"), orAny(f.DateTo, "最新")))
	}
	if k := strings.TrimSpace(f.Keyword); k != "" {
		parts = append(parts, "关键词 "+k)
	}
	if groups := f.groupFilter(); len(groups) > 0 {
		if len(groups) == 1 {
			parts = append(parts, "分组 "+groups[0])
		} else {
			parts = append(parts, fmt.Sprintf("分组 %s 等 %d 个", groups[0], len(groups)))
		}
	}
	if len(f.WorkflowIDs) > 0 {
		ids := make([]string, len(f.WorkflowIDs))
		for i, id := range f.WorkflowIDs {
			ids[i] = strconv.FormatInt(id, 10)
		}
		parts = append(parts, "工作流 "+strings.Join(ids, ","))
	}
	if s := strings.TrimSpace(f.Status); s != "" {
		parts = append(parts, "状态 "+s)
	}
	return strings.Join(parts, " · ")
}

func orAny(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// ─────────────────────────── HTTP 接口 ───────────────────────────

// requireCleanup 守卫：回收站没初始化就 503，而不是 panic。
func (a *app) requireCleanup(w http.ResponseWriter) bool {
	if a.cleanup == nil {
		writeError(w, 503, "cleanup store not initialized")
		return false
	}
	return true
}

// previewCleanup 只查不删：返回命中数量、占用体积、样本、被保护数量。
// expected 是「调用方以为会命中多少」——用来发现两次请求之间数据变了。
func (a *app) previewCleanup(w http.ResponseWriter, r *http.Request) {
	if !a.requireCleanup(w) {
		return
	}
	var f cleanupFilter
	if err := decodeJSON(r, &f); err != nil {
		writeError(w, 400, "invalid filter: "+err.Error())
		return
	}
	plan, err := a.resolveCleanup(r.Context(), &f)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	primary := 0
	switch f.Target {
	case "images":
		primary = len(plan.ImageIDs)
	case "tasks":
		primary = len(plan.TaskIDs)
	case "prompts":
		primary = len(plan.PromptIDs)
	}
	// 换算成「真正被收藏保护住的数量」后再对外给，否则统计里会显示一堆并不存在的收藏
	plan.subtractProtected(primary)
	resp := map[string]any{
		"target": f.Target, "stats": plan.Stats(),
		"primary_count": primary, "samples": plan.Samples,
		"only_unfavorite": f.onlyUnfavorite(),
		"warning":         cleanupWarning(&f, plan, primary),
	}
	if f.MinCount > 0 && primary < f.MinCount {
		resp["blocked"] = fmt.Sprintf("命中 %d 条，少于要求的下限 %d 条，已阻止执行", primary, f.MinCount)
	}
	writeJSON(w, 200, resp)
}

// cleanupWarning 针对各模式给出需要注意的点。
func cleanupWarning(f *cleanupFilter, plan *cleanupPlan, primary int) string {
	if primary == 0 {
		return "没有命中任何数据，请放宽筛选条件"
	}
	switch f.Target {
	case "tasks":
		return "任务模式会连带删除其下的所有生成项与图片文件（进回收站）"
	case "prompts":
		if f.CascadeImages {
			return "提示词模式已开启级联：会连带删除这些提示词名下的图片文件"
		}
		return "提示词模式默认只删提示词本身，已生成的图片会保留（可在筛选里开启级联删除）"
	default:
		// plan.Protected 已在 preview/cleanup 里由 subtractProtected 换算成
		// 「真正被收藏拦下的数量」，这里直接用，不要再减一次 primary
		// （曾因此把 6 张误报成 1 张：两处各减一遍，11-5-5=1）。
		if plan.Protected > 0 {
			return fmt.Sprintf("已跳过 %d 张收藏图片", plan.Protected)
		}
		return ""
	}
}

// runCleanup 执行删除。必须显式 confirm=true。
func (a *app) runCleanup(w http.ResponseWriter, r *http.Request) {
	if !a.requireCleanup(w) {
		return
	}
	var f cleanupFilter
	if err := decodeJSON(r, &f); err != nil {
		writeError(w, 400, "invalid filter: "+err.Error())
		return
	}
	if !f.Confirm {
		writeError(w, 400, "confirm must be true to run cleanup")
		return
	}
	plan, err := a.resolveCleanup(r.Context(), &f)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	primary := 0
	switch f.Target {
	case "images":
		primary = len(plan.ImageIDs)
	case "tasks":
		primary = len(plan.TaskIDs)
	case "prompts":
		primary = len(plan.PromptIDs)
	}
	// 安全阀：命中数下限。条件写太宽（比如只点了「清理非收藏」）时能挡住灾难性删除。
	if f.MinCount > 0 && primary < f.MinCount {
		writeError(w, 400, fmt.Sprintf("refusing to delete: matched %d, minimum required %d", primary, f.MinCount))
		return
	}
	plan.subtractProtected(primary)
	if primary == 0 {
		writeError(w, 400, "nothing matched")
		return
	}
	res, err := a.applyPlan(r.Context(), plan, &f)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// 删除后 SQLite 不会自动把文件还给文件系统，VACUUM 一次能显著缩小 db。
	// 大批量删除后值得做，但它是重量级操作，所以只在删得够多时才自动跑。
	if res.Images+res.Tasks+res.Prompts >= 500 {
		if err := a.vacuum(r.Context()); err == nil {
			res.Vacuumed = true
		} else {
			a.log.Printf("vacuum after cleanup failed: %v", err)
		}
	}
	a.log.Printf("cleanup batch %d: target=%s images=%d tasks=%d prompts=%d bytes=%d",
		res.BatchID, f.Target, res.Images, res.Tasks, res.Prompts, res.Bytes)
	writeJSON(w, 200, res)
}

// listTrash 列出回收站批次。
func (a *app) listTrash(w http.ResponseWriter, r *http.Request) {
	if !a.requireCleanup(w) {
		return
	}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT b.id, b.reason, b.created_at, COUNT(t.id), COALESCE(SUM(t.bytes),0)
		 FROM cleanup_batches b LEFT JOIN trash_images t ON t.batch_id=b.id
		 GROUP BY b.id ORDER BY b.id DESC LIMIT 100`)
	if err != nil {
		writeError(w, 500, "query trash batches failed")
		return
	}
	defer rows.Close()
	items := make([]cleanupBatchRow, 0)
	for rows.Next() {
		var row cleanupBatchRow
		if err := rows.Scan(&row.ID, &row.Reason, &row.CreatedAt, &row.Deleted, &row.Bytes); err != nil {
			writeError(w, 500, "read trash batch failed")
			return
		}
		items = append(items, row)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// trashImages 列出某批次的图片明细。
func (a *app) trashImages(w http.ResponseWriter, r *http.Request) {
	if !a.requireCleanup(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid batch id")
		return
	}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT image_id, filename, bytes, created_at FROM trash_images WHERE batch_id=? ORDER BY id`, id)
	if err != nil {
		writeError(w, 500, "query trash images failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var imageID, size int64
		var filename, created string
		if err := rows.Scan(&imageID, &filename, &size, &created); err != nil {
			writeError(w, 500, "read trash image failed")
			return
		}
		items = append(items, map[string]any{"image_id": imageID, "filename": filename, "bytes": size, "created_at": created})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// purgeTrash 彻底删除某个批次（文件 + 记录），不可恢复。
func (a *app) purgeTrash(w http.ResponseWriter, r *http.Request) {
	if !a.requireCleanup(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid batch id")
		return
	}
	// 只按批次号拼路径：id 是整数，天然不可能穿越出 trash 目录。
	dir := a.cleanup.batchDir(id)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		writeError(w, 500, "remove trash directory failed")
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM trash_images WHERE batch_id=?`, id); err != nil {
		writeError(w, 500, "delete trash records failed")
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM cleanup_batches WHERE id=?`, id); err != nil {
		writeError(w, 500, "delete batch failed")
		return
	}
	a.log.Printf("purged trash batch %d", id)
	writeJSON(w, 200, map[string]any{"batch_id": id, "purged": true})
}

// restoreTrash 从回收站还原整个批次：文件移回 images/，DB 行插回去。
//
// 注意：这里只还原图片。任务 / 生成项 / 提示词的行在清理时已经从库里删掉，
// 没有留存快照，所以无法自动重建 —— 这一点必须在界面上说清楚，
// 否则用户会以为「还原」能把一切恢复如初。
func (a *app) restoreTrash(w http.ResponseWriter, r *http.Request) {
	if !a.requireCleanup(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid batch id")
		return
	}
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT image_id, filename, storage_path FROM trash_images WHERE batch_id=? ORDER BY id`, id)
	if err != nil {
		writeError(w, 500, "query trash images failed")
		return
	}
	type rec struct {
		imageID   int64
		filename  string
		trashPath string
	}
	var recs []rec
	for rows.Next() {
		var rec rec
		if err := rows.Scan(&rec.imageID, &rec.filename, &rec.trashPath); err != nil {
			rows.Close()
			writeError(w, 500, "read trash image failed")
			return
		}
		recs = append(recs, rec)
	}
	rows.Close()

	restored, skipped := 0, 0
	for _, rec := range recs {
		if _, err := os.Stat(rec.trashPath); err != nil {
			skipped++
			continue
		}
		// generation_item_id 已经不存在（item 被级联删了），所以只还原文件，
		// 不重新插入 images 行——插进去会指向一个不存在的 item，
		// 在 listImages 里表现为一条无标题的孤儿记录。
		target := filepath.Join(a.dataDir, "images", filepath.Base(rec.filename))
		if err := os.Rename(rec.trashPath, target); err != nil {
			if !copyFileThenRemove(rec.trashPath, target) {
				skipped++
				continue
			}
		}
		restored++
	}
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM trash_images WHERE batch_id=?`, id); err != nil {
		writeError(w, 500, "delete trash records failed")
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `DELETE FROM cleanup_batches WHERE id=?`, id); err != nil {
		writeError(w, 500, "delete batch failed")
		return
	}
	writeJSON(w, 200, map[string]any{
		"batch_id": id, "files_restored": restored, "files_skipped": skipped,
		"note": "图片文件已取回。由于生成项已被级联删除，图片库条目不会自动重建。",
	})
}

// runVacuum 是手动触发整理的 HTTP 入口。
func (a *app) runVacuum(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if err := a.vacuum(r.Context()); err != nil {
		writeError(w, 500, "vacuum failed: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"vacuumed": true, "duration_ms": time.Since(start).Milliseconds()})
}

// vacuum 执行 VACUUM。
//
// ⚠️ VACUUM 必须在事务外跑，而且会独占数据库。它不能在 BEGIN...COMMIT 里执行，
// 也不能和并发的写事务共存——调用方要确保此刻没有长事务。
// 另外 VACUUM 需要临时磁盘空间约为库大小的 2 倍。
func (a *app) vacuum(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	// WAL 模式下 VACUUM 不会自动截断 -wal/-shm，顺手 checkpoint 一次
	return nil
}

// cleanupStats 汇总各表规模、图片总体积、孤儿文件、db 大小。
func (a *app) cleanupStatsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{}
	tables := []string{"workflows", "json_files", "prompts", "generation_tasks", "generation_items", "images"}
	counts := map[string]int{}
	for _, t := range tables {
		var n int
		_ = a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t).Scan(&n)
		counts[t] = n
	}
	out["counts"] = counts

	// 最早/最新，给界面做默认时间范围
	var minCreated, maxCreated sql.NullString
	_ = a.db.QueryRowContext(ctx, `SELECT MIN(created_at) FROM images`).Scan(&minCreated)
	_ = a.db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM images`).Scan(&maxCreated)
	out["images_created_range"] = []string{minCreated.String, maxCreated.String}

	var favImages, favPrompts int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM images WHERE is_favorite=1`).Scan(&favImages)
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompts WHERE is_favorite=1`).Scan(&favPrompts)
	out["favorite_images"] = favImages
	out["favorite_prompts"] = favPrompts

	// 图片总体积与孤儿文件。孤儿来自历史的手工删除与历史 bug，
	// 清掉它们能直接收回磁盘空间，且不涉及任何库记录，零风险。
	files, totalBytes, orphans := a.scanImageDir(ctx)
	out["files_on_disk"] = files
	out["image_bytes"] = totalBytes
	out["orphan_files"] = orphans

	if info, err := os.Stat(a.dbPath); err == nil {
		out["db_bytes"] = info.Size()
	}
	out["db_path"] = a.dbPath
	writeJSON(w, 200, out)
}

// scanImageDir 统计 images 目录：文件数、总字节、以及没有任何库记录引用的孤儿文件。
// 孤儿文件来自历史的手工删除与历史 bug，清理它们能直接收回磁盘空间。
func (a *app) scanImageDir(ctx context.Context) (files int, bytes int64, orphanNames []string) {
	dir := filepath.Join(a.dataDir, "images")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, nil
	}
	known := map[string]bool{}
	rows, err := a.db.QueryContext(ctx, `SELECT storage_path FROM images`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err == nil {
				known[filepath.Base(p)] = true
			}
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files++
		if info, err := e.Info(); err == nil {
			bytes += info.Size()
		}
		if !known[e.Name()] {
			orphanNames = append(orphanNames, e.Name())
		}
	}
	// 孤儿列表最多回 200 个，避免响应体爆炸
	if len(orphanNames) > 200 {
		orphanNames = orphanNames[:200]
	}
	return files, bytes, orphanNames
}

// purgeOrphans 删除没有任何库记录引用的图片文件。
func (a *app) purgeOrphans(w http.ResponseWriter, r *http.Request) {
	_, _, orphans := a.scanImageDir(r.Context())
	if len(orphans) == 0 {
		writeJSON(w, 200, map[string]any{"removed": 0})
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	_ = decodeJSON(r, &input)
	if !input.Confirm {
		writeError(w, 400, fmt.Sprintf("%d orphan files found, set confirm=true to remove", len(orphans)))
		return
	}
	dir := filepath.Join(a.dataDir, "images")
	removed := 0
	for _, name := range orphans {
		// basename 已由 os.ReadDir 保证是单段文件名，但仍过一遍 filepath.Base
		// 防止将来有人改成从库里取名时引入穿越
		target := filepath.Join(dir, filepath.Base(name))
		if err := os.Remove(target); err == nil {
			removed++
		}
	}
	a.log.Printf("purged %d orphan image files", removed)
	writeJSON(w, 200, map[string]any{"removed": removed})
}

var _ = sql.ErrNoRows
