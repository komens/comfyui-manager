// 图片 URL 与尺寸约定。与后端 backend/cmd/server/thumbs.go 的两档一一对应，
// 改这里的常量必须同步改后端，否则 PhotoSwipe 的 data.width/height 会和真实图不符。
//
//   thumb   —— 小图标（列表 / 卡片里的方形小图，最大显示 140px）
//   preview —— 大图预览，同时给图库网格卡片用（两处共用一个 URL，
//              点开大图时浏览器缓存直接命中，不会二次下载）
//   file    —— 原图，只给「查看原图」和下载用

export const THUMB_SHORT_EDGE = 480
export const PREVIEW_MAX_EDGE = 1600

export function thumbUrl(id: number): string {
  return `/api/images/${id}/thumb`
}

export function previewUrl(id: number): string {
  return `/api/images/${id}/preview`
}

export function originalUrl(id: number): string {
  return `/api/images/${id}/file`
}

/**
 * preview 那张图的实际像素：长边压到 PREVIEW_MAX_EDGE，等比、不放大。
 * 原图比上限小时原样返回（后端也是这么处理的）。
 */
export function previewSize(width: number, height: number): { width: number; height: number } {
  if (!(width > 0) || !(height > 0)) return { width, height }
  const longest = Math.max(width, height)
  if (longest <= PREVIEW_MAX_EDGE) return { width, height }
  const scale = PREVIEW_MAX_EDGE / longest
  return {
    width: Math.max(1, Math.round(width * scale)),
    height: Math.max(1, Math.round(height * scale)),
  }
}
