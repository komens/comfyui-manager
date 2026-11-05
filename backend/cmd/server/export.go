package main

import (
	"archive/zip"
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
	Prompts []PromptRecord `json:"prompts"`
	Images  []ImageRecord  `json:"images"`
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

	// 查询 prompts
	rows, err := a.db.Query(`SELECT p.id, COALESCE(p.title,''), COALESCE(p.description,''), COALESCE(p.positive_prompt,''), COALESCE((SELECT negative_prompt FROM workflows w JOIN generation_tasks t ON t.workflow_id=w.id JOIN generation_items i ON i.task_id=t.id WHERE i.prompt_id=p.id LIMIT 1),''), COALESCE(p.group_name,''), COALESCE(p.group_id,''), COALESCE(p.status,''), p.is_favorite, COALESCE(p.source_platform,'') FROM prompts p`+promptCond, promptArgs...)
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

	export := ExportRecord{Prompts: prompts, Images: images}

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
		res, err := tx.Exec(`INSERT INTO prompts(title, description, positive_prompt, group_name, group_id, status, is_favorite, source_platform, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
			p.Title, p.Description, p.PositivePrompt, p.GroupName, p.GroupID, p.Status, boolToInt(p.IsFavorite), p.SourcePlatform)
		if err != nil {
			skippedPrompts++
			continue
		}
		newID, _ := res.LastInsertId()
		idMap[p.ID] = newID
		importedPrompts++
	}

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
	})
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
