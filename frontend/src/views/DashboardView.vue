<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Task } from '../api/client'
import { openImageViewer } from '../utils/imageViewer'

type Stats = { total_tasks: number; total_images: number; pending_entries: number; running_tasks: number }

const router = useRouter()
const tasks = ref<Task[]>([])
const stats = ref<Stats | null>(null)
const comfyUrl = ref('')
const comfyStatus = ref<'checking' | 'online' | 'offline'>('checking')
let timer: ReturnType<typeof setInterval> | null = null

async function load() {
  try {
    const [taskResult, statsData] = await Promise.all([
      api<{ items: Task[] }>('/api/tasks?page_size=10'),
      api<Stats>('/api/stats'),
    ])
    tasks.value = taskResult.items
    stats.value = statsData
  } catch {
    tasks.value = []
  }
}

onMounted(async () => {
  load()
  timer = setInterval(load, 5000)
  // ComfyUI 连通性检测只跑一次，不进轮询
  try {
    const settings = await api<{ comfyui_url: string }>('/api/settings')
    comfyUrl.value = settings.comfyui_url
    try {
      await api(`/api/settings/comfyui/test`, { method: 'POST' })
      comfyStatus.value = 'online'
    } catch {
      comfyStatus.value = 'offline'
    }
  } catch {
    comfyStatus.value = 'offline'
  }
})
onUnmounted(() => { if (timer) clearInterval(timer) })

function goPrompt(task: Task, e: MouseEvent) {
  e.preventDefault()
  e.stopPropagation()
  if (task.prompt_id) router.push(`/prompts/${task.prompt_id}`)
}

function openResult(task: Task, e: MouseEvent) {
  e.preventDefault()
  e.stopPropagation()
  if (task.image_id) openImageViewer([task.image_id], 0)
}

function statusLabel(s: string) {
  return ({ pending: '排队中', running: '生成中', completed: '已完成', failed: '失败', cancelled: '已取消' } as Record<string, string>)[s] || s
}

function statusClass(s: string) {
  if (s === 'running') return 'badge-running'
  if (s === 'completed') return 'badge-completed'
  if (s === 'failed') return 'badge-failed'
  if (s === 'pending') return 'badge-pending'
  if (s === 'cancelled') return 'badge-cancelled'
  return 'badge-pending'
}

function formatTime(t: string) {
  if (!t) return ''
  const d = new Date(t)
  return d.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}
</script>

<template>
  <PageHeader eyebrow="OVERVIEW" title="作图概览" description="查看 ComfyUI 连接状态和最近任务。" />

  <!-- Connection Status -->
  <div class="card mb-4">
    <div class="card-body" style="display:flex; align-items:center; gap:14px; padding:16px 24px;">
      <span class="connection-dot" :class="comfyStatus"></span>
      <div class="flex-1">
        <strong style="font-size:14px;">ComfyUI 连接</strong>
        <div class="text-xs muted" style="margin-top:2px;">{{ comfyUrl || '未配置' }}</div>
      </div>
      <span class="badge" :class="comfyStatus === 'online' ? 'badge-success' : comfyStatus === 'offline' ? 'badge-failed' : 'badge-pending'">
        {{ comfyStatus === 'online' ? '已连接' : comfyStatus === 'offline' ? '未连接' : '检测中...' }}
      </span>
    </div>
  </div>

  <!-- Stats -->
  <div class="stats-grid" v-if="stats">
    <div class="stat-card">
      <div class="stat-label">总任务数</div>
      <div class="stat-value">{{ stats.total_tasks }}</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">生成图片</div>
      <div class="stat-value">{{ stats.total_images }}</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">待生成条目</div>
      <div class="stat-value">{{ stats.pending_entries }}</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">运行中任务</div>
      <div class="stat-value">{{ stats.running_tasks }}</div>
    </div>
  </div>

  <!-- Recent Tasks -->
  <div class="card">
    <div class="card-header">
      <h2>最近任务</h2>
      <RouterLink to="/submit" class="btn btn-primary btn-sm">新建任务</RouterLink>
    </div>
    <div v-if="!tasks.length" class="empty-state">
      <div class="empty-icon">&#128221;</div>
      <p>暂无任务记录，点击上方按钮新建任务。</p>
    </div>
    <div v-else>
      <RouterLink
        v-for="task in tasks.slice(0, 10)"
        :key="task.id"
        :to="`/tasks/${task.id}`"
        class="list-row list-row-clickable recent-task-row"
      >
        <img v-if="task.image_id" class="recent-thumb" :src="`/api/images/${task.image_id}/file`" alt="结果图" loading="lazy" @click="openResult(task, $event)" />
        <div v-else class="recent-thumb recent-thumb-empty">无图</div>
        <div class="flex-1 truncate">
          <div class="recent-line1">
            <strong>#{{ task.id }}</strong>
            <span class="muted text-xs" style="margin-left:6px;">{{ task.source_type === 'direct' ? '直接提交' : 'JSON 批量' }}</span>
          </div>
          <div v-if="task.prompt_id" class="recent-prompt" :title="`提示词 #${task.prompt_id}`" @click.stop.prevent="goPrompt(task, $event)">
            <span class="prompt-title">{{ task.prompt_title || '未命名提示词' }}</span>
            <span v-if="(task.prompt_count || 0) > 1" class="prompt-more">等 {{ task.prompt_count }} 个</span>
            <span class="prompt-link">查看 →</span>
          </div>
        </div>
        <span class="text-xs muted">{{ formatTime(task.created_at) }}</span>
        <span class="badge" :class="statusClass(task.status)">{{ statusLabel(task.status) }}</span>
      </RouterLink>
    </div>
  </div>
</template>

<style scoped>
.recent-task-row { align-items: center; }
.recent-thumb { width: 44px; height: 44px; object-fit: cover; border-radius: 8px; flex-shrink: 0; cursor: zoom-in; border: 1px solid var(--c-border); }
.recent-thumb-empty { display: grid; place-items: center; font-size: 10px; color: var(--c-muted); background: var(--c-bg-subtle); cursor: default; }
.recent-line1 { display: flex; align-items: baseline; min-width: 0; }
.recent-prompt { display: flex; align-items: center; gap: 6px; font-size: 12px; margin-top: 2px; min-width: 0; cursor: pointer; }
.recent-prompt:hover .prompt-link { opacity: 1; }
.prompt-title { color: var(--c-primary); font-weight: 600; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; min-width: 0; }
.prompt-more { flex-shrink: 0; color: var(--c-muted); }
.prompt-link { flex-shrink: 0; color: var(--c-primary); opacity: 0.6; }
</style>
