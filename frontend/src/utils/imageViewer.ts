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

export async function openImageViewer(ids: number[], startIndex: number, cb: ViewerCallbacks = {}) {
  if (!ids.length || activeIds.length) return
  activeIds = [...ids]

  // PhotoSwipe 需要宽高用于排布；当前页图片多数已在网格加载过，预取一般即时完成
  const urls = ids.map(id => `/api/images/${id}/file`)
  const sizes = await Promise.all(urls.map(loadImageSize))
  const items: SlideItem[] = ids.map((id, i) => ({
    width: sizes[i].width,
    height: sizes[i].height,
    src: urls[i],
    msrc: urls[i],
    alt: `图片 #${id}`,
    psId: id,
  }))

  const detailCache = new Map<number, ViewerDetail>()
  let mutated = false
  let expanded = typeof window !== 'undefined' && window.innerWidth > 768

  const lightbox = new PhotoSwipeLightbox({
    pswpModule: PhotoSwipe,
    loop: true,
    showHideAnimationType: 'zoom',
    bgOpacity: 0.92,
  })

  let captionEl: HTMLElement | null = null
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
          <button class="ps-caps-btn" data-action="favorite" type="button">${detail.is_favorite ? '★ 已收藏' : '☆ 收藏'}</button>
          <button class="ps-caps-btn danger" data-action="delete" type="button">删除</button>
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
      if (seq === renderSeq && id === currentId) renderCaption(detail, false)
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
          } else if (action === 'favorite' && detail) {
            try {
              const r = await api<{ is_favorite: boolean }>(`/api/images/${currentId}/favorite`, { method: 'PATCH' })
              detail.is_favorite = r.is_favorite
              mutated = true
              renderCaption(detail, false)
            } catch { /* ignore */ }
          } else if (action === 'delete') {
            if (!confirm('确定删除这张图片？\n\n这会同时删除磁盘上的图片文件，且不可恢复。')) return
            try {
              await api(`/api/images/${currentId}`, { method: 'DELETE' })
              mutated = true
              viewerRemoveId(currentId)
              pswp.close()
            } catch { /* ignore */ }
          }
        })
      },
    } as never)
  })

  lightbox.on('change', () => {
    const data = (lightbox.pswp as unknown as { currData?: SlideItem })?.currData
    if (data?.psId) showCaptionFor(data.psId)
  })

  lightbox.on('destroy', () => {
    activeIds = []
    captionEl = null
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
