import { api } from '../api/client'

/**
 * 外部同步平台清单。
 * 以 /api/external/platforms 为准（后端加平台前端零改动），
 * 本地这份只是兜底：接口失败或首帧未返回时，列表不能整片空白。
 */
export type Platform = { id: string; label: string; color: string }

const FALLBACK: Platform[] = [
  { id: 'chatgpt', label: 'ChatGPT', color: '#10a37f' },
  { id: 'grok', label: 'Grok', color: '#1d1d1f' },
]

let cache: Platform[] = [...FALLBACK]
let loaded = false
let inflight: Promise<Platform[]> | null = null

/** 同步取当前已知清单，供渲染直接用 */
export function platforms(): Platform[] {
  return cache
}

/** 拉一次平台清单（模块级缓存，多页面共用一次请求） */
export async function ensurePlatforms(): Promise<Platform[]> {
  if (loaded) return cache
  if (!inflight) {
    inflight = api<{ items: Platform[] }>('/api/external/platforms')
      .then(r => {
        if (Array.isArray(r.items) && r.items.length) cache = r.items
        loaded = true
        return cache
      })
      .catch(() => cache)
      .finally(() => { inflight = null })
  }
  return inflight
}

/** id → 展示信息；未知平台（例如换过版本的包）至少把 id 原样显示出来 */
export function platformMeta(id: string): Platform {
  if (!id) return { id: '', label: '', color: '' }
  return cache.find(p => p.id === id) ?? { id, label: id, color: '#6b7280' }
}
