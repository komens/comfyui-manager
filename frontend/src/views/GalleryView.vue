<script setup lang="ts">
import FilterToolbar from '../components/FilterToolbar.vue'
import ViewFeedback from '../components/ViewFeedback.vue'
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import PlatformBadge from '../components/PlatformBadge.vue'
import { api } from '../api/client'
import { openImageViewer } from '../utils/imageViewer'
import { previewUrl, thumbUrl } from '../utils/imageUrls'
import { useUrlState } from '../composables/useUrlState'
import { rememberGalleryScroll } from '../utils/galleryScroll'
import { ensurePlatforms, platforms, type Platform } from '../utils/platforms'

type ImageItem = { id: number; filename: string; created_at: string; is_favorite: boolean; prompt_id: number; prompt_title: string; width: number; height: number; source_platform: string }
type PageResult = { items: ImageItem[]; total: number; page: number; page_size: number }
type BatchResult = { applied: number; skipped: number }

const router = useRouter()
const route = useRoute()

const images = ref<ImageItem[]>([])
const loading = ref(true)
const loadError = ref(false)
const scrollReady = ref(false)
const resultsStart = ref<HTMLElement | null>(null)
let loadController: AbortController | undefined
const batchBusy = ref(false)
const message = ref('')
const messageKind = ref<'success' | 'error'>('success')
const total = ref(0)

// 分页与筛选进 URL query：从详情页返回、刷新、分享链接都保持当前位置
const state = useUrlState({
  page: 1,
  page_size: 50,
  favorite: false,
  q: '',
  group: '',
  platform: '',
}, {
  onExternalSync: () => {
    clearSelection()
    load()
  },
})

const platformOptions = ref<Platform[]>([])
const activeFilters = computed(() => [
  state.q.trim() && `搜索 · ${state.q.trim()}`,
  state.group && `分组 · ${state.group}`,
  state.platform && `来源 · ${state.platform === 'local' ? '本平台' : platformOptions.value.find(p => p.id === state.platform)?.label || state.platform}`,
  state.favorite && '仅收藏',
].filter((value): value is string => !!value))
const hasFilters = computed(() => activeFilters.value.length > 0)

type Group = { group_name: string; group_id: string; count: number }
const groups = ref<Group[]>([])

async function loadGroups() {
  try {
    groups.value = await api<Group[]>('/api/prompts/groups')
  } catch {
    groups.value = []
  }
}

const selectedIds = ref<Set<number>>(new Set())
const selectedCount = computed(() => selectedIds.value.size)
const allSelected = computed(() => images.value.length > 0 && images.value.every(i => selectedIds.value.has(i.id)))

async function load(scrollToResults = false) {
  loadController?.abort()
  const controller = new AbortController()
  loadController = controller
  loading.value = true
  loadError.value = false
  scrollReady.value = false
  try {
    const params = new URLSearchParams()
    if (state.favorite) params.set('favorite', '1')
    if (state.q) params.set('q', state.q)
    if (state.group) params.set('group', state.group)
    if (state.platform) params.set('platform', state.platform)
    params.set('page', String(state.page))
    params.set('page_size', String(state.page_size))
    const result = await api<PageResult>(`/api/images?${params}`, { signal: controller.signal })
    if (controller.signal.aborted) return
    images.value = result.items
    total.value = result.total
    // 只保留当前页仍存在的 id，避免残留"幽灵选中项"导致误删
    const alive = new Set(result.items.map(i => i.id))
    selectedIds.value = new Set([...selectedIds.value].filter(id => alive.has(id)))
  } catch {
    if (controller.signal.aborted) return
    images.value = []
    total.value = 0
    clearSelection()
    loadError.value = true
  } finally {
    if (!controller.signal.aborted) {
      loading.value = false
      await nextTick()
      if (!controller.signal.aborted) {
        layoutMasonry()
        scrollReady.value = true
        if (scrollToResults && !loadError.value && resultsStart.value) {
          const headerHeight = document.querySelector('.mobile-header')?.getBoundingClientRect().height ?? 0
          const top = window.scrollY + resultsStart.value.getBoundingClientRect().top - headerHeight - 12
          window.scrollTo({ top, behavior: 'auto' })
        }
      }
    }
  }
}

function clearSelection() {
  selectedIds.value = new Set()
}

// —— 图片信息弹窗：点击卡片底部标题文案唤起，展示提示词等生成数据 ——
type ImageDetail = { id: number; filename: string; created_at: string; is_favorite: boolean; prompt_id: number; prompt_title: string; positive_prompt: string; negative_prompt: string; source_platform: string }

const detailOpen = ref(false)
const detailLoading = ref(false)
const detail = ref<ImageDetail | null>(null)
const detailImage = ref<ImageItem | null>(null)
const copiedKey = ref('')

async function openDetail(image: ImageItem) {
  detailOpen.value = true
  detailLoading.value = true
  detail.value = null
  detailImage.value = image
  try {
    detail.value = await api<ImageDetail>(`/api/images/${image.id}`)
  } catch {
    flash('加载图片信息失败')
    closeDetail()
  } finally {
    detailLoading.value = false
  }
}

function closeDetail() {
  detailOpen.value = false
  detailImage.value = null
  detail.value = null
}

async function copyPrompt(key: 'positive' | 'negative') {
  const text = key === 'positive' ? detail.value?.positive_prompt : detail.value?.negative_prompt
  if (!text) return
  let ok = false
  try {
    await navigator.clipboard.writeText(text)
    ok = true
  } catch {
    // 非安全上下文（局域网 IP 访问）没有 clipboard API，退回 execCommand
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      ok = document.execCommand('copy')
      document.body.removeChild(ta)
    } catch {
      ok = false
    }
  }
  if (ok) {
    copiedKey.value = key
    setTimeout(() => { if (copiedKey.value === key) copiedKey.value = '' }, 1500)
  } else {
    flash('复制失败，请手动选择文本')
  }
}

function onPageChange(p: number) {
  if (p === state.page) return
  clearTimeout(searchTimer)
  state.page = p
  clearSelection()
  void load(true)
}

function onPageSizeChange() {
  clearTimeout(searchTimer)
  state.page = 1
  clearSelection()
  void load(true)
}

function onFavOnlyChange() {
  state.favorite = !state.favorite
  state.page = 1
  clearSelection()
  load()
}

function clearFilters() {
  state.q = ''
  state.group = ''
  state.platform = ''
  state.favorite = false
  onFilterChange()
}

function onFilterChange() {
  clearTimeout(searchTimer)
  state.page = 1
  clearSelection()
  load()
}

let searchTimer: ReturnType<typeof setTimeout> | undefined
function onSearch() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(onFilterChange, 300)
}

function toggleSelect(id: number) {
  const s = new Set(selectedIds.value)
  if (s.has(id)) s.delete(id)
  else s.add(id)
  selectedIds.value = s
}

function toggleSelectAll() {
  selectedIds.value = allSelected.value ? new Set() : new Set(images.value.map(i => i.id))
}

function flash(msg: string, kind: 'success' | 'error' = 'success') {
  messageKind.value = kind
  message.value = msg
  setTimeout(() => {
    if (message.value === msg) message.value = ''
  }, 3000)
}

async function batchFavorite(fav: boolean) {
  const ids = [...selectedIds.value]
  if (!ids.length) return
  batchBusy.value = true
  try {
    const r = await api<BatchResult>('/api/images/batch-favorite', {
      method: 'POST',
      body: JSON.stringify({ ids, favorite: fav }),
    })
    flash(`已${fav ? '收藏' : '取消收藏'} ${r.applied} 张${r.skipped ? `，跳过 ${r.skipped} 张` : ''}`)
    clearSelection()
    await load()
  } catch (e) {
    flash(e instanceof Error ? e.message : '操作失败', 'error')
  } finally {
    batchBusy.value = false
  }
}

async function batchDelete() {
  const ids = [...selectedIds.value]
  if (!ids.length) return
  if (!confirm(`确定删除选中的 ${ids.length} 张图片？\n\n这会同时删除磁盘上的图片文件，且不可恢复。`)) return
  batchBusy.value = true
  try {
    const r = await api<BatchResult>('/api/images/batch-delete', {
      method: 'POST',
      body: JSON.stringify({ ids }),
    })
    flash(`已删除 ${r.applied} 张${r.skipped ? `，跳过 ${r.skipped} 张` : ''}`)
    clearSelection()
    await load()
  } catch (e) {
    flash(e instanceof Error ? e.message : '删除失败', 'error')
  } finally {
    batchBusy.value = false
  }
}

/** 用后端返回的原图宽高给缩略图占位：图片加载前盒子高度就是对的，不会跳动 */
function thumbAspect(image: ImageItem) {
  return image.width > 0 && image.height > 0 ? { aspectRatio: `${image.width} / ${image.height}` } : {}
}

// —— 瀑布流 ——
// 用 grid 跨行实现（grid-auto-rows 8px + 每张算 span），不用绝对定位：
// 列数交给 CSS 的 auto-fill，卡片保持文档流顺序（从左到右、从上到下），
// Tab 焦点、读屏顺序、选中框都不会乱。
const gridEl = ref<HTMLElement | null>(null)
let gridObserver: ResizeObserver | null = null
let layoutRaf = 0

/**
 * 重排一次：先清空 span 让卡片回到自然高度，量完再按行高换算跨行数。
 * row-gap 是 0，纵向间距由 span 一起带出来，舍入误差压在 8px 以内。
 */
function layoutMasonry() {
  const grid = gridEl.value
  if (!grid) return
  const cards = Array.from(grid.children as HTMLCollectionOf<HTMLElement>)
  if (!cards.length) return
  cards.forEach(c => { c.style.gridRowEnd = '' })
  const cs = getComputedStyle(grid)
  const rowH = parseFloat(cs.getPropertyValue('grid-auto-rows')) || 8
  const spacing = parseFloat(cs.getPropertyValue('column-gap')) || 16
  // 一次读完所有高度再统一写：逐张「读高度→写 span」会触发 N 次强制重排
  const heights = cards.map(c => c.getBoundingClientRect().height)
  cards.forEach((card, i) => {
    const h = heights[i]
    if (h <= 0) return
    card.style.gridRowEnd = `span ${Math.max(1, Math.ceil((h + spacing) / rowH))}`
  })
}

function scheduleMasonry() {
  if (layoutRaf) cancelAnimationFrame(layoutRaf)
  layoutRaf = requestAnimationFrame(() => {
    layoutRaf = 0
    layoutMasonry()
  })
}

function saveScrollForReload() {
  if (route.path === '/gallery' && scrollReady.value) rememberGalleryScroll(route.fullPath)
}

// 只看宽度变化：高度变化正是重排自己造成的，跟着它重排会死循环
function observeGrid(el: HTMLElement) {
  if (!gridObserver) {
    let lastWidth = -1
    gridObserver = new ResizeObserver(entries => {
      const w = entries[0].contentRect.width
      if (Math.abs(w - lastWidth) < 1) return
      lastWidth = w
      scheduleMasonry()
    })
  }
  gridObserver.observe(el)
}

watch(gridEl, (el, previous) => {
  if (previous) gridObserver?.unobserve(previous)
  if (!el) return
  observeGrid(el)
  scheduleMasonry()
})

onBeforeUnmount(() => {
  window.removeEventListener('scroll', saveScrollForReload)
  loadController?.abort()
  clearTimeout(searchTimer)
  gridObserver?.disconnect()
  gridObserver = null
  if (layoutRaf) cancelAnimationFrame(layoutRaf)
})

function openViewer(index: number) {
  let leavingForPrompt = false
  // 带上宽高：查看器按真比例排布，跨页预取也不用再逐张拉原图量尺寸
  const refs = images.value.map(i => ({ id: i.id, width: i.width, height: i.height }))
  openImageViewer(
    refs,
    index,
    {
      goToPrompt: id => {
        leavingForPrompt = true
        void router.push(`/prompts/${id}`)
      },
      // 关闭时按「最后停留的那张」定位页码：翻到第 3 页的图再关，
      // 列表就停在第 3 页，而不是退回点开时的第 1 页。
      onClose: (lastIndex, changed) => {
        if (leavingForPrompt || route.path !== '/gallery') return
        const page = Math.floor(lastIndex / state.page_size) + 1
        const pageChanged = page !== state.page
        if (pageChanged) state.page = page
        if (pageChanged || changed) {
          clearSelection()
          void load(pageChanged)
        }
      },
    },
    {
      page: state.page,
      pageSize: state.page_size,
      total: total.value,
      filters: { favorite: state.favorite, q: state.q, group: state.group, platform: state.platform },
    },
  )
}

async function toggleFavorite(image: ImageItem) {
  try {
    const result = await api<{ is_favorite: boolean }>(`/api/images/${image.id}/favorite`, { method: 'PATCH' })
    // 后端以提示词为主体级联收藏：同提示词的所有图片共享收藏态，本地同步刷新
    for (const img of images.value) {
      if (image.prompt_id && img.prompt_id === image.prompt_id) img.is_favorite = result.is_favorite
      else if (!image.prompt_id && img.id === image.id) img.is_favorite = result.is_favorite
    }
  } catch {
    // ignore
  }
}

onMounted(() => {
  window.addEventListener('scroll', saveScrollForReload, { passive: true })
  load()
  loadGroups()
  ensurePlatforms().then(list => { platformOptions.value = list })
})
</script>

<template>
  <PageHeader title="图片库" description="查看已生成的图片，支持预览、下载、提示词查看与批量管理。" />

  <FilterToolbar :active="activeFilters" @clear="clearFilters">
    <el-input
      v-model="state.q"
      type="text"
      placeholder="搜索提示词或文件名..."
      aria-label="搜索提示词或文件名"
      class="toolbar-search"
      @input="onSearch" />
    <el-button size="small" :aria-pressed="state.favorite" :type="state.favorite ? 'primary' : 'default'" @click="onFavOnlyChange">
      {{ state.favorite ? '★ 仅收藏' : '☆ 仅收藏' }}
    </el-button>
    <template #secondary>
      <el-select v-model="state.group" aria-label="图片分组" class="filter-select filter-select-wide" @change="onFilterChange" :empty-values="[]">
        <el-option value="" label="全部分组" />
        <el-option v-for="g in groups" :key="g.group_name" :value="g.group_name" :label="`${g.group_name}（${g.count}）`" />
      </el-select>
      <el-select v-model="state.platform" aria-label="图片来源" class="filter-select" @change="onFilterChange" :empty-values="[]">
        <el-option value="" label="全部来源" />
        <el-option value="local" label="本平台" />
        <el-option v-for="p in platformOptions" :key="p.id" :value="p.id" :label="p.label" />
      </el-select>
    </template>
  </FilterToolbar>

  <!-- 多选操作栏 -->
  <div class="batch-bar">
    <el-checkbox class="checkbox-wrap" :model-value="allSelected" :disabled="!images.length" @change="toggleSelectAll">全选本页</el-checkbox>
    <span class="batch-info">已选 {{ selectedCount }} 张</span>
    <template v-if="selectedCount > 0">
      <el-button text size="small" :disabled="batchBusy" @click="batchFavorite(true)">★ 收藏</el-button>
      <el-button text size="small" :disabled="batchBusy" @click="batchFavorite(false)">☆ 取消收藏</el-button>
      <el-button type="danger" text size="small" :disabled="batchBusy" @click="batchDelete">&#128465; 批量删除</el-button>
      <el-button text size="small" @click="clearSelection">取消选择</el-button>
    </template>
  </div>
  <ViewFeedback :kind="messageKind" :message="message" />

  <div ref="resultsStart" :data-gallery-ready="scrollReady ? route.fullPath : undefined"></div>
  <div v-if="loadError || !images.length" class="card">
    <ViewFeedback v-if="loading" kind="loading" />
    <ViewFeedback v-else-if="loadError" kind="error" message="无法加载图片">
      <el-button text size="small" @click="load()">重试</el-button>
    </ViewFeedback>
    <ViewFeedback v-else kind="empty" :message="hasFilters ? '没有匹配的图片' : '图片库还没有素材'">
      <RouterLink v-if="!hasFilters" class="btn btn-primary btn-sm" to="/submit">创建作图任务</RouterLink>
    </ViewFeedback>
  </div>

  <!-- Image Grid：瀑布流，卡片高度按图片比例（span 由 layoutMasonry 计算） -->
  <div
    v-if="images.length"
    ref="gridEl"
    class="image-grid"
    @load.capture="scheduleMasonry"
    @error.capture="scheduleMasonry"
  >
    <div
      v-for="(image, index) in images"
      :key="image.id"
      class="image-card"
      :class="{ selected: selectedIds.has(image.id) }"
    >
      <el-checkbox class="checkbox-wrap image-check" :aria-label="'选择图片 ' + image.filename" :model-value="selectedIds.has(image.id)" @change="toggleSelect(image.id)" />
      <button type="button" class="fav-badge" :aria-pressed="image.is_favorite"
        :aria-label="'收藏图片 ' + image.filename" @click="toggleFavorite(image)">{{ image.is_favorite ? '★' : '☆' }}</button>
      <button type="button" class="image-thumb-wrap" :aria-label="'预览图片 ' + image.filename" @click="openViewer(index)">
        <img class="image-thumb" :src="previewUrl(image.id)" :alt="image.filename" :style="thumbAspect(image)" loading="lazy" decoding="async" />
        <PlatformBadge v-if="image.source_platform" :platform="image.source_platform" class="platform-tag" />
      </button>
      <button type="button" class="image-title image-title-btn"
        :title="image.prompt_title || image.filename"
        :aria-label="'查看图片详情 ' + (image.prompt_title || image.filename)" @click="openDetail(image)">
        {{ image.prompt_title || image.filename }}
        <span class="title-info-hint" aria-hidden="true">&#9432;</span>
      </button>
    </div>
  </div>

  <div v-if="total > 0" class="pagination-footer">
    <Pagination
      :total="total"
      :page="state.page"
      :page-size="state.page_size"
      @update:page="onPageChange"
    />
    <div class="page-size-wrap">
      <span class="text-xs muted">每页</span>
      <el-select v-model="state.page_size" class="page-size-select" size="small" aria-label="每页条数" @change="onPageSizeChange">
        <el-option :value="20" label="20" />
        <el-option :value="50" label="50" />
        <el-option :value="100" label="100" />
        <el-option :value="200" label="200" />
      </el-select>
      <span class="text-xs muted">条</span>
    </div>
  </div>

  <el-dialog
    :model-value="detailOpen"
    title="图片信息"
    class="app-dialog"
    width="min(560px, calc(100% - 24px))"
    top="6vh"
    append-to-body
    @update:model-value="open => { if (!open) closeDetail() }"
  >
    <div class="detail-head">
      <img v-if="detailImage" class="detail-thumb" :src="thumbUrl(detailImage.id)" :alt="detailImage.filename" />
      <div class="detail-meta">
        <div class="detail-title-row">
          <span class="detail-id">#{{ detailImage?.id }}</span>
          <PlatformBadge v-if="detail?.source_platform" :platform="detail.source_platform" size="md" />
          <span v-if="detail?.is_favorite" class="detail-fav">★ 已收藏</span>
        </div>
        <div class="detail-filename" :title="detail?.filename">{{ detail?.filename || '…' }}</div>
        <div v-if="detail" class="text-xs muted">{{ new Date(detail.created_at).toLocaleString('zh-CN') }}</div>
        <RouterLink v-if="detail && detail.prompt_id" :to="`/prompts/${detail.prompt_id}`" class="detail-prompt-link">
          查看关联提示词 &rarr;
        </RouterLink>
        <div v-else-if="detail" class="text-xs muted">手动提交，未入库</div>
      </div>
    </div>

    <div v-if="detailLoading" class="detail-loading">
      <span class="spinner"></span><span class="text-xs muted">加载图片信息…</span>
    </div>
    <template v-else-if="detail">
      <section class="detail-prompt">
        <div class="detail-prompt-head">
          <span class="dp-label"><span class="label-dot positive"></span>正面提示词</span>
          <el-button v-if="detail.positive_prompt" text size="small" @click="copyPrompt('positive')">
            {{ copiedKey === 'positive' ? '已复制 ✓' : '复制' }}
          </el-button>
        </div>
        <p class="detail-prompt-body">{{ detail.positive_prompt || '（无）' }}</p>
      </section>
      <section v-if="detail.negative_prompt" class="detail-prompt">
        <div class="detail-prompt-head">
          <span class="dp-label"><span class="label-dot negative"></span>负面提示词</span>
          <el-button text size="small" @click="copyPrompt('negative')">
            {{ copiedKey === 'negative' ? '已复制 ✓' : '复制' }}
          </el-button>
        </div>
        <p class="detail-prompt-body">{{ detail.negative_prompt }}</p>
      </section>
    </template>
    <template #footer>
      <a v-if="detailImage" :href="`/api/images/${detailImage.id}/download`" class="el-button">下载</a>
      <el-button @click="closeDetail">关闭</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.fav-badge {
  position: absolute;
  top: 8px;
  right: 8px;
  display: grid;
  width: 32px;
  height: 32px;
  place-items: center;
  border: 0;
  border-radius: 6px;
  background: rgba(0,0,0,.45);
  font-size: 18px;
  cursor: pointer;
  color: rgba(255,255,255,0.85);
  text-shadow: 0 1px 4px rgba(0,0,0,0.6);
  z-index: 2;
  transition: transform 0.2s;
  line-height: 1;
}
.fav-badge:hover { transform: scale(1.3); }
.image-card { position: relative; overflow: hidden; }
/* 平台角标要贴着图片，不能相对卡片定位——卡片底部还有标题行，会错位 */
.image-thumb-wrap { position: relative; display: block; width: 100%; border: 0; background: transparent; cursor: zoom-in; }
.platform-tag {
  position: absolute;
  left: 8px;
  bottom: 8px;
  z-index: 2;
  box-shadow: 0 1px 6px rgba(0, 0, 0, 0.35);
}
.fav-btn {
  background: var(--c-bg-subtle);
  color: var(--c-muted);
  border: 1px solid var(--c-border);
}
.fav-btn.fav-active,
.fav-btn.fav-active:hover {
  background: #fff7ed;
  color: #d97706;
  border-color: #fbbf24;
}
.fav-btn:hover {
  background: #fffbeb;
  color: #b45309;
}
.fav-btn:disabled { opacity: 0.5; cursor: not-allowed; }

/* 多选 */
.batch-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  background: var(--c-primary-light);
  border: 1px solid var(--c-primary-border);
  border-radius: 8px;
  margin-bottom: 12px;
  flex-wrap: wrap;
}
.batch-info { font-size: 12px; font-weight: 600; color: var(--c-muted); }
.batch-msg { margin-left: auto; color: var(--c-success); }
.checkbox-wrap { display: inline-flex; align-items: center; gap: 6px; cursor: pointer; }
.checkbox-wrap input { width: 15px; height: 15px; cursor: pointer; accent-color: var(--c-primary); }
.image-check {
  position: absolute;
  top: 8px;
  left: 8px;
  z-index: 3;
  background: rgba(0,0,0,0.45);
  border-radius: 6px;
  padding: 3px 5px;
  line-height: 0;
}
.image-card.selected {
  outline: 2px solid var(--c-primary);
  outline-offset: -2px;
}
.image-title {
  padding: 7px 9px;
  font-size: 12px;
  color: var(--c-text);
  background: var(--c-bg-subtle);
  border-top: 1px solid var(--c-border);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
/* 标题文案行：点击查看图片信息 */
.image-title-btn { width: 100%; border: 0; border-top: 1px solid var(--c-border); text-align: left; cursor: pointer; }
.image-thumb-wrap:focus-visible, .image-title-btn:focus-visible, .fav-badge:focus-visible { outline: 2px solid var(--c-primary); outline-offset: -2px; }
.image-title-btn:hover { color: var(--c-primary); background: var(--c-surface-hover); }
.image-title-btn:hover .title-info-hint { opacity: 1; }
.title-info-hint { float: right; opacity: 0; color: var(--c-primary); transition: opacity 0.15s; }

/* 图片信息弹窗 */
.detail-head { display: flex; gap: 14px; margin-bottom: 16px; }
.detail-thumb { width: 120px; height: 120px; object-fit: cover; border-radius: 10px; border: 1px solid var(--c-border); flex-shrink: 0; background: var(--c-bg-subtle); }
.detail-meta { min-width: 0; display: flex; flex-direction: column; gap: 4px; }
.detail-title-row { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.detail-id { font-weight: 700; color: var(--c-primary); }
.detail-fav { font-size: 12px; color: #d97706; }
.detail-filename { font-size: 13px; word-break: break-all; }
.detail-prompt-link { font-size: 13px; color: var(--c-primary); font-weight: 600; margin-top: 2px; }
.detail-prompt-link:hover { text-decoration: underline; }
.detail-loading { display: flex; align-items: center; gap: 8px; padding: 14px 0; }
.detail-prompt { border: 1px solid var(--c-border); border-radius: 10px; padding: 10px 12px; margin-bottom: 10px; background: var(--c-bg-subtle); }
.detail-prompt-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-bottom: 6px; }
.dp-label { display: flex; align-items: center; gap: 6px; font-size: 12px; font-weight: 700; color: var(--c-muted); }
/* 不单独限高滚动：长文本完整展开，由弹窗外层统一滚动，避免双滚动条 */
.detail-prompt-body { font-size: 13px; line-height: 1.6; white-space: pre-wrap; word-break: break-word; }
@media (max-width: 480px) {
  .detail-thumb { width: 80px; height: 80px; }
}
</style>
