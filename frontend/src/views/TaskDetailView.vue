<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Task } from '../api/client'

type TaskItem = { id: number; status: string; positive_prompt: string; negative_prompt: string; error_message: string }

const route = useRoute()
const router = useRouter()
const task = ref<Task | null>(null)
const items = ref<TaskItem[]>([])
const message = ref('')
const messageType = ref<'success' | 'error' | 'info'>('info')
let stream: EventSource | undefined
let retryCount = 0
const maxRetries = 5

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
    failed: 'badge-failed',
    pending: 'badge-pending',
    cancelled: 'badge-cancelled',
  }
  return map[s] || 'badge-pending'
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
  <PageHeader eyebrow="TASK DETAIL" :title="`任务 #${route.params.id}`" description="查看任务进度和详情。" />

  <div v-if="task" class="card">
    <div class="card-header">
      <div class="flex items-center gap-3">
        <span class="badge" :class="statusClass(task.status)">{{ task.status }}</span>
        <span class="text-sm muted">{{ task.source_type === 'direct' ? '直接提交' : 'JSON 批量' }}</span>
      </div>
      <div class="btn-group">
        <button v-if="task.status === 'pending' || task.status === 'running'" class="btn btn-secondary btn-sm" @click="action(`/api/tasks/${task.id}/cancel`)">取消任务</button>
        <button v-if="task.status === 'failed'" class="btn btn-primary btn-sm" @click="action(`/api/tasks/${task.id}/retry`)">重试任务</button>
        <button class="btn btn-ghost btn-sm" @click="load()">刷新</button>
      </div>
    </div>

    <div class="card-body">
      <!-- Progress -->
      <div v-if="task.total_count > 0" class="mb-4">
        <div class="flex justify-between text-sm mb-2">
          <span>进度</span>
          <span>{{ task.success_count + task.failed_count }} / {{ task.total_count }}</span>
        </div>
        <div class="progress-bar">
          <div class="progress-bar-fill" :class="task.failed_count > 0 ? 'danger' : 'success'" :style="{ width: progressPercent() + '%' }"></div>
        </div>
      </div>

      <!-- Stats -->
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

      <!-- Parameters -->
      <div v-if="task.parameters" class="mt-4">
        <h3 class="text-sm mb-2" style="font-weight:600;">任务参数</h3>
        <div class="json-display">{{ JSON.stringify(task.parameters, null, 2) }}</div>
      </div>

      <div v-if="items.length" class="mt-4">
        <h3 class="text-sm mb-2" style="font-weight:600;">生成项目</h3>
        <div v-for="item in items" :key="item.id" class="task-item-row">
          <span class="badge" :class="statusClass(item.status)">{{ item.status }}</span>
          <div class="flex-1" style="min-width:0;">
            <div class="prompt-block">{{ item.positive_prompt }}</div>
            <div v-if="item.error_message" class="text-xs" style="color:var(--c-danger);margin-top:4px;">{{ item.error_message }}</div>
          </div>
        </div>
      </div>
    </div>

    <div v-if="message" class="card-footer">
      <div class="status-msg" :class="messageType">{{ message }}</div>
    </div>
  </div>

  <div v-else class="card">
    <div class="card-body">
      <div class="flex items-center gap-3">
        <span class="spinner"></span>
        <span class="muted">正在加载任务...</span>
      </div>
    </div>
  </div>
</template>
