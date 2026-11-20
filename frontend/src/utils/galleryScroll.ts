const key = 'gallery-refresh-scroll'

export function rememberGalleryScroll(fullPath: string) {
  try {
    sessionStorage.setItem(key, JSON.stringify({
      fullPath,
      entry: window.history.state?.position,
      left: window.scrollX,
      top: window.scrollY,
    }))
  } catch {
    // The router's history restoration also works when storage is unavailable.
  }
}

export function readGalleryScroll(fullPath: string) {
  try {
    const raw = sessionStorage.getItem(key)
    if (!raw) return undefined
    const position = JSON.parse(raw)
    if (position.fullPath === fullPath && position.entry === window.history.state?.position &&
        Number.isFinite(position.left) && Number.isFinite(position.top)) {
      return { left: Number(position.left), top: Number(position.top) }
    }
  } catch {
    // Ignore unavailable storage or an obsolete entry.
  }
}
