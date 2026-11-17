package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ExportRecord 导出数据结构
type ExportRecord struct {
	Prompts   []PromptRecord   `json:"prompts"`
	Images    []ImageRecord    `json:"images"`
	Workflows []WorkflowRecord `json:"workflows"`
}

type PromptRecord struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	PositivePrompt string `json:"positive_prompt"`
	NegativePrompt string `json:"negative_prompt"`
	GroupName      string `json:"group_name"`
	GroupID        string `json:"group_id"` // prompts.group_id 是 TEXT 列，不能按整数扫描
	Status         string `json:"status"`
	IsFavorite     bool   `json:"is_favorite"`
	// 来源平台：空=本平台。旧包没有这个字段，导入端按空处理。
	SourcePlatform string `json:"source_platform,omitempty"`
}

type ImageRecord struct {
	ID             int64  `json:"id"`
	Filename       string `json:"filename"`               // 展示/下载用的文件名（通常是 ComfyUI 原始名）
	ArchiveName    string `json:"archive_name,omitempty"` // ZIP 内的实际文件名，与 filename 不同时才写入
	PromptID       int64  `json:"prompt_id"`              // 原始 prompt ID，导入时需要映射
	PositivePrompt string `json:"positive_prompt"`        // 冗余存储，方便导入时关联
	IsFavorite     bool   `json:"is_favorite"`
	// 外部平台同步的图没有生成项，全靠这几项还原：来源标识、去重 hash、原始宽高
	SourcePlatform string `json:"source_platform,omitempty"`
	ImageHash      string `json:"image_hash,omitempty"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
}

// WorkflowRecord 是导出包里的工作流。
//
// workflow_json 直接内嵌，不按 ZIP 条目搬文件：库里只存 workflow_path，而文件名是
// `<安全名>-<id>.json`，导入端 id 必然变，按文件搬就得改名 + 防撞名 + 防 zip slip。
// 内嵌 JSON 交给导入端用本地 id 重新落盘，这几件事一并消失。
//
// Hash 只用于「同名同内容」判重（见 workflowContentHash），不含 name。
type WorkflowRecord struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	NegativePrompt string          `json:"negative_prompt"`
	Enabled        bool            `json:"enabled"`
	IsDefault      bool            `json:"is_default"`
	Mapping        json.RawMessage `json:"mapping,omitempty"`
	WorkflowJSON   json.RawMessage `json:"workflow_json"`
	Hash           string          `json:"hash,omitempty"`
}

// workflowContentHash 是工作流内容指纹：先去掉格式空白再取 sha256。
//
// 必须规范化后再算——导出时 json.MarshalIndent 会重排缩进，直接对原始字节取哈希
// 会让「同一个工作流」在导入端算不出同一个值，去重就永远不命中。
func workflowContentHash(raw []byte) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return ""
	}
	sum := sha256.Sum256(compact.Bytes())
	return fmt.Sprintf("%x", sum)
}

func (a *app) exportData(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	favorite := q.Get("favorite") == "1"
	groupName := q.Get("group")
	status := q.Get("status")

	// 构建 prompts 查询
	var promptWhere []string
	var promptArgs []any
	if favorite {
		promptWhere = append(promptWhere, "p.is_favorite=1")
	}
	if groupName != "" {
		promptWhere = append(promptWhere, "p.group_name=?")
		promptArgs = append(promptArgs, groupName)
	}
	if status != "" {
		promptWhere = append(promptWhere, "p.status=?")
		promptArgs = append(promptArgs, status)
	}
	if search := q.Get("search"); search != "" {
		promptWhere = append(promptWhere, "(p.title LIKE ? OR p.positive_prompt LIKE ? OR p.description LIKE ?)")
		s := "%" + search + "%"
		promptArgs = append(promptArgs, s, s, s)
	}
	// 来源筛选：local 指本平台（source_platform 为空），其余按平台标识精确匹配
	if platform := q.Get("platform"); platform != "" {
		if platform == platformLocal {
			promptWhere = append(promptWhere, "COALESCE(p.source_platform,'')=''")
		} else {
			promptWhere = append(promptWhere, "p.source_platform=?")
			promptArgs = append(promptArgs, platform)
		}
	}
	promptCond := ""
	if len(promptWhere) > 0 {
		promptCond = " WHERE " + strings.Join(promptWhere, " AND ")
	}

	// 查询 prompts：负面优先取提示词自带列，没有则回落到关联工作流的静态负面（保留旧导出行为）。
	// ⚠️ 子查询里的 negative_prompt 必须带 w. 前缀：这是相关子查询，外层 p 和内层 w 都有
	// 同名列，不限定就报 ambiguous column name，整个导出接口直接 500。
	rows, err := a.db.Query(`SELECT p.id, COALESCE(p.title,''), COALESCE(p.description,''), COALESCE(p.positive_prompt,''), COALESCE(NULLIF(p.negative_prompt,''),(SELECT w.negative_prompt FROM workflows w JOIN generation_tasks t ON t.workflow_id=w.id JOIN generation_items i ON i.task_id=t.id WHERE i.prompt_id=p.id LIMIT 1),''), COALESCE(p.group_name,''), COALESCE(p.group_id,''), COALESCE(p.status,''), p.is_favorite, COALESCE(p.source_platform,'') FROM prompts p`+promptCond, promptArgs...)
	if err != nil {
		writeError(w, 500, "query prompts failed")
		return
	}
	defer rows.Close()

	var prompts []PromptRecord
	var promptIDs []int64
	for rows.Next() {
		var p PromptRecord
		var fav int
		if err := rows.Scan(&p.ID, &p.Title, &p.Description, &p.PositivePrompt, &p.NegativePrompt, &p.GroupName, &p.GroupID, &p.Status, &fav, &p.SourcePlatform); err != nil {
			continue
		}
		p.IsFavorite = fav == 1
		prompts = append(prompts, p)
		promptIDs = append(promptIDs, p.ID)
	}

	// 查询关联图片
	var images []ImageRecord
	imageStorage := map[int64]string{} // image id -> 磁盘路径
	if len(promptIDs) > 0 {
		// 构建 IN 子句
		placeholders := make([]string, len(promptIDs))
		imgArgs := make([]any, len(promptIDs))
		for i, id := range promptIDs {
			placeholders[i] = "?"
			imgArgs[i] = id
		}
		imgCond := strings.Join(placeholders, ",")

		// 双通道取图：本平台图走 generation_items，外部同步图走 images.prompt_id。
		// 原来这里是 JOIN generation_items，外部图（generation_item_id=0）会被整批漏掉——
		// 包里有提示词条目、却没有对应图片文件，导入回来就是「有词无图」。
		imgRows, err := a.db.Query(`SELECT img.id, img.filename, img.storage_path,
				COALESCE(NULLIF(img.prompt_id,0), gi.prompt_id, 0), COALESCE(p.positive_prompt,''), img.is_favorite,
				COALESCE(img.source_platform,''), COALESCE(img.image_hash,''), img.width, img.height
			FROM images img
			LEFT JOIN generation_items gi ON gi.id=img.generation_item_id AND img.generation_item_id > 0
			LEFT JOIN prompts p ON p.id=COALESCE(NULLIF(img.prompt_id,0), gi.prompt_id)
			WHERE COALESCE(NULLIF(img.prompt_id,0), gi.prompt_id) IN (`+imgCond+`)`, imgArgs...)
		if err == nil {
			defer imgRows.Close()
			for imgRows.Next() {
				var img ImageRecord
				var storage string
				var fav int
				if err := imgRows.Scan(&img.ID, &img.Filename, &storage, &img.PromptID, &img.PositivePrompt, &fav,
					&img.SourcePlatform, &img.ImageHash, &img.Width, &img.Height); err == nil {
					img.IsFavorite = fav == 1
					// filename 与磁盘上的实际文件名常常不一致（下载时被重命名为 <时间戳>_<id>.png），
					// ZIP 条目必须以磁盘文件名为准，否则导入侧按 filename 找不到文件。
					archive := safeArchiveName(storage)
					if archive != "" && archive != img.Filename {
						img.ArchiveName = archive
					}
					imageStorage[img.ID] = storage
					images = append(images, img)
				}
			}
		}
	}

	// 工作流：JSON 本体在 data/workflows/ 下（库里只存 workflow_path），必须读盘内嵌。
	// 读不到文件或内容坏掉的跳过并记日志——导出半个工作流比不导出更糟：导入端会得到
	// 一条「有记录、打不开」的空壳，等提交时才发现。
	// with_workflows=0 可关掉，兼容只想要素材的旧调用方。
	var exportWorkflows []WorkflowRecord
	if q.Get("with_workflows") != "0" {
		wrows, werr := a.db.Query(`SELECT name, COALESCE(description,''), COALESCE(negative_prompt,''), enabled, is_default, COALESCE(mapping_json,''), workflow_path FROM workflows ORDER BY id`)
		if werr == nil {
			defer wrows.Close()
			for wrows.Next() {
				var name, desc, negative, mapping, path string
				var enabled, isDefault int
				if err := wrows.Scan(&name, &desc, &negative, &enabled, &isDefault, &mapping, &path); err != nil {
					continue
				}
				raw, err := readWorkflowFile(a.workflowFilePath(path))
				if err != nil || len(raw) == 0 {
					a.log.Printf("export: 跳过工作流 %q（读不到 %s：%v）", name, path, err)
					continue
				}
				mappingJSON := json.RawMessage(mapping)
				if !json.Valid(mappingJSON) {
					mappingJSON = nil
				}
				exportWorkflows = append(exportWorkflows, WorkflowRecord{
					Name:           name,
					Description:    desc,
					NegativePrompt: negative,
					Enabled:        enabled == 1,
					IsDefault:      isDefault == 1,
					Mapping:        mappingJSON,
					WorkflowJSON:   json.RawMessage(raw),
					Hash:           workflowContentHash(raw),
				})
			}
		}
	}

	export := ExportRecord{Prompts: prompts, Images: images, Workflows: exportWorkflows}

	// 创建 ZIP
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="export-%s.zip"`, time.Now().Format("20060102_150405")))

	zw := zip.NewWriter(w)
	defer zw.Close()

	// 写入 prompts.json
	pj, _ := json.MarshalIndent(export.Prompts, "", "  ")
	pw, _ := zw.Create("prompts.json")
	pw.Write(pj)

	// 写入 images.json
	ij, _ := json.MarshalIndent(export.Images, "", "  ")
	iw, _ := zw.Create("images.json")
	iw.Write(ij)

	// 写入 workflows.json（JSON 内嵌，见 WorkflowRecord 的说明）
	wj, _ := json.MarshalIndent(export.Workflows, "", "  ")
	ww, _ := zw.Create("workflows.json")
	ww.Write(wj)

	// 写入图片文件
	for _, img := range images {
		srcPath := a.imageFilePath(imageStorage[img.ID])
		if srcPath == "" {
			continue
		}
		srcFile, err := os.Open(srcPath)
		if err != nil {
			continue
		}
		entryName := safeArchiveName(img.ArchiveName)
		if entryName == "" {
			entryName = safeArchiveName(img.Filename)
		}
		if entryName == "" {
			// 名字不可用（空或含路径分隔符）——宁可少写一个条目，也不往 ZIP 里塞可疑路径
			srcFile.Close()
			continue
		}
		fw, err := zw.Create("images/" + entryName)
		if err != nil {
			srcFile.Close()
			continue
		}
		_, _ = io.Copy(fw, srcFile)
		srcFile.Close()
	}
}

func (a *app) importData(w http.ResponseWriter, r *http.Request) {
	// 解析 multipart form（最大 500MB）
	r.ParseMultipartForm(500 << 20)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, "missing file field")
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		writeError(w, 400, "only .zip files are supported")
		return
	}

	// 读取 ZIP 到临时文件（zip.NewReader 需要 ReaderAt）
	tmpFile, err := os.CreateTemp("", "import-*.zip")
	if err != nil {
		writeError(w, 500, "create temp file failed")
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()
	io.Copy(tmpFile, file)
	tmpFile.Seek(0, 0)

	stat, _ := tmpFile.Stat()
	zr, err := zip.NewReader(tmpFile, stat.Size())
	if err != nil {
		writeError(w, 400, "invalid zip file")
		return
	}

	// 解析 ZIP 内容
	var prompts []PromptRecord
	var images []ImageRecord
	var workflows []WorkflowRecord
	fileMap := make(map[string]*zip.File) // filename -> zip file entry

	for _, f := range zr.File {
		switch {
		case f.Name == "prompts.json":
			rc, _ := f.Open()
			json.NewDecoder(rc).Decode(&prompts)
			rc.Close()
		case f.Name == "images.json":
			rc, _ := f.Open()
			json.NewDecoder(rc).Decode(&images)
			rc.Close()
		case f.Name == "workflows.json":
			// 旧包没有这个条目：workflows 保持空，行为与改动前一致
			rc, _ := f.Open()
			json.NewDecoder(rc).Decode(&workflows)
			rc.Close()
		case strings.HasPrefix(f.Name, "images/"):
			if base := safeArchiveName(f.Name); base != "" {
				fileMap[base] = f
			}
		}
	}

	// 合并 prompts（按 title + positive_prompt 去重）
	tx, err := a.db.Begin()
	if err != nil {
		writeError(w, 500, "begin transaction failed")
		return
	}

	importedPrompts := 0
	skippedPrompts := 0
	idMap := make(map[int64]int64) // 旧ID -> 新ID

	for _, p := range prompts {
		// 检查重复：相同 title + positive_prompt + 来源。
		// 来源必须进去重键——两个平台可能撞出完全一样的提示词，合并会让来源标识丢失。
		var existingID int64
		err := tx.QueryRow(`SELECT id FROM prompts WHERE title=? AND positive_prompt=? AND COALESCE(source_platform,'')=?`,
			p.Title, p.PositivePrompt, p.SourcePlatform).Scan(&existingID)
		if err == nil {
			// 已存在，记录映射
			idMap[p.ID] = existingID
			skippedPrompts++
			continue
		}

		// 插入新 prompt
		res, err := tx.Exec(`INSERT INTO prompts(title, description, positive_prompt, negative_prompt, group_name, group_id, status, is_favorite, source_platform, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
			p.Title, p.Description, p.PositivePrompt, p.NegativePrompt, p.GroupName, p.GroupID, importedPromptStatus(p.Status), boolToInt(p.IsFavorite), p.SourcePlatform)
		if err != nil {
			skippedPrompts++
			continue
		}
		newID, _ := res.LastInsertId()
		idMap[p.ID] = newID
		importedPrompts++
	}

	// 合并 workflows（按 name 判重；同名不同内容则加后缀共存）
	// 放在 prompts 之后没有依赖关系，只是让响应里的计数顺序和打包顺序一致。
	importedWorkflows, skippedWorkflows := a.importWorkflows(tx, workflows)

	// 合并 images（按 filename 去重）
	importedImages := 0
	skippedImages := 0
	importedExternal := 0
	// 事务里插入成功的图片，提交后统一丢进缩略图队列
	pendingThumbs := make([]thumbJob, 0, 16)

	for _, img := range images {
		// ZIP 内的实际文件名：优先 archive_name，兼容旧导出包仅有 filename 的情况。
		// 这两个字段都来自 ZIP 内的 images.json，是**不可信输入**，必须收敛成纯文件名——
		// 否则构造过的包可以用 archive_name: "../../../.ssh/authorized_keys"
		// 把文件写到 data/images/ 之外的任意路径（zip slip）。
		archiveName := safeArchiveName(img.ArchiveName)
		if archiveName == "" {
			archiveName = safeArchiveName(img.Filename)
		}
		if archiveName == "" {
			skippedImages++
			continue
		}
		displayName := safeArchiveName(img.Filename)
		if displayName == "" {
			displayName = archiveName
		}

		// 检查文件是否已存在
		var existingID int64
		err := tx.QueryRow(`SELECT id FROM images WHERE filename=?`, displayName).Scan(&existingID)
		if err == nil {
			skippedImages++
			continue
		}

		// 查找对应的 prompt（通过映射后的 ID）
		newPromptID := idMap[img.PromptID]

		// 查找关联的 generation_item（仅本平台出图有）。外部同步图找不到，
		// 但 prompt_id 会直连写入，图库/详情/搜索照样能用。
		var itemID int64
		if newPromptID > 0 {
			tx.QueryRow(`SELECT id FROM generation_items WHERE prompt_id=? ORDER BY id DESC LIMIT 1`, newPromptID).Scan(&itemID)
		}

		// 写入图片文件
		// archiveName 已被 safeArchiveName 收敛成纯文件名，落点必然在 data/images/ 之内
		target := filepath.Join(a.dataDir, "images", archiveName)
		if zf, ok := fileMap[archiveName]; ok {
			rc, err := zf.Open()
			if err == nil {
				if dst, createErr := os.Create(target); createErr == nil {
					_, _ = io.Copy(dst, rc)
					dst.Close()
				}
				rc.Close()
			}
		}

		// 插入图片记录。宽高优先用包内记录的值（省一次解码）；旧包没带就现读文件头，
		// 都拿不到则留 0，由 backfillImageSizes 回填。
		imgW, imgH := img.Width, img.Height
		if imgW <= 0 || imgH <= 0 {
			imgW, imgH = imageSizeFromFile(target)
		}
		res, err := tx.Exec(`INSERT INTO images(generation_item_id, prompt_id, source_platform, image_hash, filename, storage_path, is_favorite, width, height, created_at) VALUES(?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP)`,
			itemID, newPromptID, img.SourcePlatform, img.ImageHash, displayName, target, boolToInt(img.IsFavorite), imgW, imgH)
		if err != nil {
			skippedImages++
			continue
		}
		importedImages++
		if img.SourcePlatform != "" {
			importedExternal++
		}
		if newID, idErr := res.LastInsertId(); idErr == nil && imgW > 0 && imgH > 0 {
			pendingThumbs = append(pendingThumbs, thumbJob{id: newID, src: target})
		}
	}

	if err := tx.Commit(); err != nil {
		tx.Rollback()
		writeError(w, 500, "commit failed")
		return
	}

	// 事务提交后再排队：回滚的图不该留下缩略图
	for _, job := range pendingThumbs {
		a.queueThumbBuild(job.id, job.src)
	}

	writeJSON(w, 200, map[string]any{
		"imported_prompts":         importedPrompts,
		"skipped_prompts":          skippedPrompts,
		"imported_images":          importedImages,
		"skipped_images":           skippedImages,
		"imported_external_images": importedExternal,
		"imported_workflows":       importedWorkflows,
		"skipped_workflows":        skippedWorkflows,
	})
}

// importWorkflows 把导出包里的工作流写进库 + 盘，返回 (导入, 跳过)。
//
// 两条必须做的收敛：
//   - is_default 一律置 0。目标库若已有默认工作流，再来一个就有两个「默认」，
//     pickWorkflowId（记忆 → 默认 → 首个启用）的结果会变成实现细节。
//   - enabled 保留原值。备份时禁用的，导回来仍是禁用，用户能自己看出来。
func (a *app) importWorkflows(tx *sql.Tx, workflows []WorkflowRecord) (int, int) {
	imported, skipped := 0, 0
	for _, wf := range workflows {
		name := strings.TrimSpace(wf.Name)
		if name == "" || len(wf.WorkflowJSON) == 0 {
			skipped++
			continue
		}
		// normalizeWorkflowJSON 兼容「JSON 被当字符串又包了一层」的旧包
		raw := normalizeWorkflowJSON(wf.WorkflowJSON)
		if len(raw) == 0 || !json.Valid(raw) {
			skipped++
			continue
		}
		finalName, duplicate := a.resolveImportedWorkflowName(tx, name, workflowContentHash(raw))
		if duplicate {
			skipped++
			continue
		}
		// 导入即用：编辑器没选的那一侧由自动识别补上，与 createWorkflow 一致
		mapping := mergePromptMapping(wf.Mapping, raw)
		if len(mapping) == 0 {
			mapping = json.RawMessage(`{}`)
		}
		now := time.Now()
		res, err := tx.Exec(`INSERT INTO workflows(name, description, workflow_path, mapping_json, negative_prompt, enabled, is_default, created_at, updated_at) VALUES(?,?,?,?,?,?,0,?,?)`,
			finalName, wf.Description, "", string(mapping), wf.NegativePrompt, boolToInt(wf.Enabled), now, now)
		if err != nil {
			skipped++
			continue
		}
		id, _ := res.LastInsertId()
		fileName := workflowFileName(finalName, id)
		if err := writeAtomic(filepath.Join(a.dataDir, "workflows", fileName), raw); err != nil {
			// 落不了盘就把这行撤掉：留着「有记录、没文件」的工作流，等提交时才炸
			_, _ = tx.Exec(`DELETE FROM workflows WHERE id=?`, id)
			skipped++
			continue
		}
		if _, err := tx.Exec(`UPDATE workflows SET workflow_path=? WHERE id=?`, fileName, id); err != nil {
			_, _ = tx.Exec(`DELETE FROM workflows WHERE id=?`, id)
			skipped++
			continue
		}
		imported++
	}
	return imported, skipped
}

// resolveImportedWorkflowName 给出一个不与现有工作流撞名的名字，并判断是否为重复。
//
// 名字被占用时读对方文件比内容指纹：一样就是同一个工作流（duplicate=true）；
// 不一样就试 `name (2)`、`name (3)`……宁可多一条带后缀的副本，也不静默吞掉导入的数据。
func (a *app) resolveImportedWorkflowName(tx *sql.Tx, name, hash string) (string, bool) {
	for i := 1; i <= 100; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s (%d)", name, i)
		}
		var path string
		if err := tx.QueryRow(`SELECT workflow_path FROM workflows WHERE name=? LIMIT 1`, candidate).Scan(&path); err != nil {
			return candidate, false
		}
		existing, readErr := readWorkflowFile(a.workflowFilePath(path))
		if readErr == nil && hash != "" && workflowContentHash(existing) == hash {
			return candidate, true
		}
	}
	return name, true
}

// importedPromptStatus 收敛导入的提示词状态。
//
// running / submitted / queued 是「有任务正在跑」的瞬时态：新库没有那条任务，
// 原样导入会让列表永远显示「生成中」。其余状态（completed / failed / pending）
// 只是静态标签，保留原值。
func importedPromptStatus(status string) string {
	switch status {
	case "running", "submitted", "queued":
		return "pending"
	}
	return status
}

// safeArchiveName 把（来自 ZIP 成员的）文件名收敛成纯文件名，防住 zip slip。
// 返回空串表示这个名字不可用——空值、"." / ".."，或仍残留路径分隔符（含 Windows 的 "\"）。
// 调用方遇到空串应跳过该条目，而不是退回原值。
func safeArchiveName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, `/\`) {
		return ""
	}
	return base
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
