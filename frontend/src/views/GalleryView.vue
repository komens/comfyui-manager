<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api } from '../api/client'

type ImageItem = { id: number; filename: string; created_at: string; is_favorite: boolean; prompt_id: number; prompt_title: string }
type ImageDetail = { id: number; positive_prompt: string; negative_prompt: string; created_at: string; is_favorite: boolean; prompt_id: number; prompt_title: string }
type PageResult = { items: ImageItem[]; total: number; page: number; page_size: number }
type BatchResult = { applied: number; skipped: number }

const router = useRouter()

const images = ref<ImageItem[]>([])
const loading = ref(true)
const favOnly = ref(false)
const lightbox = ref<ImageDetail | null>(null)
const lightboxLoading = ref(false)
const batchBusy = ref(false)
const message = ref('')

const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

const selectedIds = ref<Set<number>>(new Set())
const selectedCount = computed(() => selectedIds.value.size)
const allSelected = computed(() => images.value.length > 0 && images.value.every(i => selectedIds.value.has(i.id)))

async function load() {
  loading.value = true
  try {
    const params = new URLSearchParams()
    if (favOnly.value) params.set('favorite', '1')
    params.set('page', String(page.value))
    params.set('page_size', String(pageSize.value))
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

function onPageChange(p: number) {
  page.value = p
  clearSelection()
  load()
}

function onPageSizeChange() {
  page.value = 1
  clearSelection()
  load()
}

function onFavOnlyChange() {
  favOnly.value = !favOnly.value
  page.value = 1
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

async function openLightbox(id: number) {
  lightboxLoading.value = true
  try {
    lightbox.value = await api<ImageDetail>(`/api/images/${id}`)
  } catch {
    lightbox.value = { id, positive_prompt: '无法加载', negative_prompt: '', created_at: '', is_favorite: false, prompt_id: 0, prompt_title: '' }
  } finally {
    lightboxLoading.value = false
  }
}

function closeLightbox() {
  lightbox.value = null
}

function goToPrompt(id: number) {
  closeLightbox()
  router.push(`/prompts/${id}`)
}

function downloadImage(id: number) {
  window.open(`/api/images/${id}/download`, '_blank')
}

async function deleteImage() {
  if (!lightbox.value) return
  const id = lightbox.value.id
  if (!confirm(`确定删除这张图片？\n\n这会同时删除磁盘上的图片文件，且不可恢复。`)) return
  lightboxLoading.value = true
  try {
    await api(`/api/images/${id}`, { method: 'DELETE' })
    lightbox.value = null
    await load()  // 重新加载以更新 total 和分页
  } catch (e) {
    alert(e instanceof Error ? e.message : '删除失败')
  } finally {
    lightboxLoading.value = false
  }
}

async function toggleFavorite(image: ImageItem) {
  try {
    const result = await api<{ is_favorite: boolean }>(`/api/images/${image.id}/favorite`, { method: 'PATCH' })
    image.is_favorite = result.is_favorite
  } catch {
    // ignore
  }
}

async function toggleFavLightbox() {
  if (!lightbox.value) return
  try {
    const result = await api<{ is_favorite: boolean }>(`/api/images/${lightbox.value.id}/favorite`, { method: 'PATCH' })
    lightbox.value.is_favorite = result.is_favorite
    const idx = images.value.findIndex(i => i.id === lightbox.value!.id)
    if (idx >= 0) images.value[idx].is_favorite = result.is_favorite
  } catch {
    // ignore
  }
}

function handleKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape' && lightbox.value) closeLightbox()
}

onMounted(load)
</script>

<template>
  <PageHeader eyebrow="IMAGE LIBRARY" title="图片库" description="查看已生成的图片，支持预览、下载、提示词查看与批量管理。" />

  <div class="toolbar">
    <button class="btn btn-sm" :class="{ 'btn-primary': favOnly }" @click="onFavOnlyChange">
      {{ favOnly ? '★ 仅收藏' : '☆ 仅收藏' }}
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
      v-for="image in images"
      :key="image.id"
      class="image-card"
      :class="{ selected: selectedIds.has(image.id) }"
      @click="openLightbox(image.id)"
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
      <div class="image-title" :title="image.prompt_id ? (image.prompt_title || '未命名提示词') : '手动提交'">
        {{ image.prompt_id ? (image.prompt_title || '未命名提示词') : '手动提交' }}
      </div>
    </div>
  </div>

  <div v-if="total > 0" class="pagination-footer">
    <Pagination
      :total="total"
      :page="page"
      :page-size="pageSize"
      @update:page="onPageChange"
    />
    <div class="page-size-wrap">
      <span class="text-xs muted">每页</span>
      <select v-model.number="pageSize" class="input page-size-select" @change="onPageSizeChange">
        <option :value="10">10</option>
        <option :value="20">20</option>
        <option :value="50">50</option>
        <option :value="100">100</option>
      </select>
      <span class="text-xs muted">条</span>
    </div>
  </div>

  <!-- Lightbox -->
  <Teleport to="body">
    <div v-if="lightbox" class="lightbox-overlay" @click.self="closeLightbox" @keydown="handleKeydown" tabindex="0">
      <div class="lightbox-dialog">
        <!-- Close button -->
        <button class="lightbox-close" @click="closeLightbox">&times;</button>

        <div class="lightbox-body">
          <!-- Left: Image -->
          <div class="lightbox-image-wrap">
            <img class="lightbox-image" :src="`/api/images/${lightbox.id}/file`" :alt="`Image #${lightbox.id}`" />
          </div>

          <!-- Right: Details -->
          <div class="lightbox-sidebar">
            <div class="lightbox-sidebar-header">
              <span class="lightbox-id">#{{ lightbox.id }}</span>
              <span class="lightbox-date" v-if="lightbox.created_at">{{ new Date(lightbox.created_at).toLocaleString('zh-CN') }}</span>
            </div>

            <div v-if="lightboxLoading" class="lightbox-loading">
              <span class="spinner"></span>
              <span class="muted">加载中...</span>
            </div>

            <template v-else>
              <!-- 来源提示词：有则可跳转，无则是手动提交 -->
              <div v-if="lightbox.prompt_id" class="lightbox-section">
                <div class="lightbox-section-label">
                  <span class="label-dot source"></span>来源提示词
                </div>
                <div class="lightbox-prompt">{{ lightbox.prompt_title || '未命名提示词' }}</div>
                <button class="btn btn-sm source-link" @click="goToPrompt(lightbox!.prompt_id)">
                  查看提示词 #{{ lightbox.prompt_id }} &rarr;
                </button>
              </div>
              <div v-else class="lightbox-section">
                <div class="lightbox-section-label">
                  <span class="label-dot source"></span>来源提示词
                </div>
                <div class="lightbox-empty">手动提交（未关联提示词）</div>
              </div>

              <div v-if="lightbox.positive_prompt" class="lightbox-section">
                <div class="lightbox-section-label">
                  <span class="label-dot positive"></span>正向提示词
                </div>
                <div class="lightbox-prompt">{{ lightbox.positive_prompt }}</div>
              </div>

              <div v-if="lightbox.negative_prompt" class="lightbox-section">
                <div class="lightbox-section-label">
                  <span class="label-dot negative"></span>负向提示词
                </div>
                <div class="lightbox-prompt">{{ lightbox.negative_prompt }}</div>
              </div>

              <div v-if="!lightbox.positive_prompt && !lightbox.negative_prompt" class="lightbox-empty">
                暂无提示词信息
              </div>
            </template>

            <div class="lightbox-footer">
              <button class="btn btn-primary btn-sm" @click="downloadImage(lightbox!.id)">
                <span class="dl-icon">&#8615;</span> 下载原图
              </button>
              <button class="btn btn-sm fav-btn" :class="{ 'fav-active': lightbox?.is_favorite }" @click="toggleFavLightbox" :disabled="lightboxLoading">
                {{ lightbox?.is_favorite ? '★ 已收藏' : '☆ 收藏' }}
              </button>
              <button class="btn btn-danger btn-sm" @click="deleteImage()">删除</button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
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

/* 来源提示词区块 */
.label-dot.source { background: var(--c-primary, #6366f1); }
.source-link { margin-top: 8px; }
</style>
