package main

import "net/http"

func (a *app) listImages(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, generation_item_id, filename, storage_path, created_at FROM images ORDER BY id DESC LIMIT 200`)
	if err != nil { writeError(w, 500, "query images failed"); return }
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() { var id, itemID int; var filename, path, created string; if err := rows.Scan(&id, &itemID, &filename, &path, &created); err != nil { writeError(w, 500, "read image failed"); return }; items = append(items, map[string]any{"id": id, "generation_item_id": itemID, "filename": filename, "path": path, "created_at": created}) }
	writeJSON(w, http.StatusOK, items)
}
