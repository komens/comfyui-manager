// 统一图片查看器：基于 PhotoSwipe 5（双指缩放/滑动切换/键盘导航），
// 底部挂自定义 caption：移动端默认摘要一行、点击展开，支持复制提示词、跳转来源、收藏/下载/删除。
import PhotoSwipeLightbox from 'photoswipe/lightbox'
import PhotoSwipe from 'photoswipe'
import 'photoswipe/style.css'
import { api } from '../api/client'

export type ViewerDetail = {
  id: number
  filename: string
  created_at: string
  is_favorite: boolean
  prompt_id: number
  prompt_title: string
  positive_prompt: string
  negative_prompt: string
}

export type ViewerCallbacks = {
  goToPrompt?: (promptId: number) => void
  /** 查看期间发生收藏/删除后回调，用于父列表刷新 */
  mutated?: () => void
}

type SlideItem = { width: number; height: number; src: string; msrc: string; alt: string; psId: number }

function loadImageSize(url: string): Promise<{ width: number; height: number }> {
  return new Promise(resolve => {
    const img = new Image()
    const timer = setTimeout(() => resolve({ width: 1024, height: 1024 }), 10000)
    img.onload = () => {
      clearTimeout(timer)
      resolve({ width: img.naturalWidth || 1024, height: img.naturalHeight || 1024 })
    }
    img.onerror = () => {
      clearTimeout(timer)
      resolve({ width: 1024, height: 1024 })
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

export async function openImageViewer(ids: number[], startIndex: number, cb: ViewerCallbacks = {}, prefetch?: PrefetchOptions) {
  if (!ids.length || activeIds.length) return

  let loadedIds = [...ids]
  let firstPage = 1
  let lastPage = 1
  let totalCount = ids.length

  // —— 分页预取 ——
  // 打开大图时，先把「当前页之前」的各页补齐进列表（按页请求 /api/images，
  // 不是一次性全量拉取），这样往回切能一路走完整个筛选结果集；
  // 往后切则在接近末尾时由 maybePrefetch 按需拉下一页 push 进列表。
  if (prefetch) {
    firstPage = prefetch.page
    lastPage = prefetch.page
    totalCount = prefetch.total
    if (prefetch.page > 1) {
      try {
        const pages = await Promise.all(
          Array.from({ length: prefetch.page - 1 }, (_, k) => fetchPageIds(k + 1, prefetch)),
        )
        const before = pages.flat()
        loadedIds = [...before, ...ids]
        startIndex = (prefetch.page - 1) * prefetch.pageSize + startIndex
      } catch {
        // 预取失败不阻断：退化为仅当前页
        loadedIds = [...ids]
      }
    }
  }

  activeIds = [...loadedIds]

  const urls = loadedIds.map(id => `/api/images/${id}/file`)
  const sizes = await Promise.all(urls.map(loadImageSize))
  const items: SlideItem[] = loadedIds.map((id, i) => ({
    width: sizes[i].width,
    height: sizes[i].height,
    src: urls[i],
    msrc: urls[i],
    alt: `图片 #${id}`,
    psId: id,
  }))

  const loadingPages = new Set<string>()
  async function fetchPageIds(page: number, opts: PrefetchOptions): Promise<number[]> {
    const params = new URLSearchParams()
    const f = opts.filters || {}
    if (f.favorite) params.set('favorite', '1')
    if (f.q) params.set('q', f.q)
    if (f.group) params.set('group', f.group)
    params.set('page', String(page))
    params.set('page_size', String(opts.pageSize))
    const result = await api<{ items: { id: number }[]; total: number }>(`/api/images?${params}`)
    totalCount = result.total
    return result.items.map(i => i.id)
  }
  async function maybePrefetch(cur: number) {
    const opts = prefetch
    if (!opts) return
    if (cur < items.length - 2) return
    if (loadingPages.has('next')) return
    if (lastPage * opts.pageSize >= totalCount) return
    loadingPages.add('next')
    try {
      const pageIds = await fetchPageIds(lastPage + 1, opts)
      if (!pageIds.length) return
      const sz = await Promise.all(pageIds.map(id => loadImageSize(`/api/images/${id}/file`)))
      pageIds.forEach((id, k) => {
        loadedIds.push(id)
        items.push({
          width: sz[k].width,
          height: sz[k].height,
          src: `/api/images/${id}/file`,
          msrc: `/api/images/${id}/file`,
          alt: `图片 #${id}`,
          psId: id,
        })
      })
      activeIds = [...loadedIds]
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

  let captionEl: HTMLElement | null = null
  let fabEl: HTMLElement | null = null
  let currentId = 0
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

  /** 右下角浮动按钮列：根据当前图收藏状态刷新星标 */
  function renderFab(detail: ViewerDetail | null) {
    if (!fabEl) return
    const favBtn = fabEl.querySelector<HTMLButtonElement>('[data-action="fab-favorite"]')
    if (!favBtn) return
    const fav = !!detail?.is_favorite
    favBtn.classList.toggle('active', fav)
    favBtn.textContent = fav ? '★ 已收藏' : '☆ 收藏'
  }

  async function doFavorite() {
    let detail = detailCache.get(currentId)
    if (!detail) {
      try {
        detail = await api<ViewerDetail>(`/api/images/${currentId}`)
        detailCache.set(currentId, detail)
      } catch {
        return
      }
    }
    try {
      const r = await api<{ is_favorite: boolean }>(`/api/images/${currentId}/favorite`, { method: 'PATCH' })
      detail.is_favorite = r.is_favorite
      mutated = true
      renderCaption(detail, false)
      renderFab(detail)
    } catch { /* ignore */ }
  }

  async function doDelete() {
    if (!confirm('确定删除这张图片？\n\n这会同时删除磁盘上的图片文件，且不可恢复。')) return
    try {
      await api(`/api/images/${currentId}`, { method: 'DELETE' })
      mutated = true
      viewerRemoveId(currentId)
      lightbox.pswp?.close()
    } catch { /* ignore */ }
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

    // 浮动操作列直接挂到 document.body，彻底脱离 PhotoSwipe 的 UI 层叠与
    // 图片拖拽事件体系，避免点击被吞掉（之前 appendTo:'root' 时按钮点不动）。
    if (!fabEl) {
      fabEl = document.createElement('div')
      fabEl.className = 'ps-action-fab'
      fabEl.innerHTML = `
        <button class="ps-fab-btn" data-action="fab-favorite" type="button">☆ 收藏</button>
        <button class="ps-fab-btn danger" data-action="fab-delete" type="button">🗑 删除</button>`
      // 关键：阻止 PhotoSwipe 把按钮上的按下当作图片拖拽（否则 click 会被吞，按钮点不动）
      fabEl.addEventListener('pointerdown', e => e.stopPropagation())
      fabEl.addEventListener('click', async e => {
        const btn = (e.target as HTMLElement).closest<HTMLElement>('[data-action]')
        if (!btn) return
        const action = btn.getAttribute('data-action')
        e.preventDefault()
        e.stopPropagation()
        if (action === 'fab-favorite') {
          if (!detailCache.get(currentId)) return
          await doFavorite()
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
    const data = (pswp as unknown as { currData?: SlideItem })?.currData
    if (data?.psId) showCaptionFor(data.psId)
    if (prefetch && pswp) maybePrefetch(pswp.currIndex)
  })

  lightbox.on('destroy', () => {
    activeIds = []
    // FAB 挂在 body 上，必须由我们手动移除，否则会残留
    if (fabEl && fabEl.parentNode) fabEl.parentNode.removeChild(fabEl)
    captionEl = null
    fabEl = null
    if (mutated) cb.mutated?.()
  })

  try {
    await lightbox.loadAndOpen(startIndex, items)
  } catch (e) {
    activeIds = []
    lightbox.destroy()
    throw e
  }
}
