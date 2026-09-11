<script setup lang="ts">
import { onMounted, ref, onUnmounted } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api } from '../api/client'

type Task = {
  id: number
  source_type: string
  workflow_id: number
  workflow_name: string
  status: string
  total_count: number
  success_count: number
  failed_count: number
  created_at: string
}
type PageResult = { items: Task[]; total: number; page: number; page_size: number }

const tasks = ref<Task[]>([])
const statusFilter = ref('')
const loading = ref(false)
const message = ref('')
const messageType = ref<'success' | 'error'>('success')
let timer: ReturnType<typeof setInterval> | null = null

const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

async function load() {
  loading.value = true
  try {
    const params = new URLSearchParams()
    if (statusFilter.value) params.set('status', statusFilter.value)
    params.set('page', String(page.value))
    params.set('page_size', String(pageSize.value))
    const result = await api<PageResult>(`/api/tasks?${params}`)
    tasks.value = result.items
    total.value = result.total
  } finally {
    loading.value = false
  }
}

function onFilterChange() {
  page.value = 1
  load()
}

function onPageChange(p: number) {
  page.value = p
  load()
}

function onPageSizeChange() {
  page.value = 1
  load()
}

async function cancelTask(task: Task) {
  if (!confirm(`确定取消任务 #${task.id}？`)) return
  try {
    await api(`/api/tasks/${task.id}/cancel`, { method: 'POST' })
    message.value = `任务 #${task.id} 已取消`
    messageType.value = 'success'
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '取消失败'
    messageType.value = 'error'
  }
}

async function deleteTask(task: Task) {
  if (!confirm(`确定删除任务 #${task.id}？此操作不可恢复。`)) return
  try {
    await api(`/api/tasks/${task.id}`, { method: 'DELETE' })
    message.value = `任务 #${task.id} 已删除`
    messageType.value = 'success'
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '删除失败'
    messageType.value = 'error'
  }
}

function statusLabel(s: string) {
  return { pending: '排队中', running: '生成中', completed: '已完成', failed: '失败', cancelled: '已取消' }[s] || s
}

function canCancel(s: string) {
  return s === 'pending' || s === 'running'
}

function canDelete(s: string) {
  return s !== 'running'
}

onMounted(() => {
  load()
  timer = setInterval(load, 5000)
})
onUnmounted(() => { if (timer) clearInterval(timer) })
</script>

<template>
  <PageHeader eyebrow="TASKS" title="任务列表" description="查看所有生成任务状态，支持取消排队中的任务。" />

  <div class="toolbar" style="flex-wrap:wrap; gap:10px; margin-bottom:16px;">
    <select v-model="statusFilter" class="input" style="width:auto;" @change="onFilterChange">
      <option value="">全部状态</option>
      <option value="pending">排队中</option>
      <option value="running">生成中</option>
      <option value="completed">已完成</option>
      <option value="failed">失败</option>
      <option value="cancelled">已取消</option>
    </select>
    <button class="btn btn-ghost" @click="load()">&#8635; 刷新</button>
  </div>

  <span v-if="message" class="text-sm" :style="{ color: messageType === 'success' ? 'var(--c-success)' : 'var(--c-danger)', marginBottom:'8px', display:'block' }">{{ message }}</span>

  <div class="card">
    <div v-if="loading && !tasks.length" class="empty-state"><div class="empty-icon">&#8987;</div><p>加载中...</p></div>
    <div v-else-if="!tasks.length" class="empty-state">
      <div class="empty-icon">&#128640;</div>
      <p>暂无任务记录。</p>
    </div>
    <div v-else>
      <div class="list-row task-row" v-for="t in tasks" :key="t.id">
        <RouterLink :to="`/tasks/${t.id}`" class="task-info" style="min-width:0; flex:1;">
          <div class="task-head">
            <span class="task-id">#{{ t.id }}</span>
            <span class="badge" :class="'badge-' + t.status">{{ statusLabel(t.status) }}</span>
            <span class="badge badge-muted">{{ t.source_type }}</span>
            <span v-if="t.workflow_name" class="badge badge-muted">{{ t.workflow_name }}</span>
          </div>
          <div class="task-meta text-xs muted">
            <span>{{ t.created_at }}</span>
            <span v-if="t.total_count > 0"> · {{ t.success_count }}/{{ t.total_count }} 成功</span>
            <span v-if="t.failed_count > 0" style="color:var(--c-danger);"> · {{ t.failed_count }} 失败</span>
          </div>
        </RouterLink>
        <div class="task-actions">
          <RouterLink :to="`/tasks/${t.id}`" class="btn btn-ghost btn-sm">详情</RouterLink>
          <button v-if="canCancel(t.status)" class="btn btn-ghost btn-sm btn-warn" @click="cancelTask(t)">取消</button>
          <button v-if="canDelete(t.status)" class="btn btn-ghost btn-sm btn-danger" @click="deleteTask(t)">&#10005;</button>
        </div>
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
</template>

<style scoped>
.task-row { align-items: flex-start; }
.task-info { text-decoration: none; color: inherit; display: block; }
.task-head { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; flex-wrap: wrap; }
.task-id { font-weight: 700; font-size: 14px; color: var(--c-primary); }
.task-meta { line-height: 1.5; }
.task-actions { display: flex; gap: 4px; flex-shrink: 0; }
.badge-muted { background: var(--c-bg-subtle); color: var(--c-muted); }
.badge-pending { background: #fef3c7; color: #92400e; }
.badge-running { background: #dbeafe; color: #1e40af; }
.badge-completed { background: #dcfce7; color: #166534; }
.badge-failed { background: #fee2e2; color: #991b1b; }
.badge-cancelled { background: #f3f4f6; color: #6b7280; }
.btn-warn { color: #d97706; }
.btn-danger { color: var(--c-danger); }
</style>
