<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Task } from '../api/client'
import { openImageViewer } from '../utils/imageViewer'
import { backToListOr } from '../utils/nav'

// 返回时优先走历史（保留任务列表的分页/筛选 query），直链进入则回列表第一页
function backToList() {
  backToListOr(router, '/tasks')
}

type TaskItem = { id: number; status: string; positive_prompt: string; negative_prompt: string; error_message: string; images?: { id: number; filename: string }[] }

const route = useRoute()
const router = useRouter()
const task = ref<Task | null>(null)
const items = ref<TaskItem[]>([])
const message = ref('')
const messageType = ref<'success' | 'error' | 'info'>('info')
let stream: EventSource | undefined
let retryCount = 0
const maxRetries = 5

// 任务全部结果图摊平：点开某张图后可跨生成项前后翻页浏览
const allImageIds = computed(() => items.value.flatMap(i => (i.images || []).map(img => img.id)))

function openItemImage(item: TaskItem, index: number) {
  const ids = (item.images || []).map(i => i.id)
  if (!ids.length) return
  const flat = allImageIds.value
  const globalIndex = Math.max(0, flat.indexOf(ids[index]))
  openImageViewer(flat, globalIndex, { mutated: () => load() })
}

async function load() {
  try {
    const data = await api<Task & { items?: TaskItem[] }>(`/api/tasks/${route.params.id}`)
    task.value = data
    items.value = data.items || []
  } catch {
    message.value = '加载任务失败'
    messageType.value = 'error'
  }
}

async function action(path: string) {
  try {
    await api(path, { method: 'POST' })
    await load()
    message.value = '操作已提交'
    messageType.value = 'success'
  } catch (e) {
    message.value = e instanceof Error ? e.message : '操作失败'
    messageType.value = 'error'
  }
}

function statusClass(s: string) {
  const map: Record<string, string> = {
    running: 'badge-running',
    completed: 'badge-completed',
    success: 'badge-completed',
    failed: 'badge-failed',
    pending: 'badge-pending',
    submitted: 'badge-running',
    cancelled: 'badge-cancelled',
  }
  return map[s] || 'badge-pending'
}

function statusLabel(s: string) {
  const map: Record<string, string> = {
    pending: '排队中', running: '生成中', submitted: '已提交', completed: '已完成',
    success: '成功', failed: '失败', cancelled: '已取消',
  }
  return map[s] || s
}

function formatTime(t: string) {
  if (!t) return ''
  return new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function progressPercent() {
  if (!task.value || task.value.total_count === 0) return 0
  return Math.round(((task.value.success_count + task.value.failed_count) / task.value.total_count) * 100)
}

function connectSSE() {
  stream = new EventSource(`/api/tasks/${route.params.id}/events`)
  stream.onmessage = async () => {
    retryCount = 0
    await load()
  }
  stream.onerror = () => {
    stream?.close()
    if (retryCount < maxRetries) {
      retryCount++
      const delay = Math.min(1000 * Math.pow(2, retryCount), 10000)
      message.value = `连接中断，${delay / 1000}秒后重连...`
      messageType.value = 'info'
      setTimeout(() => connectSSE(), delay)
    } else {
      message.value = '实时连接已断开，请手动刷新页面'
      messageType.value = 'error'
    }
  }
}

onMounted(async () => {
  await load()
  connectSSE()
})

onUnmounted(() => stream?.close())
</script>

<template>
  <PageHeader eyebrow="TASK DETAIL" :title="`任务 #${route.params.id}`" description="重点查看每个生成项的提示词与结果图。" />

  <button class="btn btn-ghost btn-sm" style="margin-bottom:12px;" @click="backToList">&larr; 返回任务列表</button>

  <div v-if="message" class="status-msg-inline" :class="messageType">{{ message }}</div>

  <!-- 紧凑状态条 -->
  <div v-if="task" class="status-bar">
    <span class="badge" :class="statusClass(task.status)">{{ statusLabel(task.status) }}</span>
    <span class="text-sm muted">{{ task.source_type === 'direct' ? '直接提交' : '提示词库' }}</span>
    <span v-if="task.total_count > 0" class="text-sm muted">
      进度 {{ task.success_count + task.failed_count }}/{{ task.total_count }}
      <template v-if="task.success_count"> · 成功 {{ task.success_count }}</template>
      <template v-if="task.failed_count"> · <span style="color:var(--c-danger);">失败 {{ task.failed_count }}</span></template>
    </span>
    <span class="text-sm muted">{{ formatTime(task.created_at) }}</span>
    <div class="btn-group" style="margin-left:auto;">
      <button v-if="task.status === 'pending' || task.status === 'running'" class="btn btn-secondary btn-sm" @click="action(`/api/tasks/${task.id}/cancel`)">取消任务</button>
      <button v-if="task.status === 'failed'" class="btn btn-primary btn-sm" @click="action(`/api/tasks/${task.id}/retry`)">重试任务</button>
      <button class="btn btn-ghost btn-sm" @click="load()">刷新</button>
    </div>
  </div>

  <!-- 生成项：提示词 + 结果图 为主 -->
  <div v-if="items.length" class="item-list">
    <div v-for="item in items" :key="item.id" class="card item-card">
      <div class="ic-head">
        <span class="badge" :class="statusClass(item.status)">{{ statusLabel(item.status) }}</span>
        <span class="text-xs muted">生成项 #{{ item.id }}</span>
      </div>
      <div class="ic-body">
        <div class="ic-prompt">{{ item.positive_prompt || '（无提示词）' }}</div>
        <div v-if="item.images?.length" class="ic-images">
          <img
            v-for="(img, i) in item.images"
            :key="img.id"
            class="ic-thumb"
            :src="`/api/images/${img.id}/file`"
            :alt="img.filename"
            loading="lazy"
            @click="openItemImage(item, i)"
          />
        </div>
        <div v-else-if="item.status === 'success' || item.status === 'completed'" class="ic-noimg text-xs">无结果图</div>
      </div>
      <div v-if="item.error_message" class="ic-error">{{ item.error_message }}</div>
    </div>
  </div>

  <!-- 次要信息：统计与参数，默认折叠 -->
  <details v-if="task" class="meta-details card">
    <summary>任务信息（进度 / 统计 / 参数）</summary>
    <div class="meta-body">
      <div v-if="task.total_count > 0" class="mb-4">
        <div class="progress-bar">
          <div class="progress-bar-fill" :class="task.failed_count > 0 ? 'danger' : 'success'" :style="{ width: progressPercent() + '%' }"></div>
        </div>
      </div>
      <div class="stats-grid">
        <div class="stat-card">
          <div class="stat-label">总条目</div>
          <div class="stat-value">{{ task.total_count }}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">成功</div>
          <div class="stat-value" style="color:var(--c-success);">{{ task.success_count }}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">失败</div>
          <div class="stat-value" style="color:var(--c-danger);">{{ task.failed_count }}</div>
        </div>
      </div>
      <div v-if="task.parameters" class="mt-4">
        <h3 class="text-sm mb-2" style="font-weight:600;">任务参数</h3>
        <div class="json-display">{{ JSON.stringify(task.parameters, null, 2) }}</div>
      </div>
      <div class="text-xs muted" style="margin-top:10px;">ComfyUI 地址：{{ task.comfyui_url || '-' }}</div>
    </div>
  </details>

  <div v-if="!task" class="card">
    <div class="card-body">
      <div class="flex items-center gap-3">
        <span class="spinner"></span>
        <span class="muted">正在加载任务...</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.status-msg-inline { font-size: 13px; margin-bottom: 10px; }
.status-bar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; padding: 10px 14px; background: var(--c-surface); border: 1px solid var(--c-border); border-radius: 10px; margin-bottom: 14px; }
.item-list { display: flex; flex-direction: column; gap: 12px; margin-bottom: 14px; }
.item-card { padding: 14px 16px; }
.ic-head { display: flex; align-items: center; gap: 8px; margin-bottom: 10px; }
.ic-body { display: flex; gap: 14px; align-items: flex-start; }
.ic-prompt { flex: 1; min-width: 0; font-size: 14px; line-height: 1.7; white-space: pre-wrap; word-break: break-word; }
.ic-images { display: flex; gap: 8px; flex-wrap: wrap; flex-shrink: 0; max-width: 46%; }
.ic-thumb { width: 128px; height: 128px; object-fit: cover; border-radius: 10px; border: 1px solid var(--c-border); cursor: zoom-in; background: var(--c-bg-subtle); transition: border-color .15s; }
.ic-thumb:hover { border-color: var(--c-primary); }
.ic-noimg { align-self: center; color: var(--c-muted); border: 1px dashed var(--c-border); border-radius: 10px; padding: 18px 14px; }
.ic-error { margin-top: 8px; font-size: 12px; color: var(--c-danger); word-break: break-all; }
.meta-details { padding: 0; }
.meta-details summary { cursor: pointer; padding: 12px 16px; font-size: 13px; font-weight: 600; color: var(--c-muted); user-select: none; }
.meta-body { padding: 0 16px 16px; }
@media (max-width: 720px) {
  .ic-body { flex-direction: column; }
  .ic-images { max-width: 100%; }
}
</style>
