package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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

	prompts, err := a.exportPrompts(r, promptCond, promptArgs)
	if err != nil {
		writeError(w, 500, "query prompts failed")
		return
	}
	images, err := a.exportImages(r, promptCond, promptArgs)
	if err != nil {
		writeError(w, 500, "query images failed")
		return
	}
	var workflows []WorkflowRecord
	var warnings []exportWarning
	if q.Get("with_workflows") != "0" {
		workflows, warnings, err = a.exportWorkflows(r)
		if err != nil {
			writeError(w, 500, "query workflows failed")
			return
		}
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"export-%s.zip\"", time.Now().Format("20060102_150405")))
	zw := zip.NewWriter(w)

	var savedImages []ImageRecord
	for _, img := range images {
		if err := a.writeExportImage(zw, img); err != nil {
			reason := "read_failed"
			if errors.Is(err, fs.ErrNotExist) {
				reason = "missing"
			}
			warnings = append(warnings, exportWarning{Type: "image", ID: img.ID, Name: img.Filename, Reason: reason})
			a.log.Printf("export image %d: %v", img.ID, err)
			continue
		}
		savedImages = append(savedImages, img.ImageRecord)
	}

	for _, entry := range []struct {
		name  string
		value any
	}{
		{"prompts.json", prompts},
		{"images.json", savedImages},
		{"workflows.json", workflows},
	} {
		if err := writeExportJSON(zw, entry.name, entry.value); err != nil {
			a.log.Printf("export %s: %v", entry.name, err)
			break
		}
	}
	if len(warnings) > 0 {
		if err := writeExportJSON(zw, "export-warnings.json", warnings); err != nil {
			a.log.Printf("export warnings: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		a.log.Printf("close export: %v", err)
	}
}

type exportWarning struct {
	Type   string `json:"type"`
	ID     int64  `json:"id,omitempty"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type exportImage struct {
	ImageRecord
	storagePath string
}

func (a *app) exportPrompts(r *http.Request, condition string, args []any) ([]PromptRecord, error) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT p.id, COALESCE(p.title,''), COALESCE(p.description,''), COALESCE(p.positive_prompt,''),
		COALESCE(NULLIF(p.negative_prompt,''),(SELECT w.negative_prompt FROM workflows w JOIN generation_tasks t ON t.workflow_id=w.id JOIN generation_items i ON i.task_id=t.id WHERE i.prompt_id=p.id LIMIT 1),''),
		COALESCE(p.group_name,''), COALESCE(p.group_id,''), COALESCE(p.status,''), p.is_favorite, COALESCE(p.source_platform,'')
		FROM prompts p`+condition+" ORDER BY p.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prompts []PromptRecord
	for rows.Next() {
		var prompt PromptRecord
		var favorite int
		if err := rows.Scan(&prompt.ID, &prompt.Title, &prompt.Description, &prompt.PositivePrompt, &prompt.NegativePrompt, &prompt.GroupName, &prompt.GroupID, &prompt.Status, &favorite, &prompt.SourcePlatform); err != nil {
			return nil, err
		}
		prompt.IsFavorite = favorite == 1
		prompts = append(prompts, prompt)
	}
	return prompts, rows.Err()
}

func (a *app) exportImages(r *http.Request, condition string, args []any) ([]exportImage, error) {
	// 无筛选时保留所有素材，包括提示词已经删除的图片。筛选时沿用提示词条件。
	rows, err := a.db.QueryContext(r.Context(), `SELECT img.id, img.filename, img.storage_path,
		COALESCE(p.id,0), COALESCE(p.positive_prompt,''), img.is_favorite,
		COALESCE(img.source_platform,''), COALESCE(img.image_hash,''), img.width, img.height
		FROM images img
		LEFT JOIN generation_items gi ON gi.id=img.generation_item_id AND img.generation_item_id>0
		LEFT JOIN prompts p ON p.id=COALESCE(NULLIF(img.prompt_id,0),gi.prompt_id)`+condition+" ORDER BY img.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []exportImage
	for rows.Next() {
		var img exportImage
		var favorite int
		if err := rows.Scan(&img.ID, &img.Filename, &img.storagePath, &img.PromptID, &img.PositivePrompt, &favorite,
			&img.SourcePlatform, &img.ImageHash, &img.Width, &img.Height); err != nil {
			return nil, err
		}
		img.IsFavorite = favorite == 1
		img.ArchiveName = fmt.Sprintf("image-%d%s", img.ID, filepath.Ext(safeArchiveName(img.storagePath)))
		images = append(images, img)
	}
	return images, rows.Err()
}

func (a *app) exportWorkflows(r *http.Request) ([]WorkflowRecord, []exportWarning, error) {
	rows, err := a.db.QueryContext(r.Context(), "SELECT id, name, COALESCE(description,''), COALESCE(negative_prompt,''), enabled, is_default, COALESCE(mapping_json,''), workflow_path FROM workflows ORDER BY id")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var workflows []WorkflowRecord
	var warnings []exportWarning
	for rows.Next() {
		var id int64
		var workflow WorkflowRecord
		var mapping, path string
		var enabled, isDefault int
		if err := rows.Scan(&id, &workflow.Name, &workflow.Description, &workflow.NegativePrompt, &enabled, &isDefault, &mapping, &path); err != nil {
			return nil, nil, err
		}
		raw, err := readWorkflowFile(a.workflowFilePath(path))
		if err != nil || !json.Valid(raw) {
			reason := "read_failed"
			if errors.Is(err, fs.ErrNotExist) {
				reason = "missing"
			}
			warnings = append(warnings, exportWarning{Type: "workflow", ID: id, Name: workflow.Name, Reason: reason})
			continue
		}
		if json.Valid([]byte(mapping)) {
			workflow.Mapping = json.RawMessage(mapping)
		}
		workflow.Enabled = enabled == 1
		workflow.IsDefault = isDefault == 1
		workflow.WorkflowJSON = json.RawMessage(raw)
		workflow.Hash = workflowContentHash(raw)
		workflows = append(workflows, workflow)
	}
	return workflows, warnings, rows.Err()
}

func (a *app) writeExportImage(zw *zip.Writer, img exportImage) error {
	file, err := os.Open(a.imageFilePath(img.storagePath))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("image file is empty or is not a regular file")
	}
	// 图片已有压缩，直接写入 ZIP，降低 NAS 导出时的 CPU 开销。
	entry, err := zw.CreateHeader(&zip.FileHeader{Name: "images/" + img.ArchiveName, Method: zip.Store})
	if err != nil {
		return err
	}
	n, err := io.Copy(entry, file)
	if err == nil && n != info.Size() {
		return io.ErrUnexpectedEOF
	}
	return err
}

func writeExportJSON(zw *zip.Writer, name string, value any) error {
	entry, err := zw.Create(name)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(entry)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

const maxImportBytes = 500 << 20

func (a *app) importData(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, 400, "无法读取导入文件")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, "missing file field")
		return
	}
	defer file.Close()
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		writeError(w, 400, "请选择 ZIP 文件")
		return
	}
	zr, err := zip.NewReader(file, header.Size)
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
		var metadata any
		switch f.Name {
		case "prompts.json":
			metadata = &prompts
		case "images.json":
			metadata = &images
		case "workflows.json":
			metadata = &workflows
		default:
			if strings.HasPrefix(f.Name, "images/") {
				if base := safeArchiveName(f.Name); base != "" && !f.FileInfo().IsDir() {
					fileMap[base] = f
				}
			}
		}
		if metadata != nil {
			if err := decodeArchiveMetadata(f, metadata); err != nil {
				writeError(w, 400, "导入清单无法读取："+f.Name)
				return
			}
		}
	}

	// 合并 prompts（按 title + positive_prompt 去重）
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, 500, "begin transaction failed")
		return
	}

	defer tx.Rollback()

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

	importedWorkflows, skippedWorkflows, savedPaths := a.importWorkflows(tx, workflows)

	importedImages := 0
	importedExternal := 0
	skippedImageReasons := map[string]int{}
	var pendingThumbs []thumbJob
	committed := false
	defer func() {
		if !committed {
			for _, path := range savedPaths {
				_ = os.Remove(path)
			}
		}
	}()

	for _, img := range images {
		archiveName := safeArchiveName(img.ArchiveName)
		if archiveName == "" {
			archiveName = safeArchiveName(img.Filename)
		}
		entry := fileMap[archiveName]
		if entry == nil {
			skippedImageReasons["missing"]++
			continue
		}
		displayName := safeArchiveName(img.Filename)
		if displayName == "" {
			displayName = archiveName
		}

		target, hash, config, err := saveArchiveImage(entry, filepath.Join(a.dataDir, "images"))
		if err != nil {
			reason := "unreadable"
			if errors.Is(err, errImportImageWrite) {
				reason = "write_failed"
			}
			skippedImageReasons[reason]++
			a.log.Printf("import image %q: %v", archiveName, err)
			continue
		}
		duplicate, err := a.hasImportedImage(tx, displayName, hash)
		if err != nil {
			_ = os.Remove(target)
			writeError(w, 500, "check image duplicate failed")
			return
		}
		if duplicate {
			_ = os.Remove(target)
			skippedImageReasons["duplicate"]++
			continue
		}

		// 导出包只保留素材和提示词关联，不包含任务历史。
		savedPaths = append(savedPaths, target)
		res, err := tx.Exec("INSERT INTO images(generation_item_id, prompt_id, source_platform, image_hash, filename, storage_path, is_favorite, width, height, created_at) VALUES(0,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP)",
			idMap[img.PromptID], img.SourcePlatform, hash, displayName, target, boolToInt(img.IsFavorite), config.Width, config.Height)
		if err != nil {
			writeError(w, 500, "save imported image failed")
			return
		}
		newID, err := res.LastInsertId()
		if err != nil {
			writeError(w, 500, "save imported image failed")
			return
		}
		importedImages++
		if img.SourcePlatform != "" {
			importedExternal++
		}
		pendingThumbs = append(pendingThumbs, thumbJob{id: newID, src: target})
	}

	if err := tx.Commit(); err != nil {
		writeError(w, 500, "commit failed")
		return
	}
	committed = true
	for _, job := range pendingThumbs {
		a.queueThumbBuild(job.id, job.src)
	}

	writeJSON(w, 200, map[string]any{
		"imported_prompts":         importedPrompts,
		"skipped_prompts":          skippedPrompts,
		"imported_images":          importedImages,
		"skipped_images":           len(images) - importedImages,
		"skipped_image_reasons":    skippedImageReasons,
		"imported_external_images": importedExternal,
		"imported_workflows":       importedWorkflows,
		"skipped_workflows":        skippedWorkflows,
	})
}

func decodeArchiveMetadata(entry *zip.File, target any) error {
	if entry.UncompressedSize64 > maxImportBytes {
		return errors.New("ZIP metadata exceeds import size limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	decoder := json.NewDecoder(io.LimitReader(reader, maxImportBytes+1))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("invalid trailing ZIP metadata")
	}
	return nil
}

var errImportImageWrite = errors.New("write imported image")

func saveArchiveImage(entry *zip.File, dir string) (path, hash string, config image.Config, err error) {
	reader, err := entry.Open()
	if err != nil {
		return "", "", config, err
	}
	defer reader.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", config, fmt.Errorf("%w: %v", errImportImageWrite, err)
	}
	dst, err := os.CreateTemp(dir, "import-*"+filepath.Ext(entry.Name))
	if err != nil {
		return "", "", config, fmt.Errorf("%w: %v", errImportImageWrite, err)
	}
	defer func() {
		_ = dst.Close()
		if err != nil {
			_ = os.Remove(dst.Name())
		}
	}()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, digest), io.LimitReader(reader, maxImportBytes+1))
	if err != nil {
		var pathError *os.PathError
		if errors.As(err, &pathError) {
			err = fmt.Errorf("%w: %v", errImportImageWrite, err)
		}
		return "", "", config, err
	}
	if n > maxImportBytes {
		return "", "", config, errors.New("image exceeds import size limit")
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return "", "", config, err
	}
	config, _, err = image.DecodeConfig(dst)
	if err != nil {
		return "", "", config, err
	}
	if err := dst.Close(); err != nil {
		return "", "", config, fmt.Errorf("%w: %v", errImportImageWrite, err)
	}
	return dst.Name(), fmt.Sprintf("%x", digest.Sum(nil)), config, nil
}

func (a *app) hasImportedImage(tx *sql.Tx, filename, hash string) (bool, error) {
	rows, err := tx.Query("SELECT storage_path, image_hash FROM images WHERE image_hash=? OR (image_hash='' AND filename=?)", hash, filename)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var storagePath, knownHash string
		if err := rows.Scan(&storagePath, &knownHash); err != nil {
			return false, err
		}
		file, err := os.Open(a.imageFilePath(storagePath))
		if err != nil {
			continue
		}
		if knownHash == hash {
			info, err := file.Stat()
			file.Close()
			if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
				return true, nil
			}
			continue
		}
		// 旧版生成的图片没有哈希，读同名文件确认内容是否相同。
		digest := sha256.New()
		_, err = io.Copy(digest, file)
		file.Close()
		if err == nil && fmt.Sprintf("%x", digest.Sum(nil)) == hash {
			return true, nil
		}
	}
	return false, rows.Err()
}

// importWorkflows 把导出包里的工作流写进库 + 盘，返回导入数、跳过数和需要在事务失败时清理的文件。
//
// 两条必须做的收敛：
//   - is_default 一律置 0。目标库若已有默认工作流，再来一个就有两个「默认」，
//     pickWorkflowId（记忆 → 默认 → 首个启用）的结果会变成实现细节。
//   - enabled 保留原值。备份时禁用的，导回来仍是禁用，用户能自己看出来。
func (a *app) importWorkflows(tx *sql.Tx, workflows []WorkflowRecord) (int, int, []string) {
	imported, skipped := 0, 0
	var savedPaths []string
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
		path := filepath.Join(a.dataDir, "workflows", fileName)
		if err := writeAtomic(path, raw); err != nil {
			// 落不了盘就把这行撤掉：留着「有记录、没文件」的工作流，等提交时才炸
			_, _ = tx.Exec(`DELETE FROM workflows WHERE id=?`, id)
			skipped++
			continue
		}
		if _, err := tx.Exec(`UPDATE workflows SET workflow_path=? WHERE id=?`, fileName, id); err != nil {
			_ = os.Remove(path)
			_, _ = tx.Exec(`DELETE FROM workflows WHERE id=?`, id)
			skipped++
			continue
		}
		savedPaths = append(savedPaths, path)
		imported++
	}
	return imported, skipped, savedPaths
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
