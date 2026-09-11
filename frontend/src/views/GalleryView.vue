<script setup lang="ts">
import { onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api } from '../api/client'

type ImageItem = { id: number; filename: string; created_at: string; is_favorite: boolean }
type ImageDetail = { id: number; positive_prompt: string; negative_prompt: string; created_at: string; is_favorite: boolean }
type PageResult = { items: ImageItem[]; total: number; page: number; page_size: number }

const images = ref<ImageItem[]>([])
const loading = ref(true)
const favOnly = ref(false)
const lightbox = ref<ImageDetail | null>(null)
const lightboxLoading = ref(false)

const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

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
  } catch {
    images.value = []
  } finally {
    loading.value = false
  }
}

function onPageChange(p: number) {
  page.value = p
  load()
}

function onPageSizeChange() {
  page.value = 1
  load()
}

async function openLightbox(id: number) {
  lightboxLoading.value = true
  try {
    lightbox.value = await api<ImageDetail>(`/api/images/${id}`)
  } catch {
    lightbox.value = { id, positive_prompt: '无法加载', negative_prompt: '', created_at: '', is_favorite: false }
  } finally {
    lightboxLoading.value = false
  }
}

function closeLightbox() {
  lightbox.value = null
}

function downloadImage(id: number) {
  window.open(`/api/images/${id}/download`, '_blank')
}

async function deleteImage() {
  if (!lightbox.value) return
  const id = lightbox.value.id
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
  <PageHeader eyebrow="IMAGE LIBRARY" title="图片库" description="查看已生成的图片，支持预览、下载和提示词查看。" />

  <div class="toolbar">
    <button class="btn btn-sm" :class="{ 'btn-primary': favOnly }" @click="favOnly = !favOnly; page = 1; load()">
      {{ favOnly ? '★ 仅收藏' : '☆ 仅收藏' }}
    </button>
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
    <div v-for="image in images" :key="image.id" class="image-card" @click="openLightbox(image.id)">
      <span class="fav-badge" @click.stop="toggleFavorite(image)">{{ image.is_favorite ? '★' : '☆' }}</span>
      <img class="image-thumb" :src="`/api/images/${image.id}/file`" :alt="image.filename" loading="lazy" />
      <div class="image-info">
        <span class="text-xs">#{{ image.id }}</span>
        <span class="text-xs">{{ new Date(image.created_at).toLocaleDateString('zh-CN') }}</span>
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
</style>
