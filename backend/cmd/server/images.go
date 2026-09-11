package main

import (
	"net/http"
	"os"
	"strconv"
	"strings"
)

func (a *app) listImages(w http.ResponseWriter, r *http.Request) {
	// 收藏过滤
	where := ` WHERE 1=1`
	if r.URL.Query().Get("favorite") == "1" {
		where += ` AND is_favorite=1`
	}
	// 统计总数
	var total int
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM images`+where).Scan(&total)
	// 分页
	page, pageSize, offset := parsePagination(r)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, generation_item_id, filename, storage_path, is_favorite, created_at FROM images`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		writeError(w, 500, "query images failed")
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id, itemID, isFav int
		var filename, path, created string
		if err := rows.Scan(&id, &itemID, &filename, &path, &isFav, &created); err != nil {
			writeError(w, 500, "read image failed")
			return
		}
		items = append(items, map[string]any{"id": id, "generation_item_id": itemID, "filename": filename, "path": path, "is_favorite": isFav == 1, "created_at": created})
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
	http.ServeFile(w, r, path)
}

func (a *app) getImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid image id")
		return
	}
	var itemID, isFav int
	var filename, path, created string
	err = a.db.QueryRowContext(r.Context(), `SELECT generation_item_id, filename, storage_path, is_favorite, created_at FROM images WHERE id=?`, id).Scan(&itemID, &filename, &path, &isFav, &created)
	if err != nil {
		writeError(w, 404, "image not found")
		return
	}
	var positive, negative string
	_ = a.db.QueryRowContext(r.Context(),
		`SELECT gi.positive_prompt, COALESCE(w.negative_prompt,'')
		 FROM generation_items gi
		 JOIN generation_tasks gt ON gi.task_id=gt.id
		 JOIN workflows w ON gt.workflow_id=w.id
		 WHERE gi.id=?`, itemID).Scan(&positive, &negative)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "generation_item_id": itemID, "filename": filename, "storage_path": path, "positive_prompt": positive, "negative_prompt": negative, "is_favorite": isFav == 1, "created_at": created})
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
	w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(filename, "\"", "_")+"\"")
	http.ServeFile(w, r, path)
}

func (a *app) deleteImage(w http.ResponseWriter, r *http.Request) {
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
	_ = os.Remove(path)
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM images WHERE id=?`, id)
	if err != nil {
		writeError(w, 500, "delete image failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, 404, "image not found")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "deleted": true})
}

func (a *app) toggleImageFavorite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid image id")
		return
	}
	result, err := a.db.ExecContext(r.Context(), `UPDATE images SET is_favorite = 1 - is_favorite WHERE id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "toggle favorite failed")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	var fav int
	_ = a.db.QueryRowContext(r.Context(), `SELECT is_favorite FROM images WHERE id=?`, id).Scan(&fav)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "is_favorite": fav == 1})
}
