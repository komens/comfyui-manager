package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	// 注册解码器：出图是 PNG，导入的旧图可能是 JPEG，少数情况有 GIF。
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/gen2brain/webp"
	xdraw "golang.org/x/image/draw"
)

// 缩略图只做两档，不做中间档：
//
//	thumb   —— 小图标（列表 / 卡片里的方形小图，最大显示 140px）
//	preview —— 大图预览，同时给图库网格卡片用
//
// 网格卡片与预览共用同一个 URL 是刻意的：点开大图时浏览器缓存直接命中，
// 不会出现「网格下一份、预览再下一份」的重复流量。
//
// 两档都是「上限」而非固定尺寸：原图比上限小就保持原尺寸，只做 PNG→WebP
// 的转码，绝不放大（放大会让体积变大、画质更糊，没有任何收益）。
const (
	// 小图标按短边算：网格/卡片是 object-fit:cover，短边决定清晰度。
	// 最大显示 140px（提示词详情），按 DPR3 是 420，480 留了点余量。
	thumbShortEdge = 480
	// 大图预览按长边算：整图等比缩到长边 1600，2K 屏刚好显示 1:1。
	previewMaxEdge = 1600
	// q82 在 AI 生图这类高细节素材上，肉眼与原图几乎无差；
	// 降到 78 体积再省约 15%，但渐变区和细线开始出现可察觉的块状。
	thumbQuality = 82
)

// thumbSpec 描述一档缩略图的生成规则。
type thumbSpec struct {
	name    string // 文件名标识
	edge    int    // 目标边长
	byShort bool   // true=短边对齐 edge（cover 场景）；false=长边对齐 edge
}

var (
	thumbSpecThumb   = thumbSpec{name: "thumb", edge: thumbShortEdge, byShort: true}
	thumbSpecPreview = thumbSpec{name: "preview", edge: previewMaxEdge, byShort: false}
)

// ─────────────────────────── 存储 ───────────────────────────

// thumbRoot 是缩略图根目录，刻意与 images/ 同级而不是塞进去：
// 迁移/备份时可以整个目录排除，原图目录也不会混进派生文件。
func (a *app) thumbRoot() string {
	return filepath.Join(a.dataDir, "thumbnails")
}

// thumbFile 返回某张图某档缩略图的落点。
//
// 文件名带上了边长与质量参数：以后调整 thumbShortEdge/thumbQuality 时，
// 旧文件自然不会再被命中，新参数会重新生成，不需要手写迁移。
// 按 id 后两位分桶，避免几万张图堆在一个目录里（ext4/NTFS 下 readdir 会变慢）。
func (a *app) thumbFile(id int64, spec thumbSpec) string {
	bucket := fmt.Sprintf("%02d", ((id%100)+100)%100)
	name := fmt.Sprintf("%d_%s_%d_q%d.webp", id, spec.name, spec.edge, thumbQuality)
	return filepath.Join(a.thumbRoot(), bucket, name)
}

// targetSize 按 spec 算出输出尺寸。base 取短边或长边，超过上限才缩放。
func targetSize(w, h int, spec thumbSpec) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	base := w
	if spec.byShort {
		base = min(w, h)
	} else {
		base = max(w, h)
	}
	if base <= spec.edge {
		return w, h
	}
	scale := float64(spec.edge) / float64(base)
	nw := int(math.Round(float64(w) * scale))
	nh := int(math.Round(float64(h) * scale))
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}

// ─────────────────────────── 编码 ───────────────────────────

// encodeThumb 把解码后的图片缩放并编码成 WebP。
// 缩放用 CatmullRom（比最近邻/双线性锐利得多），代价是几十毫秒，可接受。
func encodeThumb(img image.Image, spec thumbSpec) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := targetSize(w, h, spec)
	if nw <= 0 || nh <= 0 {
		return nil, fmt.Errorf("invalid image bounds %dx%d", w, h)
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	if nw == w && nh == h {
		xdraw.Draw(dst, dst.Bounds(), img, b.Min, xdraw.Src)
	} else {
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	}
	var buf bytes.Buffer
	// Method 4：libwebp 默认压缩级别，体积/耗时平衡点。
	if err := webp.Encode(&buf, dst, webp.Options{Quality: thumbQuality, Method: 4}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildThumbFile 读原图 → 缩放 → 编码 → 原子落盘。
// 写临时文件再 rename：并发请求同一张图时不会读到写了一半的文件。
func (a *app) buildThumbFile(id int64, spec thumbSpec, srcPath, dst string) error {
	if a.thumbs == nil {
		return fmt.Errorf("thumbnail maker not initialized")
	}
	a.thumbs.sem <- struct{}{}
	defer func() { <-a.thumbs.sem }()

	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	data, err := encodeThumb(img, spec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ensureThumbFile 保证某档缩略图存在，返回可读路径。
// 同一张图同一档被并发请求时只编码一次（singleflight），其余等结果。
func (a *app) ensureThumbFile(id int64, spec thumbSpec, srcPath string) (string, error) {
	dst := a.thumbFile(id, spec)
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		return dst, nil
	}
	if a.thumbs == nil {
		return "", fmt.Errorf("thumbnail maker not initialized")
	}

	a.thumbs.mu.Lock()
	if call, ok := a.thumbs.calls[dst]; ok {
		a.thumbs.mu.Unlock()
		<-call.done
		return dst, call.err
	}
	call := &thumbCall{done: make(chan struct{})}
	a.thumbs.calls[dst] = call
	a.thumbs.mu.Unlock()

	call.err = a.buildThumbFile(id, spec, srcPath, dst)
	close(call.done)

	a.thumbs.mu.Lock()
	delete(a.thumbs.calls, dst)
	a.thumbs.mu.Unlock()
	return dst, call.err
}

// removeThumbs 删除某张图的全部派生文件。原图被删/被彻底清理时必须同步调用，
// 否则缩略图会变成永远没人引用的孤儿。
func (a *app) removeThumbs(id int64) {
	for _, spec := range []thumbSpec{thumbSpecThumb, thumbSpecPreview} {
		_ = os.Remove(a.thumbFile(id, spec))
	}
}

// ─────────────────────────── 后台生成队列 ───────────────────────────

type thumbJob struct {
	id  int64
	src string
}

type thumbCall struct {
	done chan struct{}
	err  error
}

type thumbMaker struct {
	queue chan thumbJob
	sem   chan struct{} // 编码并发上限：NAS 的 CPU 很弱，别把核打满
	mu    sync.Mutex
	calls map[string]*thumbCall
}

// initThumbs 初始化生成器并起 worker。
// 并发取 CPU 的一半、最多 2：单张 preview 编码 0.4–1.1s，2 路已经能把
// 第一批历史图在几分钟内补完，又不至于让接口请求排队等 CPU。
func (a *app) initThumbs() {
	workers := runtime.NumCPU() / 2
	if workers < 1 {
		workers = 1
	}
	if workers > 2 {
		workers = 2
	}
	a.thumbs = &thumbMaker{
		queue: make(chan thumbJob, 256),
		sem:   make(chan struct{}, workers),
		calls: map[string]*thumbCall{},
	}
	for i := 0; i < workers; i++ {
		go a.thumbWorker()
	}
}

// queueThumbBuild 把新图丢进后台队列，不阻塞出图/导入流程。
// 队列满了就丢弃 —— 缩略图是派生数据，缺了下次请求会自动补。
func (a *app) queueThumbBuild(id int64, src string) {
	if a.thumbs == nil || src == "" {
		return
	}
	select {
	case a.thumbs.queue <- thumbJob{id: id, src: src}:
	default:
	}
}

func (a *app) thumbWorker() {
	for job := range a.thumbs.queue {
		for _, spec := range []thumbSpec{thumbSpecThumb, thumbSpecPreview} {
			if _, err := a.ensureThumbFile(job.id, spec, job.src); err != nil {
				a.log.Printf("build %s for image %d failed: %v", spec.name, job.id, err)
			}
		}
	}
}

// prewarmThumbnails 给存量图片补缩略图。启动时后台跑，慢一点没关系——
// 前端只有在真的翻到某张历史图、且它还没生成时才会等那一次编码。
func (a *app) prewarmThumbnails() {
	if a.thumbs == nil {
		return
	}
	ctx := context.Background()
	rows, err := a.db.QueryContext(ctx, `SELECT id, storage_path FROM images ORDER BY id DESC`)
	if err != nil {
		a.log.Printf("prewarm thumbnails: query failed: %v", err)
		return
	}
	type job struct {
		id  int64
		src string
	}
	jobs := make([]job, 0, 256)
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			continue
		}
		if src := a.imageFilePath(path); src != "" {
			jobs = append(jobs, job{id: id, src: src})
		}
	}
	_ = rows.Close()

	built, failed := 0, 0
	start := time.Now()
	for i, j := range jobs {
		// 已存在就跳过，避免每次重启都把整个库重编码一遍
		allExist := true
		for _, spec := range []thumbSpec{thumbSpecThumb, thumbSpecPreview} {
			if fi, err := os.Stat(a.thumbFile(j.id, spec)); err != nil || fi.Size() == 0 {
				allExist = false
				break
			}
		}
		if allExist {
			continue
		}
		for _, spec := range []thumbSpec{thumbSpecThumb, thumbSpecPreview} {
			if _, err := a.ensureThumbFile(j.id, spec, j.src); err != nil {
				failed++
				continue
			}
			built++
		}
		if (i+1)%50 == 0 {
			a.log.Printf("prewarm thumbnails: %d/%d scanned, %d built, %d failed", i+1, len(jobs), built, failed)
		}
	}
	if len(jobs) > 0 {
		a.log.Printf("prewarm thumbnails: done, %d images, %d files built, %d failed, %s",
			len(jobs), built, failed, time.Since(start).Round(time.Second))
	}
}

// ─────────────────────────── HTTP ───────────────────────────

// imageThumbHandler 输出某一档缩略图。
//
// 生成失败（原图损坏、格式不支持、磁盘满）时**退回原图**而不是报错：
// 缩略图只是省流量的优化，不该让一张本来能看的图变成 404。
func (a *app) imageThumbHandler(spec thumbSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		// 与 /file、删除走同一套路径解析，避免「能看原图、缩略图 404」
		src := a.imageFilePath(path)
		if src == "" {
			writeError(w, 404, "image file not found")
			return
		}
		dst, err := a.ensureThumbFile(id, spec, src)
		if err != nil {
			a.log.Printf("thumbnail %s for image %d failed, serving original: %v", spec.name, id, err)
			http.ServeFile(w, r, src)
			return
		}
		f, err := os.Open(dst)
		if err != nil {
			http.ServeFile(w, r, src)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			http.ServeFile(w, r, src)
			return
		}

		// 图片 id 唯一且内容不可变（同一 id 不会被替换），所以可以放心 immutable。
		// 参数变更会改变文件名 → URL，天然绕开旧缓存。
		etag := fmt.Sprintf(`"im-%d-%s-%d-q%d"`, id, spec.name, spec.edge, thumbQuality)
		w.Header().Set("Content-Type", "image/webp")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		http.ServeContent(w, r, "", info.ModTime(), f)
	}
}

// ─────────────────────────── 维护统计 ───────────────────────────

// scanThumbDir 统计缩略图目录：文件数、总字节、以及 id 已不在库里的孤儿。
func (a *app) scanThumbDir(ctx context.Context) (files int, bytes int64, orphanIDs []int64) {
	root := a.thumbRoot()
	buckets, err := os.ReadDir(root)
	if err != nil {
		return 0, 0, nil
	}
	known := map[int64]bool{}
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM images`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				known[id] = true
			}
		}
	}
	seen := map[int64]bool{}
	for _, bucket := range buckets {
		if !bucket.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, bucket.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			files++
			if info, err := e.Info(); err == nil {
				bytes += info.Size()
			}
			if id := thumbIDFromName(e.Name()); id > 0 && !known[id] && !seen[id] {
				seen[id] = true
				orphanIDs = append(orphanIDs, id)
			}
		}
	}
	if len(orphanIDs) > 200 {
		orphanIDs = orphanIDs[:200]
	}
	return files, bytes, orphanIDs
}

// thumbIDFromName 从 "<id>_<spec>_<edge>_q<q>.webp" 里取出 id。
func thumbIDFromName(name string) int64 {
	for i := 0; i < len(name); i++ {
		if name[i] == '_' {
			id, err := strconv.ParseInt(name[:i], 10, 64)
			if err != nil {
				return 0
			}
			return id
		}
	}
	return 0
}
