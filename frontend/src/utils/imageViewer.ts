// 统一图片查看器：基于 PhotoSwipe 5（双指缩放/滑动切换/键盘导航），
// 底部挂自定义 caption：移动端默认摘要一行、点击展开，支持复制提示词、跳转来源、收藏/下载/删除。
import PhotoSwipeLightbox from 'photoswipe/lightbox'
import PhotoSwipe from 'photoswipe'
import 'photoswipe/style.css'
import { api } from '../api/client'
import { previewUrl, originalUrl, previewSize } from './imageUrls'

export type ViewerDetail = {
  id: number
  filename: string
  created_at: string
  is_favorite: boolean
  prompt_id: number
  prompt_title: string
  positive_prompt: string
  negative_prompt: string
  /** 原图像素宽高，用于「查看原图」时换算出正确的 1:1 尺寸 */
  width?: number
  height?: number
}

export type ViewerCallbacks = {
  goToPrompt?: (promptId: number) => void
  /** 查看期间发生收藏/删除后回调，用于父列表刷新 */
  mutated?: () => void
  /**
   * 关闭后回调。index 是关闭时停留的那张在「整个筛选结果」里的下标（0-based、跨页），
   * 列表页据此把自己切到对应页码，而不是退回打开时那一页。
   * 传了 onClose 就不用再传 mutated：第二个参数就是「本次有没有收藏/删除」。
   */
  onClose?: (index: number, mutated: boolean) => void
}

// src 用 preview（长边 1600 的 WebP），不再直接拉原图：
// 一张原图 1.5–4.4MB，preview 只有 74–144KB，画质在 2K 屏上看不出差别。
// 需要 1:1 细节时由底部的「查看原图」按钮按需切换。
type SlideItem = { width: number; height: number; src: string; alt: string; psId: number; isOriginal?: boolean; deleted?: boolean }

/**
 * 调用方可以只给 id，也可以带上后端返回的像素宽高。
 * 宽高**必须**尽量带上：PhotoSwipe 只认 data.width/height，从不读 naturalWidth，
 * 拿不到真尺寸的图会被当正方形拉伸（竖版长图被压扁）。
 */
export type ViewerRef = number | { id: number; width?: number; height?: number }

type Ref = { id: number; width: number; height: number }

/** 尺寸未知时的临时占位（方图）——只作为兜底显示，量到真尺寸后立即纠正 */
const PLACEHOLDER_SIZE = 1024
const MEASURE_CONCURRENCY = 3
const MEASURE_TIMEOUT = 8000

/**
 * 删除图片后当前这张的占位图。
 * 删完不关闭灯箱：把这一格就地换成占位，然后滑到相邻一张继续看，
 * 序列和索引都不用动（PhotoSwipe 没有公开的 removeSlide，改索引极易错位）。
 */
const DELETED_SRC = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(
  `<svg xmlns="http://www.w3.org/2000/svg" width="800" height="600">
    <rect width="800" height="600" fill="#18181b"/>
    <text x="400" y="300" font-family="-apple-system,system-ui,sans-serif" font-size="26"
      fill="#71717a" text-anchor="middle" dominant-baseline="middle">图片已删除</text>
  </svg>`,
)}`

function toRefs(input: ViewerRef[]): Ref[] {
  return input.map(r => {
    if (typeof r === 'number') return { id: r, width: 0, height: 0 }
    const width = Math.round(Number(r.width) || 0)
    const height = Math.round(Number(r.height) || 0)
    return { id: r.id, width: width > 0 ? width : 0, height: height > 0 ? height : 0 }
  })
}

/**
 * 兜底实测一张图的像素尺寸。
 * 正常路径下后端已随列表返回宽高（images.go/imagesize.go），这里根本不会触发；
 * 只有历史数据（列刚加上还没回填完）或文件缺失才走这条路。
 */
function measureImage(url: string): Promise<{ width: number; height: number } | null> {
  return new Promise(resolve => {
    const img = new Image()
    const timer = setTimeout(() => resolve(null), MEASURE_TIMEOUT)
    img.onload = () => {
      clearTimeout(timer)
      const w = img.naturalWidth
      const h = img.naturalHeight
      resolve(w > 0 && h > 0 ? { width: w, height: h } : null)
    }
    img.onerror = () => {
      clearTimeout(timer)
      resolve(null)
    }
    img.src = url
  })
}

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      const ok = document.execCommand('copy')
      document.body.removeChild(ta)
      return ok
    } catch {
      return false
    }
  }
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c] as string))
}

function formatDate(s: string): string {
  if (!s) return ''
  const d = new Date(s)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString('zh-CN')
}

let activeIds: number[] = []

/** 供外部删除后同步当前查看序列 */
export function viewerRemoveId(id: number) {
  const idx = activeIds.indexOf(id)
  if (idx >= 0) activeIds.splice(idx, 1)
}

export type PrefetchOptions = {
  /** 当前这组 ids 所属的页码（1-based） */
  page: number
  pageSize: number
  /** 整个筛选结果集的总数，用于判断是否还有下一页 */
  total: number
  filters?: { favorite?: boolean | string; q?: string; group?: string }
}

export async function openImageViewer(input: ViewerRef[], startIndex: number, cb: ViewerCallbacks = {}, prefetch?: PrefetchOptions) {
  if (!input.length || activeIds.length) return

  let loadedRefs = toRefs(input)
  let lastPage = 1
  let totalCount = input.length

  // —— 分页预取 ——
  // 打开大图时，先把「当前页之前」的各页补齐进列表（按页请求 /api/images，
  // 不是一次性全量拉取），这样往回切能一路走完整个筛选结果集；
  // 往后切则在接近末尾时由 maybePrefetch 按需拉下一页 push 进列表。
  if (prefetch) {
    lastPage = prefetch.page
    totalCount = prefetch.total
    if (prefetch.page > 1) {
      try {
        const pages = await Promise.all(
          Array.from({ length: prefetch.page - 1 }, (_, k) => fetchPageRefs(k + 1, prefetch)),
        )
        const before = pages.flat()
        loadedRefs = [...before, ...loadedRefs]
        startIndex = (prefetch.page - 1) * prefetch.pageSize + startIndex
      } catch {
        // 预取失败不阻断：退化为仅当前页
        loadedRefs = toRefs(input)
      }
    }
  }

  activeIds = loadedRefs.map(r => r.id)

  // 宽高全部来自接口/调用方，不再为量尺寸去并发拉原图——
  // 之前正是「上百张并发 + 10s 超时兜底成 1024×1024」把竖版长图压成方图的。
  const items: SlideItem[] = loadedRefs.map(toSlideItem)
  /** 尺寸未知（历史数据）的索引：先用方图占位，量到真尺寸再纠正 */
  const unknownIdx = new Set<number>()
  loadedRefs.forEach((r, i) => {
    if (r.width <= 0 || r.height <= 0) unknownIdx.add(i)
  })

  function toSlideItem(r: Ref): SlideItem {
    const known = r.width > 0 && r.height > 0
    // 尺寸要给「preview 这张图」的实际像素，不是原图：
    // PhotoSwipe 用它算 fit 与双击放大上限，给原图尺寸会让它以为还能放到更大，越放越糊。
    const size = known ? previewSize(r.width, r.height) : { width: PLACEHOLDER_SIZE, height: PLACEHOLDER_SIZE }
    return {
      width: size.width,
      height: size.height,
      src: previewUrl(r.id),
      alt: `图片 #${r.id}`,
      psId: r.id,
    }
  }

  const loadingPages = new Set<string>()
  async function fetchPageRefs(page: number, opts: PrefetchOptions): Promise<Ref[]> {
    const params = new URLSearchParams()
    const f = opts.filters || {}
    if (f.favorite) params.set('favorite', '1')
    if (f.q) params.set('q', f.q)
    if (f.group) params.set('group', f.group)
    params.set('page', String(page))
    params.set('page_size', String(opts.pageSize))
    const result = await api<{ items: { id: number; width?: number; height?: number }[]; total: number }>(`/api/images?${params}`)
    totalCount = result.total
    return toRefs(result.items)
  }
  async function maybePrefetch(cur: number) {
    const opts = prefetch
    if (!opts) return
    if (cur < items.length - 2) return
    if (loadingPages.has('next')) return
    if (lastPage * opts.pageSize >= totalCount) return
    loadingPages.add('next')
    try {
      const pageRefs = await fetchPageRefs(lastPage + 1, opts)
      if (!pageRefs.length) return
      pageRefs.forEach(ref => {
        const index = items.length
        items.push(toSlideItem(ref))
        loadedRefs.push(ref)
        if (ref.width <= 0 || ref.height <= 0) unknownIdx.add(index)
      })
      activeIds = loadedRefs.map(r => r.id)
      lastPage++
    } catch {
      /* 预取失败不阻断浏览 */
    } finally {
      loadingPages.delete('next')
    }
  }

  const detailCache = new Map<number, ViewerDetail>()
  let mutated = false
  let expanded = typeof window !== 'undefined' && window.innerWidth > 768

  const lightbox = new PhotoSwipeLightbox({
    pswpModule: PhotoSwipe,
    loop: !prefetch,
    showHideAnimationType: 'zoom',
    bgOpacity: 0.92,
  })

  // —— 尺寸兜底（只有历史数据才走）——
  // 并发限 3，且只对「要看的这张」按需触发；量到真尺寸后改 item 并重建该幻灯片，
  // 不会像以前那样把一次误判的方图比例永久固化。
  const measureQueue: number[] = []
  const measurePending = new Set<number>()
  let measureBusy = 0

  function pumpMeasure() {
    while (measureBusy < MEASURE_CONCURRENCY && measureQueue.length) {
      const index = measureQueue.shift() as number
      if (!unknownIdx.has(index)) {
        measurePending.delete(index)
        continue
      }
      measureBusy++
      measureImage(items[index].src).then(size => {
        measureBusy--
        measurePending.delete(index)
        if (size) {
          items[index].width = size.width
          items[index].height = size.height
          unknownIdx.delete(index)
          // 幻灯片可能已经按方图建好了，必须重建才会用上新比例
          lightbox.pswp?.refreshSlideContent(index)
        } else {
          // 量不到（文件缺失/损坏）就保持占位，本次会话不再重试
          unknownIdx.delete(index)
        }
        pumpMeasure()
      })
    }
  }

  function requestMeasure(index: number, urgent = false) {
    if (!unknownIdx.has(index) || measurePending.has(index)) return
    measurePending.add(index)
    if (urgent) measureQueue.unshift(index)
    else measureQueue.push(index)
    pumpMeasure()
  }

  let captionEl: HTMLElement | null = null
  let fabEl: HTMLElement | null = null
  let currentId = 0
  /** 关闭时停留的那张的全局下标（跨页），用于让列表页跟着切页 */
  let lastIndex = startIndex
  let renderSeq = 0

  function renderCaption(detail: ViewerDetail | null, loading: boolean) {
    if (!captionEl) return
    if (loading || !detail) {
      captionEl.innerHTML = `<div class="ps-caps-summary"><span class="ps-caps-spinner"></span>加载中…</div>`
      return
    }
    const pos = detail.positive_prompt ? `
      <div class="ps-caps-section">
        <div class="ps-caps-label"><span class="label-dot positive"></span>正向提示词
          <button class="ps-caps-copy" data-action="copy-positive" type="button">复制</button>
        </div>
        <div class="ps-caps-text">${escapeHtml(detail.positive_prompt)}</div>
      </div>` : ''
    const neg = detail.negative_prompt ? `
      <div class="ps-caps-section">
        <div class="ps-caps-label"><span class="label-dot negative"></span>负向提示词
          <button class="ps-caps-copy" data-action="copy-negative" type="button">复制</button>
        </div>
        <div class="ps-caps-text">${escapeHtml(detail.negative_prompt)}</div>
      </div>` : ''
    const source = detail.prompt_id ? `
      <div class="ps-caps-section">
        <div class="ps-caps-label"><span class="label-dot source"></span>来源提示词</div>
        <div class="ps-caps-text">${escapeHtml(detail.prompt_title || '未命名提示词')}</div>
        <button class="ps-caps-btn" data-action="go-prompt" data-prompt="${detail.prompt_id}" type="button">查看提示词 #${detail.prompt_id} →</button>
      </div>` : `
      <div class="ps-caps-section">
        <div class="ps-caps-label"><span class="label-dot source"></span>来源提示词</div>
        <div class="ps-caps-text muted">手动提交（未关联提示词）</div>
      </div>`
    captionEl.innerHTML = `
      <div class="ps-caps-summary" data-action="toggle">
        <span>#${detail.id}</span>
        <span class="ps-caps-dot">·</span>
        <span>${escapeHtml(detail.prompt_id ? (detail.prompt_title || '未命名提示词') : '手动提交')}</span>
        <span class="ps-caps-caret">${expanded ? '▴ 收起' : '▾ 展开详情'}</span>
      </div>
      <div class="ps-caps-body" ${expanded ? '' : 'hidden'}>
        <div class="ps-caps-meta">${escapeHtml(formatDate(detail.created_at))}${detail.filename ? ` · ${escapeHtml(detail.filename)}` : ''}</div>
        ${source}${pos}${neg}
        <div class="ps-caps-actions">
          <button class="ps-caps-btn primary" data-action="download" type="button">↓ 下载原图</button>
        </div>
      </div>`
  }

  async function showCaptionFor(id: number) {
    currentId = id
    const seq = ++renderSeq
    const cached = detailCache.get(id)
    if (!cached) renderCaption(null, true)
    try {
      const detail = cached || (await api<ViewerDetail>(`/api/images/${id}`))
      detailCache.set(id, detail)
      if (seq === renderSeq && id === currentId) { renderCaption(detail, false); renderFab(detail) }
    } catch {
      if (seq === renderSeq && id === currentId) {
        renderCaption({ id, filename: '', created_at: '', is_favorite: false, prompt_id: 0, prompt_title: '', positive_prompt: '', negative_prompt: '' }, false)
      }
    }
  }

  function flashCopied(btn: HTMLElement) {
    btn.textContent = '已复制'
    setTimeout(() => { btn.textContent = '复制' }, 2000)
  }

  /** 浮动按钮反馈：临时改说明文字 + 红框，2 秒后恢复。静默 catch 会让用户以为按钮坏了 */
  function flashFab(action: string, text: string, restore: string) {
    const btn = fabEl?.querySelector<HTMLElement>(`[data-action="${action}"]`)
    if (!btn) return
    setFabTip(btn, text)
    btn.classList.add('is-failed')
    setTimeout(() => {
      if (!btn.isConnected) return
      btn.classList.remove('is-failed')
      setFabTip(btn, restore)
    }, 2000)
  }

  /** 右下角浮动按钮列：根据当前图收藏状态刷新星标（实心/空心由 .active 控制） */
  function renderFab(detail: ViewerDetail | null) {
    if (!fabEl) return
    const favBtn = fabEl.querySelector<HTMLButtonElement>('[data-action="fab-favorite"]')
    if (!favBtn) return
    const fav = !!detail?.is_favorite
    favBtn.classList.toggle('active', fav)
    favBtn.setAttribute('aria-pressed', fav ? 'true' : 'false')
    setFabTip(favBtn, fav ? '取消收藏' : '收藏')
  }

  /** 已删除占位图的说明：不请求详情，也没什么可展开的 */
  function renderDeletedCaption(id: number) {
    if (!captionEl) return
    // 不能用 .ps-caps-summary：它在桌面端是 display:none（详情默认展开），会一行字都看不到
    captionEl.innerHTML = `<div class="ps-caps-deleted">#${id} · 这张图片已删除</div>`
  }

  /** 占位图上禁用原图/收藏/删除（已经没东西可操作了） */
  function setFabDisabled(disabled: boolean) {
    if (!fabEl) return
    fabEl.querySelectorAll<HTMLButtonElement>('button').forEach(b => { b.disabled = disabled })
  }

  /**
   * 当前幻灯片的 data。
   * PhotoSwipe 5 叫 `pswp.currSlide.data`——**没有** `currData`（那是 v4 的 API）。
   * 之前 change 回调读 `currData` 拿到恒为 undefined，导致 currentId 永远是 0，
   * 收藏/删除/查看原图全部静默失效。切勿再改名。
   * 首帧 currSlide 可能尚未挂上，用 currIndex 从 items 兜底。
   */
  function currentSlideData(): SlideItem | undefined {
    const pswp = lightbox.pswp as unknown as
      | { currSlide?: { data?: SlideItem }; currIndex?: number }
      | undefined
    if (!pswp) return undefined
    const data = pswp.currSlide?.data
    if (data) return data
    const index = pswp.currIndex
    return typeof index === 'number' ? items[index] : undefined
  }

  /** 浮动按钮的说明文字（悬停向左浮出，也作为读屏标签），不占画面 */
  function setFabTip(btn: HTMLElement, tip: string) {
    btn.dataset.tip = tip
    btn.setAttribute('aria-label', tip)
  }

  /** 同步原图按钮状态：加载的是原图时 HD 徽标高亮，说明改成「看压缩图」 */
  function syncOriginalButtons() {
    const isOriginal = !!currentSlideData()?.isOriginal
    const fabBtn = fabEl?.querySelector<HTMLButtonElement>('[data-action="fab-original"]')
    if (!fabBtn) return
    fabBtn.classList.toggle('active', isOriginal)
    fabBtn.classList.remove('is-failed')
    setFabTip(fabBtn, isOriginal ? '看压缩图' : '查看原图')
  }

  /**
   * 在原图与压缩版之间切换当前这一张。
   * 只对「正在看的这张」按需加载原图，不做任何预取——一张 1.5–4.4MB，
   * 预取等于把刚省下来的流量原样还回去。
   */
  async function toggleOriginal() {
    const pswp = lightbox.pswp
    const data = currentSlideData()
    if (!pswp || !data || data.deleted) return
    const fabBtn = fabEl?.querySelector<HTMLButtonElement>('[data-action="fab-original"]')
    if (fabBtn?.disabled) return

    if (data.isOriginal) {
      // 切回压缩版：preview 早就在浏览器缓存里，瞬时完成
      const detail = detailCache.get(data.psId)
      const size = detail?.width && detail?.height
        ? previewSize(detail.width, detail.height)
        : { width: data.width, height: data.height }
      data.src = previewUrl(data.psId)
      data.width = size.width
      data.height = size.height
      data.isOriginal = false
      pswp.refreshSlideContent(pswp.currIndex)
      pswp.updateSize(true)
      syncOriginalButtons()
      return
    }

    if (fabBtn) {
      fabBtn.disabled = true
      setFabTip(fabBtn, '加载原图…')
    }
    let ok = false
    try {
      const src = originalUrl(data.psId)
      // 先等加载完再换 src，否则 PhotoSwipe 会先闪一个空白图框
      const size = await measureImage(src)
      if (!size) return
      data.src = src
      // 尺寸必须一起换成原图真实像素，否则双击放大到 1:1 的上限还是压缩图的
      data.width = size.width
      data.height = size.height
      data.isOriginal = true
      pswp.refreshSlideContent(pswp.currIndex)
      pswp.updateSize(true)
      ok = true
    } finally {
      if (fabBtn) fabBtn.disabled = false
      syncOriginalButtons()
      // 超时/读失败时给个明确反馈，否则点了像没反应
      if (!ok && !data.isOriginal && fabBtn) {
        fabBtn.classList.add('is-failed')
        setFabTip(fabBtn, '原图加载失败')
        setTimeout(() => { if (fabBtn.isConnected) syncOriginalButtons() }, 2000)
      }
    }
  }

  async function doFavorite() {
    const id = currentId
    if (!id) return
    // 占位图上没有可收藏的对象（按钮通常已禁用，这里兜一层）
    if (currentSlideData()?.deleted) return
    let detail = detailCache.get(id)
    if (!detail) {
      try {
        detail = await api<ViewerDetail>(`/api/images/${id}`)
        detailCache.set(id, detail)
      } catch {
        flashFab('fab-favorite', '加载失败', '收藏')
        return
      }
    }
    try {
      const r = await api<{ is_favorite: boolean }>(`/api/images/${id}/favorite`, { method: 'PATCH' })
      detail.is_favorite = r.is_favorite
      mutated = true
      // 请求期间可能已划到下一张：别把这张的状态画到那张的 UI 上
      if (id !== currentId) return
      renderCaption(detail, false)
      renderFab(detail)
    } catch {
      flashFab('fab-favorite', '操作失败', detail.is_favorite ? '取消收藏' : '收藏')
    }
  }

  /** 把当前这张就地换成「已删除」占位：不关闭灯箱，序列与索引都保持原样 */
  function markDeleted(data: SlideItem) {
    data.src = DELETED_SRC
    data.width = 800
    data.height = 600
    data.deleted = true
    data.isOriginal = false
    detailCache.delete(data.psId)
    const pswp = lightbox.pswp
    if (pswp) {
      pswp.refreshSlideContent(pswp.currIndex)
      pswp.updateSize(true)
    }
  }

  async function doDelete() {
    const id = currentId
    if (!id) return
    const pswp = lightbox.pswp
    const data = currentSlideData()
    if (!data || data.deleted) return
    if (!confirm('确定删除这张图片？\n\n这会同时删除磁盘上的图片文件，且不可恢复。')) return
    try {
      await api(`/api/images/${id}`, { method: 'DELETE' })
      mutated = true
      viewerRemoveId(id)
      markDeleted(data)
      // 不关闭灯箱：就地停住，自动滑到相邻一张继续看。
      // 只剩这一张时没有邻居可去，才退回关闭。
      const idx = pswp?.currIndex ?? 0
      if (!pswp || items.length <= 1) {
        pswp?.close()
        return
      }
      pswp.goTo(idx < items.length - 1 ? idx + 1 : idx - 1)
    } catch {
      flashFab('fab-delete', '删除失败', '删除')
    }
  }

  lightbox.on('uiRegister', () => {
    const pswp = lightbox.pswp!
    pswp.ui?.registerElement({
      name: 'ps-caption',
      order: 8,
      className: 'ps-caption',
      appendTo: 'root',
      onInit: (element: HTMLElement) => {
        captionEl = element
        element.addEventListener('click', async e => {
          const target = e.target as HTMLElement
          const action = target.closest<HTMLElement>('[data-action]')?.dataset.action
          if (!action) return
          const detail = detailCache.get(currentId)
          if (action === 'toggle') {
            expanded = !expanded
            renderCaption(detail || null, !detail)
          } else if (action === 'copy-positive' && detail) {
            if (await copyText(detail.positive_prompt)) flashCopied(target)
          } else if (action === 'copy-negative' && detail) {
            if (await copyText(detail.negative_prompt)) flashCopied(target)
          } else if (action === 'go-prompt' && detail?.prompt_id) {
            cb.goToPrompt?.(detail.prompt_id)
            pswp.close()
          } else if (action === 'download') {
            window.open(`/api/images/${currentId}/download`, '_blank')
          }
        })
      },
    } as never)

    // 浮动操作列挂在 PhotoSwipe 根节点内，用纯图标 + 悬停说明，尽量不遮挡画面。
    // 三个按钮的文案由 data-tip 提供，不要改回可见文字。
    if (!fabEl) {
      fabEl = document.createElement('div')
      fabEl.className = 'ps-action-fab'
      fabEl.innerHTML = `
        <button class="ps-fab-btn" data-action="fab-original" data-tip="查看原图" aria-label="查看原图" type="button">
          <span class="ps-fab-hd">HD</span>
        </button>
        <button class="ps-fab-btn" data-action="fab-favorite" data-tip="收藏" aria-label="收藏" aria-pressed="false" type="button">
          <svg data-icon="star" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round" aria-hidden="true">
            <path d="M12 3.7l2.6 5.3 5.8.9-4.2 4.1 1 5.8-5.2-2.8-5.2 2.8 1-5.8-4.2-4.1 5.8-.9z"/>
          </svg>
        </button>
        <button class="ps-fab-btn danger" data-action="fab-delete" data-tip="删除" aria-label="删除" type="button">
          <svg data-icon="trash" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M4 7h16"/>
            <path d="M9.5 7V5.3c0-.7.6-1.3 1.3-1.3h2.4c.7 0 1.3.6 1.3 1.3V7"/>
            <path d="M6.5 7l.8 12.1c.1 1 .9 1.9 2 1.9h5.4c1.1 0 1.9-.8 2-1.9L17.5 7"/>
            <path d="M10.5 11v6M13.5 11v6"/>
          </svg>
        </button>`
      // 关键：阻止 PhotoSwipe 把按钮上的按下当作图片拖拽（否则 click 会被吞，按钮点不动）
      fabEl.addEventListener('pointerdown', e => e.stopPropagation())
      fabEl.addEventListener('click', async e => {
        const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action]')
        if (!btn) return
        const action = btn.getAttribute('data-action')
        e.preventDefault()
        e.stopPropagation()
        if (action === 'fab-favorite') {
          await doFavorite()
        } else if (action === 'fab-original') {
          await toggleOriginal()
        } else if (action === 'fab-delete') {
          await doDelete()
        }
      })
      // 挂在 PhotoSwipe 根节点内（而非 body），用超高 z-index 保证盖在灯箱 UI 之上
      pswp.element.appendChild(fabEl)
      renderFab(detailCache.get(currentId) || null)
    }
  })

  lightbox.on('change', () => {
    const pswp = lightbox.pswp
    if (pswp) lastIndex = pswp.currIndex
    const data = currentSlideData()
    if (data?.deleted) {
      // 退回到已删除的占位图：不拉详情，按钮也没有可操作的对象
      currentId = data.psId
      renderDeletedCaption(data.psId)
      setFabDisabled(true)
      return
    }
    setFabDisabled(false)
    if (data?.psId) showCaptionFor(data.psId)
    if (pswp) {
      // 换图后按钮要回到「查看原图」状态（上一张切过原图不影响这一张）
      syncOriginalButtons()
      // 历史数据兜底：划到哪张就先量哪张（插队），量到后重建这张的幻灯片
      requestMeasure(pswp.currIndex, true)
      if (prefetch) maybePrefetch(pswp.currIndex)
    }
  })

  lightbox.on('destroy', () => {
    activeIds = []
    // FAB 挂在 body 上，必须由我们手动移除，否则会残留
    if (fabEl && fabEl.parentNode) fabEl.parentNode.removeChild(fabEl)
    captionEl = null
    fabEl = null
    // onClose 优先：它同时负责「切到当前页」和「按需刷新」，避免父组件刷两遍
    if (cb.onClose) cb.onClose(lastIndex, mutated)
    else if (mutated) cb.mutated?.()
  })

  // 首帧这张要是历史数据（缺尺寸），先单独量一次再开——只发一个请求，
  // 不会重演「整页并发拉原图、超时全退成方图」的老问题。
  if (unknownIdx.has(startIndex)) {
    const size = await measureImage(items[startIndex].src)
    if (size) {
      items[startIndex].width = size.width
      items[startIndex].height = size.height
      unknownIdx.delete(startIndex)
    }
  }

  try {
    await lightbox.loadAndOpen(startIndex, items)
  } catch (e) {
    activeIds = []
    lightbox.destroy()
    throw e
  }
}
