package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// errImageNotFound 表示图片不存在。批量接口据此计入 skipped 而非中断。
var errImageNotFound = errors.New("image not found")

// deleteImageByID 删除单张图片：先删磁盘文件，再删库记录。
// 顺序不能反——先删库的话，删文件失败就会留下无法从界面清理的孤儿文件；
// 而现在这种顺序下，删库失败时用户再点一次即可收敛。
func (a *app) deleteImageByID(ctx context.Context, id int64) error {
	var path string
	if err := a.db.QueryRowContext(ctx, "SELECT storage_path FROM images WHERE id=?", id).Scan(&path); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errImageNotFound
		}
		return err
	}
	// 走 imageFilePath 兜底：storage_path 可能是按旧 cwd 拼出的相对路径
	if target := a.imageFilePath(path); target != "" {
		_ = os.Remove(target)
	}
	result, err := a.db.ExecContext(ctx, `DELETE FROM images WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return errImageNotFound
	}
	return nil
}

// setImageFavoriteByID 把图片设为收藏(fav=1)或取消收藏(fav=0)。
func (a *app) setImageFavoriteByID(ctx context.Context, id int64, fav int) error {
	result, err := a.db.ExecContext(ctx, `UPDATE images SET is_favorite=? WHERE id=?`, fav, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return errImageNotFound
	}
	return nil
}

// imageFilePath 把 images.storage_path 解析成可读的真实路径。
// 具体解析顺序（DATA_DIR 优先、原值兜底）见 dataFilePath。
func (a *app) imageFilePath(storagePath string) string {
	return a.dataFilePath(storagePath, "images")
}

func (a *app) listImages(w http.ResponseWriter, r *http.Request) {
	// 收藏过滤。必须带 i. 前缀：链上的 prompts 表同样有 is_favorite 列，
	// 不加前缀会变成 ambiguous column，整个 /api/images?favorite=1 直接 500。
	where := ` WHERE 1=1`
	if r.URL.Query().Get("favorite") == "1" {
		where += ` AND i.is_favorite=1`
	}
	// 统计总数
	var total int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM images i`+where).Scan(&total)
	// 分页
	page, pageSize, offset := parsePagination(r)
	// 左连接出来源提示词：直接提交(manual)的图没有 prompt_id，此时 title 为空
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT i.id, i.generation_item_id, i.filename, i.storage_path, i.is_favorite, i.created_at,
		        COALESCE(gi.prompt_id, 0), COALESCE(p.title, '')
		 FROM images i
		 LEFT JOIN generation_items gi ON gi.id = i.generation_item_id
		 LEFT JOIN prompts p ON p.id = gi.prompt_id`+where+`
		 ORDER BY i.id DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		writeError(w, 500, "query images failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, itemID, isFav, promptID int
		var filename, path, created, promptTitle string
		if err := rows.Scan(&id, &itemID, &filename, &path, &isFav, &created, &promptID, &promptTitle); err != nil {
			writeError(w, 500, "read image failed")
			return
		}
		items = append(items, map[string]any{"id": id, "generation_item_id": itemID, "filename": filename, "path": path, "is_favorite": isFav == 1, "created_at": created, "prompt_id": promptID, "prompt_title": promptTitle})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (a *app) imageFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid image id")
		return
	}
	var path string
	if err := a.db.QueryRowContext(r.Context(), "SELECT storage_path FROM images WHERE id=?", id).Scan(&path); err != nil {
		writeError(w, 404, "image not found")
		return
	}
	// 与删除走同一套解析：否则会出现「能删掉、却显示 404」这种同一条数据三种行为
	target := a.imageFilePath(path)
	if target == "" {
		writeError(w, 404, "image file not found")
		return
	}
	http.ServeFile(w, r, target)
}

func (a *app) getImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid image id")
		return
	}
	var itemID, isFav, promptID int
	var filename, path, created, promptTitle, positive, negative string
	// 一次左连接取全：图片 → 任务项 → 任务 → 工作流（负向词），以及任务项 → 提示词（title）。
	// 用 LEFT JOIN 是因为直接提交的图可能没有关联提示词/工作流，INNER JOIN 会整行丢失。
	err = a.db.QueryRowContext(r.Context(),
		`SELECT i.generation_item_id, i.filename, i.storage_path, i.is_favorite, i.created_at,
		        COALESCE(gi.prompt_id, 0), COALESCE(p.title, ''),
		        COALESCE(gi.positive_prompt, ''), COALESCE(w.negative_prompt, '')
		 FROM images i
		 LEFT JOIN generation_items gi ON gi.id = i.generation_item_id
		 LEFT JOIN generation_tasks gt ON gt.id = gi.task_id
		 LEFT JOIN workflows w ON w.id = gt.workflow_id
		 LEFT JOIN prompts p ON p.id = gi.prompt_id
		 WHERE i.id=?`, id).
		Scan(&itemID, &filename, &path, &isFav, &created, &promptID, &promptTitle, &positive, &negative)
	if err != nil {
		writeError(w, 404, "image not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "generation_item_id": itemID, "filename": filename, "storage_path": path, "positive_prompt": positive, "negative_prompt": negative, "is_favorite": isFav == 1, "created_at": created, "prompt_id": promptID, "prompt_title": promptTitle})
}

func (a *app) downloadImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid image id")
		return
	}
	var filename, path string
	if err := a.db.QueryRowContext(r.Context(), "SELECT filename, storage_path FROM images WHERE id=?", id).Scan(&filename, &path); err != nil {
		writeError(w, 404, "image not found")
		return
	}
	// 与 /file、删除走同一套解析，否则同一条数据会出现「能看、能删、但下载 404」
	target := a.imageFilePath(path)
	if target == "" {
		writeError(w, 404, "image file not found")
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(filename, "\"", "_")+"\"")
	http.ServeFile(w, r, target)
}

func (a *app) deleteImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid image id")
		return
	}
	if err := a.deleteImageByID(r.Context(), id); err != nil {
		if errors.Is(err, errImageNotFound) {
			writeError(w, 404, "image not found")
			return
		}
		writeError(w, 500, "delete image failed")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "deleted": true})
}

func (a *app) batchDeleteImages(w http.ResponseWriter, r *http.Request) {
	a.runIDBatch(w, r, a.deleteImageByID, isImageSkippable)
}

// batchFavoriteImages 批量设置收藏状态，body 额外带 favorite（true=收藏）。
// 与删除不同，这是纯字段更新，不需要逐条读盘，但仍走同一套 id 校验与计数。
func (a *app) batchFavoriteImages(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs      []int64 `json:"ids"`
		Favorite bool    `json:"favorite"`
	}
	if err := decodeJSON(r, &input); err != nil || len(input.IDs) == 0 {
		writeError(w, 400, "ids is required")
		return
	}
	if len(input.IDs) > maxBatchIDs {
		writeError(w, 400, "too many ids, max 500 per request")
		return
	}
	fav := 0
	if input.Favorite {
		fav = 1
	}
	a.execIDBatch(w, r.Context(), input.IDs, func(ctx context.Context, id int64) error {
		return a.setImageFavoriteByID(ctx, id, fav)
	}, isImageSkippable)
}

// isImageSkippable 判断批量操作中的错误是否只是"这条跳过"，而非服务端故障。
func isImageSkippable(err error) bool {
	return errors.Is(err, errImageNotFound)
}

func (a *app) toggleImageFavorite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid image id")
		return
	}
	var cur int
	if err := a.db.QueryRowContext(r.Context(), `SELECT is_favorite FROM images WHERE id=?`, id).Scan(&cur); err != nil {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	fav := 1 - cur
	if err := a.setImageFavoriteByID(r.Context(), id, fav); err != nil {
		if errors.Is(err, errImageNotFound) {
			writeError(w, http.StatusNotFound, "image not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "toggle favorite failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "is_favorite": fav == 1})
}
