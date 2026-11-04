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
	"strings"
	"sync"
	"sync/atomic"
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

	// allThumbSpecs 是「一张图需要产出的全部档位」，删除/后台生成都按它遍历。
	allThumbSpecs = []thumbSpec{thumbSpecThumb, thumbSpecPreview}
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

// writeThumbAtomic 把编码结果原子落盘：先写临时文件再 rename，
// 保证并发读到的是完整文件而不是写了一半的。
//
// 临时文件名必须唯一（CreateTemp 带随机后缀）：后台预热走 buildThumbSet、
// 前台兜底走 ensureThumbFile，两条路径可能同时写同一个 dst，固定名 ".tmp"
// 会互相踩坏。并发重复编码同一张图只是浪费一点 CPU，结果仍然正确。
func writeThumbAtomic(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// buildThumbFile 读原图 → 缩放 → 编码 → 原子落盘（单档）。
// 前台 HTTP 兜底与 singleflight 走这条路径：只缺哪档就只做哪档。
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
	return writeThumbAtomic(dst, data)
}

// missingSpecs 返回其中还没生成的档位（文件不存在或大小为 0 都算缺）。
func (a *app) missingSpecs(id int64, specs []thumbSpec) []thumbSpec {
	out := make([]thumbSpec, 0, len(specs))
	for _, spec := range specs {
		if fi, err := os.Stat(a.thumbFile(id, spec)); err == nil && fi.Size() > 0 {
			continue
		}
		out = append(out, spec)
	}
	return out
}

// buildThumbSet 读原图**一次**、解码**一次**，按需产出多档缩略图，返回新建的文件数。
//
// 这是后台路径（预热 / 出图入队）的批量入口。老实现里 thumb 和 preview
// 各自 open+decode 一遍，而 PNG 解码在弱 CPU 上占单张耗时的三成以上，
// 等于每张图白烧一次解码。两档共用同一个解码结果，缩放并行不了但解码省了一半。
func (a *app) buildThumbSet(id int64, srcPath string, specs []thumbSpec) (int, error) {
	if a.thumbs == nil {
		return 0, fmt.Errorf("thumbnail maker not initialized")
	}
	pending := a.missingSpecs(id, specs)
	if len(pending) == 0 {
		return 0, nil
	}

	a.thumbs.sem <- struct{}{}
	defer func() { <-a.thumbs.sem }()

	// 排队等名额期间别人可能已经生成好了（前台兜底请求、另一个 worker），
	// 复核一次，免得为一张已经齐全的图白解码一遍。
	if pending = a.missingSpecs(id, pending); len(pending) == 0 {
		return 0, nil
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return 0, err
	}

	// 同一张图的各档互不依赖：某一档编码失败不该连累另一档，但要汇总上报。
	built := 0
	var firstErr error
	for _, spec := range pending {
		data, err := encodeThumb(img, spec)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", spec.name, err)
			}
			continue
		}
		if err := writeThumbAtomic(a.thumbFile(id, spec), data); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", spec.name, err)
			}
			continue
		}
		built++
	}
	return built, firstErr
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
	for _, spec := range allThumbSpecs {
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
func (a *app) initThumbs() {
	workers := thumbWorkers()
	a.thumbs = &thumbMaker{
		// 1024 而不是 256：批量导入上千张时不必丢任务。丢掉的虽然能在
		// 用户翻到时补，但那等于把成本推给了一次 HTTP 请求。
		queue: make(chan thumbJob, 1024),
		sem:   make(chan struct{}, workers),
		calls: map[string]*thumbCall{},
	}
	for i := 0; i < workers; i++ {
		go a.thumbWorker()
	}
	a.log.Printf("thumbnail worker: %d (NumCPU=%d, THUMB_WORKERS=%q)",
		workers, runtime.NumCPU(), os.Getenv("THUMB_WORKERS"))
}

// thumbWorkers 决定编码并发上限。
//
// 纯 Go 的 WebP 编码是 CPU 密集型，且 encode() 每次都新建独立的 wasm 模块
// 实例（无全局锁），所以并发能真的吃满多核。老版本上限 2 是拍脑袋定的保守
// 值，实测 4 核 NAS 上整机 CPU 只到 26%——等于只有一个核在干活。
//
// 内存：单路峰值 = 原图 RGBA + 目标 RGBA + wasm 线性内存，4K 图约 40~60MB，
// 默认 4 路约 200MB。内存吃紧的机器用 THUMB_WORKERS=2 降回来。
func thumbWorkers() int {
	if v := strings.TrimSpace(os.Getenv("THUMB_WORKERS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return min(n, 8)
		}
	}
	return min(runtime.NumCPU(), 4)
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
		// 两档一次解码；已生成的那档在 buildThumbSet 内部跳过。
		if _, err := a.buildThumbSet(job.id, job.src, allThumbSpecs); err != nil {
			a.log.Printf("build thumbnails for image %d failed: %v", job.id, err)
		}
	}
}

// prewarmThumbnails 给存量图片补缩略图。启动时后台跑。
//
// 老实现是单线程 for 循环：initThumbs 起的 worker 只在消费 queue，而预热
// 走的是自己那条同步路径，于是并发永远是 1——一千多张图要爬一个多小时，
// 而机器上还有三个核闲着。现在改成 N 个 goroutine 用原子游标自己取活，
// 并发度与编码 sem 一致。
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

	if len(jobs) == 0 {
		return
	}

	// 并发数与编码 sem 对齐：预热和 queue worker 共享同一个 sem，
	// 所以两边同时跑也不会超过上限，这里开同样多就够。
	workers := cap(a.thumbs.sem)
	if workers < 1 {
		workers = 1
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	start := time.Now()
	var (
		cursor  atomic.Int64 // 已领取的任务游标，从 -1 开始自增
		scanned atomic.Int64
		built   atomic.Int64
		failed  atomic.Int64
	)
	cursor.Store(-1)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(cursor.Add(1))
				if i >= len(jobs) {
					return
				}
				j := jobs[i]
				n, err := a.buildThumbSet(j.id, j.src, allThumbSpecs)
				built.Add(int64(n))
				if err != nil {
					failed.Add(1)
					a.log.Printf("prewarm thumbnails: image %d failed: %v", j.id, err)
				}
				if s := scanned.Add(1); s%100 == 0 {
					a.log.Printf("prewarm thumbnails: %d/%d scanned, %d files built, %d failed",
						s, len(jobs), built.Load(), failed.Load())
				}
			}
		}()
	}
	wg.Wait()

	a.log.Printf("prewarm thumbnails: done, %d images, %d files built, %d failed, %s",
		len(jobs), built.Load(), failed.Load(), time.Since(start).Round(time.Second))
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
