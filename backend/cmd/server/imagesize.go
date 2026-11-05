package main

import (
	"bytes"
	"context"
	"image"
	// 注册解码器：image.DecodeConfig 只读文件头，靠这些 init 注册格式。
	// ComfyUI 出图是 png，导入的旧图可能是 jpg，
	// 外部平台同步（external.go）可能收到 webp——不注册 webp 会让它宽高恒为 0。
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"

	_ "golang.org/x/image/webp"
)

// 图片像素宽高是「大图预览按比例展示」的硬前提：PhotoSwipe 只信 data.width/height，
// 从不读 naturalWidth。以前前端为每张图 new Image() 现量，跨页预取时几十上百张
// 并发排队、超过 10s 兜底成 1024×1024 方图，竖版长图就被压扁。
// 现在改为落库时记一次，前端零请求拿到真比例。

// imageSizeFromBytes 从内存里的图片数据解出宽高。
// image.DecodeConfig 只读文件头，不做像素解码，几毫秒级。
func imageSizeFromBytes(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// imageSizeFromFile 同上，读磁盘上的图片文件头。
// 失败返回 0,0 —— 由 backfillImageSizes 下次启动再补，不阻断写入流程。
func imageSizeFromFile(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// backfillImageSizes 给历史数据补宽高（新增列后存量行全是 0）。
// 启动时后台跑：只读文件头，逐批提交，不长时间占 WAL 写锁。
func (a *app) backfillImageSizes() {
	ctx := context.Background()
	rows, err := a.db.QueryContext(ctx, `SELECT id, storage_path FROM images WHERE width=0 OR height=0`)
	if err != nil {
		a.log.Printf("backfill image sizes: query failed: %v", err)
		return
	}
	type pending struct {
		id     int64
		width  int
		height int
	}
	batch := make([]pending, 0, backfillBatchSize)
	total, filled := 0, 0

	flush := func() {
		if len(batch) == 0 {
			return
		}
		tx, txErr := a.db.BeginTx(ctx, nil)
		if txErr != nil {
			a.log.Printf("backfill image sizes: begin tx failed: %v", txErr)
			batch = batch[:0]
			return
		}
		for _, p := range batch {
			if _, execErr := tx.ExecContext(ctx, `UPDATE images SET width=?, height=? WHERE id=?`, p.width, p.height, p.id); execErr != nil {
				a.log.Printf("backfill image sizes: update %d failed: %v", p.id, execErr)
			} else {
				filled++
			}
		}
		if commitErr := tx.Commit(); commitErr != nil {
			a.log.Printf("backfill image sizes: commit failed: %v", commitErr)
		}
		batch = batch[:0]
	}

	for rows.Next() {
		var id int64
		var path string
		if scanErr := rows.Scan(&id, &path); scanErr != nil {
			continue
		}
		total++
		width, height := 0, 0
		if target := a.imageFilePath(path); target != "" {
			width, height = imageSizeFromFile(target)
		}
		// 量不到（文件丢失/损坏）也写回 0，避免每次启动重复扫；前端会退回单张实测
		batch = append(batch, pending{id: id, width: width, height: height})
		if len(batch) >= backfillBatchSize {
			flush()
		}
	}
	_ = rows.Close()
	flush()
	if total > 0 {
		a.log.Printf("backfill image sizes: %d scanned, %d updated", total, filled)
	}
}

// backfillBatchSize 单事务回填条数：SQLite 写锁别长占，也别一条一事务太碎。
const backfillBatchSize = 200
