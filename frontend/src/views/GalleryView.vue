<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api } from '../api/client'
import { openImageViewer } from '../utils/imageViewer'
import { useUrlState } from '../composables/useUrlState'

type ImageItem = { id: number; filename: string; created_at: string; is_favorite: boolean; prompt_id: number; prompt_title: string }
type PageResult = { items: ImageItem[]; total: number; page: number; page_size: number }
type BatchResult = { applied: number; skipped: number }

const router = useRouter()

const images = ref<ImageItem[]>([])
const loading = ref(true)
const batchBusy = ref(false)
const message = ref('')
const total = ref(0)

// 分页与收藏筛选进 URL query：从详情页返回、刷新、分享链接都保持当前位置
const state = useUrlState({
  page: 1,
  page_size: 20,
  favorite: false,
}, {
  onExternalSync: () => {
    clearSelection()
    load()
  },
})

const selectedIds = ref<Set<number>>(new Set())
const selectedCount = computed(() => selectedIds.value.size)
const allSelected = computed(() => images.value.length > 0 && images.value.every(i => selectedIds.value.has(i.id)))

async function load() {
  loading.value = true
  try {
    const params = new URLSearchParams()
    if (state.favorite) params.set('favorite', '1')
    params.set('page', String(state.page))
    params.set('page_size', String(state.page_size))
    const result = await api<PageResult>(`/api/images?${params}`)
    images.value = result.items
    total.value = result.total
    // 只保留当前页仍存在的 id，避免残留"幽灵选中项"导致误删
    const alive = new Set(result.items.map(i => i.id))
    selectedIds.value = new Set([...selectedIds.value].filter(id => alive.has(id)))
  } catch {
    images.value = []
  } finally {
    loading.value = false
  }
}

function clearSelection() {
  selectedIds.value = new Set()
}

// —— 图片信息弹窗：点击卡片底部标题文案唤起，展示提示词等生成数据 ——
type ImageDetail = { id: number; filename: string; created_at: string; is_favorite: boolean; prompt_id: number; prompt_title: string; positive_prompt: string; negative_prompt: string }

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
  state.page = p
  clearSelection()
  load()
}

function onPageSizeChange() {
  state.page = 1
  clearSelection()
  load()
}

function onFavOnlyChange() {
  state.favorite = !state.favorite
  state.page = 1
  clearSelection()
  load()
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

function flash(msg: string) {
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
    flash(e instanceof Error ? e.message : '操作失败')
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
    flash(e instanceof Error ? e.message : '删除失败')
  } finally {
    batchBusy.value = false
  }
}

function openViewer(index: number) {
  const ids = images.value.map(i => i.id)
  openImageViewer(ids, index, {
    goToPrompt: id => router.push(`/prompts/${id}`),
    mutated: () => load(),
  })
}

async function toggleFavorite(image: ImageItem) {
  try {
    const result = await api<{ is_favorite: boolean }>(`/api/images/${image.id}/favorite`, { method: 'PATCH' })
    image.is_favorite = result.is_favorite
  } catch {
    // ignore
  }
}

onMounted(load)
</script>

<template>
  <PageHeader eyebrow="IMAGE LIBRARY" title="图片库" description="查看已生成的图片，支持预览、下载、提示词查看与批量管理。" />

  <div class="toolbar">
    <button class="btn btn-sm" :class="{ 'btn-primary': state.favorite }" @click="onFavOnlyChange">
      {{ state.favorite ? '★ 仅收藏' : '☆ 仅收藏' }}
    </button>
  </div>

  <!-- 多选操作栏 -->
  <div class="batch-bar">
    <label class="checkbox-wrap">
      <input type="checkbox" :checked="allSelected" :disabled="!images.length" @change="toggleSelectAll" />
      <span class="text-xs">全选本页</span>
    </label>
    <span class="batch-info">已选 {{ selectedCount }} 张</span>
    <template v-if="selectedCount > 0">
      <button class="btn btn-ghost btn-sm" :disabled="batchBusy" @click="batchFavorite(true)">★ 收藏</button>
      <button class="btn btn-ghost btn-sm" :disabled="batchBusy" @click="batchFavorite(false)">☆ 取消收藏</button>
      <button class="btn btn-ghost btn-sm btn-danger" :disabled="batchBusy" @click="batchDelete">&#128465; 批量删除</button>
      <button class="btn btn-ghost btn-sm" @click="clearSelection">取消选择</button>
    </template>
    <span v-if="message" class="text-xs batch-msg">{{ message }}</span>
  </div>

  <!-- Loading -->
  <div v-if="loading" class="card">
    <div class="card-body flex items-center gap-3">
      <span class="spinner"></span>
      <span class="muted">加载中...</span>
    </div>
  </div>

  <!-- Empty -->
  <div v-else-if="!images.length" class="card">
    <div class="empty-state">
      <div class="empty-icon">&#128247;</div>
      <p>暂无生成图片，提交任务后图片将显示在这里。</p>
    </div>
  </div>

  <!-- Image Grid -->
  <div v-if="images.length" class="image-grid">
    <div
      v-for="(image, index) in images"
      :key="image.id"
      class="image-card"
      :class="{ selected: selectedIds.has(image.id) }"
      @click="openViewer(index)"
    >
      <label class="checkbox-wrap image-check" @click.stop>
        <input type="checkbox" :checked="selectedIds.has(image.id)" @change="toggleSelect(image.id)" />
      </label>
      <span class="fav-badge" @click.stop="toggleFavorite(image)">{{ image.is_favorite ? '★' : '☆' }}</span>
      <img class="image-thumb" :src="`/api/images/${image.id}/file`" :alt="image.filename" loading="lazy" />
      <div class="image-info">
        <span class="text-xs">#{{ image.id }}</span>
        <span class="text-xs">{{ new Date(image.created_at).toLocaleDateString('zh-CN') }}</span>
      </div>
      <div class="image-title image-title-btn" :title="image.prompt_id ? (image.prompt_title || '未命名提示词') : '手动提交'" @click.stop="openDetail(image)">
        {{ image.prompt_id ? (image.prompt_title || '未命名提示词') : '手动提交' }}
        <span class="title-info-hint">&#9432;</span>
      </div>
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
      <select v-model.number="state.page_size" class="input page-size-select" @change="onPageSizeChange">
        <option :value="10">10</option>
        <option :value="20">20</option>
        <option :value="50">50</option>
        <option :value="100">100</option>
      </select>
      <span class="text-xs muted">条</span>
    </div>
  </div>

  <!-- 图片信息弹窗 -->
  <div v-if="detailOpen" class="modal-overlay" @click.self="closeDetail">
    <div class="modal detail-modal">
      <div class="detail-head">
        <img v-if="detailImage" class="detail-thumb" :src="`/api/images/${detailImage.id}/file`" :alt="detailImage.filename" />
        <div class="detail-meta">
          <div class="detail-title-row">
            <span class="detail-id">#{{ detailImage?.id }}</span>
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
            <button v-if="detail.positive_prompt" class="btn btn-ghost btn-sm" @click="copyPrompt('positive')">
              {{ copiedKey === 'positive' ? '已复制 ✓' : '复制' }}
            </button>
          </div>
          <p class="detail-prompt-body">{{ detail.positive_prompt || '（无）' }}</p>
        </section>
        <section v-if="detail.negative_prompt" class="detail-prompt">
          <div class="detail-prompt-head">
            <span class="dp-label"><span class="label-dot negative"></span>负面提示词</span>
            <button class="btn btn-ghost btn-sm" @click="copyPrompt('negative')">
              {{ copiedKey === 'negative' ? '已复制 ✓' : '复制' }}
            </button>
          </div>
          <p class="detail-prompt-body">{{ detail.negative_prompt }}</p>
        </section>
      </template>

      <div class="detail-footer">
        <a v-if="detailImage" :href="`/api/images/${detailImage.id}/download`" class="btn btn-secondary btn-sm">&#8681; 下载</a>
        <button class="btn btn-ghost" @click="closeDetail">关闭</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.fav-badge {
  position: absolute;
  top: 8px;
  right: 8px;
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
.image-title-btn { cursor: pointer; }
.image-title-btn:hover { color: var(--c-primary); background: var(--c-surface-hover); }
.image-title-btn:hover .title-info-hint { opacity: 1; }
.title-info-hint { float: right; opacity: 0; color: var(--c-primary); transition: opacity 0.15s; }

/* 图片信息弹窗 */
.modal-overlay { position: fixed; inset: 0; background: rgba(0,0,0,0.5); display: flex; align-items: center; justify-content: center; z-index: 100; padding: 20px; }
.modal { background: var(--c-card); border-radius: 12px; padding: 24px; width: 100%; max-height: 85vh; overflow-y: auto; box-shadow: var(--shadow-lg); }
.detail-modal { max-width: 560px; }
.detail-head { display: flex; gap: 14px; margin-bottom: 16px; }
.detail-thumb { width: 120px; height: 120px; object-fit: cover; border-radius: 10px; border: 1px solid var(--c-border); flex-shrink: 0; background: var(--c-bg-subtle); }
.detail-meta { min-width: 0; display: flex; flex-direction: column; gap: 4px; }
.detail-title-row { display: flex; align-items: center; gap: 8px; }
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
.detail-footer { display: flex; justify-content: flex-end; gap: 8px; margin-top: 14px; }
</style>
