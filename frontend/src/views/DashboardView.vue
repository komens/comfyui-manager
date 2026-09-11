<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Task } from '../api/client'

type Stats = { total_tasks: number; total_images: number; pending_entries: number; running_tasks: number }

const tasks = ref<Task[]>([])
const stats = ref<Stats | null>(null)
const comfyUrl = ref('')
const comfyStatus = ref<'checking' | 'online' | 'offline'>('checking')

onMounted(async () => {
  try {
    const [taskResult, statsData, settings] = await Promise.all([
      api<{ items: Task[] }>('/api/tasks?page_size=10'),
      api<Stats>('/api/stats'),
      api<{ comfyui_url: string }>('/api/settings'),
    ])
    tasks.value = taskResult.items
    stats.value = statsData
    comfyUrl.value = settings.comfyui_url
    try {
      await api(`/api/settings/comfyui/test`, { method: 'POST' })
      comfyStatus.value = 'online'
    } catch {
      comfyStatus.value = 'offline'
    }
  } catch {
    tasks.value = []
  }
})

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
        class="list-row list-row-clickable"
      >
        <div class="flex-1 truncate">
          <strong>#{{ task.id }}</strong>
          <span class="muted text-sm" style="margin-left:8px;">{{ task.source_type === 'direct' ? '直接提交' : 'JSON 批量' }}</span>
        </div>
        <span class="text-xs muted">{{ formatTime(task.created_at) }}</span>
        <span class="badge" :class="statusClass(task.status)">{{ task.status }}</span>
      </RouterLink>
    </div>
  </div>
</template>
