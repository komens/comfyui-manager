package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 外部平台同步：把别的平台上生成的「图 + 提示词」单条录入本站。
// 关键差异：外部图没有 ComfyUI 生成链路，因此
//   images.generation_item_id = 0（无生成项）
//   images.prompt_id         = 新建的提示词 id（直连通道）
//   images.source_platform   = 平台标识
// 图库/提示词/收藏/导出都必须走 COALESCE(NULLIF(i.prompt_id,0), gi.prompt_id) 这个表达式，
// 否则这类数据在图库里会显示成「手动提交」、收藏报不存在、导出直接丢失。

// platformLocal 是筛选用的伪平台标识，指「来源为空」的本平台数据。
const platformLocal = "local"

type externalPlatform struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Color string `json:"color"`
}

// externalPlatforms 是平台白名单。加平台只改这张表，前端从接口拉，不硬编码。
var externalPlatforms = []externalPlatform{
	{ID: "chatgpt", Label: "ChatGPT", Color: "#10a37f"},
	{ID: "grok", Label: "Grok", Color: "#1d1d1f"},
}

func findPlatform(id string) (externalPlatform, bool) {
	for _, p := range externalPlatforms {
		if p.ID == id {
			return p, true
		}
	}
	return externalPlatform{}, false
}

// maxExternalImageBytes 单张外部图片上限。25MB 足够覆盖各家平台的原图导出。
const maxExternalImageBytes = 25 << 20

// externalImageExt 按嗅探到的 MIME 决定落盘扩展名。
// 用 DetectContentType 而不是信任上传文件名：扩展名可以随手改，字节头不会。
var externalImageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

func (a *app) listExternalPlatforms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": externalPlatforms})
}

// createExternalImage 处理单条外部平台素材录入（multipart）：
// platform + positive_prompt + title(可选) + file。
func (a *app) createExternalImage(w http.ResponseWriter, r *http.Request) {
	// 硬上限放在 ParseMultipartForm 之前：否则大文件会先被整个写进临时文件
	r.Body = http.MaxBytesReader(w, r.Body, maxExternalImageBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, 400, "invalid multipart form or file too large")
		return
	}
	platformID := strings.TrimSpace(r.FormValue("platform"))
	positive := strings.TrimSpace(r.FormValue("positive_prompt"))
	title := strings.TrimSpace(r.FormValue("title"))

	platform, ok := findPlatform(platformID)
	if !ok {
		writeError(w, 400, "unknown platform")
		return
	}
	if positive == "" {
		writeError(w, 400, "positive_prompt is required")
		return
	}
	if len(positive) > 20000 {
		writeError(w, 400, "positive_prompt too long (max 20000 bytes)")
		return
	}
	if len(title) > 200 {
		title = truncate(title, 200)
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, "file is required")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxExternalImageBytes+1))
	if err != nil {
		writeError(w, 400, "read file failed")
		return
	}
	if len(data) == 0 {
		writeError(w, 400, "file is empty")
		return
	}
	if len(data) > maxExternalImageBytes {
		writeError(w, 400, fmt.Sprintf("file too large (max %d MB)", maxExternalImageBytes>>20))
		return
	}

	mime := http.DetectContentType(data)
	ext, ok := externalImageExt[mime]
	if !ok {
		writeError(w, 400, "unsupported image type, only png/jpeg/gif/webp are allowed")
		return
	}

	// sha256 去重：同一张图重复添加没有意义，而且会让图库出现完全相同的两张。
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	var existingID int64
	if err := a.db.QueryRowContext(r.Context(),
		`SELECT id FROM images WHERE image_hash=? LIMIT 1`, hash).Scan(&existingID); err == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("这张图已在素材库中（#%d）", existingID))
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, 500, "duplicate check failed")
		return
	}

	now := time.Now()
	// 文件名带平台与时间戳：图库按文件名搜索时能按平台捞到，导出后也一眼看得出处。
	filename := fmt.Sprintf("ext_%s_%s_%s%s", platform.ID, now.Format("20060102_150405"), randomHex(2), ext)
	target := filepath.Join(a.dataDir, "images", filename)
	if err := writeAtomic(target, data); err != nil {
		writeError(w, 500, "save image file failed")
		return
	}

	// 尺寸从内存字节解，与 ComfyUI 出图走同一套逻辑（只读文件头）
	imgW, imgH := imageSizeFromBytes(data)
	if title == "" {
		title = truncate(positive, 50)
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		_ = os.Remove(target)
		writeError(w, 500, "create import failed")
		return
	}
	// status 直接给 completed：这条提示词一诞生就有成品图，显示「待生成」会让人以为要重跑。
	promptResult, err := tx.ExecContext(r.Context(),
		`INSERT INTO prompts(title, description, positive_prompt, group_name, group_id, status, source_platform, completed_at, created_at, updated_at)
		 VALUES(?, '', ?, '', '', 'completed', ?, ?, ?, ?)`,
		title, positive, platform.ID, now, now, now)
	if err != nil {
		_ = tx.Rollback()
		_ = os.Remove(target)
		writeError(w, 500, "create prompt failed")
		return
	}
	promptID, _ := promptResult.LastInsertId()

	imageResult, err := tx.ExecContext(r.Context(),
		`INSERT INTO images(generation_item_id, prompt_id, source_platform, image_hash, filename, storage_path, width, height, created_at)
		 VALUES(0, ?, ?, ?, ?, ?, ?, ?, ?)`,
		promptID, platform.ID, hash, filename, target, imgW, imgH, now)
	if err != nil {
		_ = tx.Rollback()
		_ = os.Remove(target)
		writeError(w, 500, "create image failed")
		return
	}
	imageID, _ := imageResult.LastInsertId()

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		_ = os.Remove(target)
		writeError(w, 500, "commit failed")
		return
	}

	// 事务提交后再排队：回滚的图不该留下缩略图
	if imgW > 0 && imgH > 0 {
		a.queueThumbBuild(imageID, target)
	}
	a.log.Printf("external image imported: id=%d platform=%s prompt=%d size=%dx%d file=%s (%d bytes)",
		imageID, platform.ID, promptID, imgW, imgH, filename, len(data))

	writeJSON(w, http.StatusCreated, map[string]any{
		"image_id":        imageID,
		"prompt_id":       promptID,
		"platform":        platform.ID,
		"platform_label":  platform.Label,
		"filename":        filename,
		"width":           imgW,
		"height":          imgH,
		"source_filename": header.Filename,
	})
}

// randomHex 生成 n 字节随机数的十六进制串，用于文件名避免同秒冲突。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// 随机源不可用时退回时间纳秒，只影响文件名唯一性，不影响数据正确性
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(buf)
}
