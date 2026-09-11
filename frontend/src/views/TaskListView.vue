<script setup lang="ts">
import { computed, onMounted, ref, onUnmounted } from 'vue'
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
type BatchResult = { applied: number; skipped: number }

const tasks = ref<Task[]>([])
const statusFilter = ref('')
const loading = ref(false)
const message = ref('')
const messageType = ref<'success' | 'error'>('success')
const batchBusy = ref(false)
let timer: ReturnType<typeof setInterval> | null = null

const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

// 多选（仅作用于当前页；翻页/改筛选时清空）
const selectedIds = ref<Set<number>>(new Set())
const allSelected = computed(() => tasks.value.length > 0 && tasks.value.every(t => selectedIds.value.has(t.id)))
const selectedCount = computed(() => selectedIds.value.size)
const selectedTasks = computed(() => tasks.value.filter(t => selectedIds.value.has(t.id)))
const canBatchCancel = computed(() => selectedTasks.value.some(t => canCancel(t.status)))
const canBatchRetry = computed(() => selectedTasks.value.some(t => t.status === 'failed'))
const canBatchDelete = computed(() => selectedTasks.value.some(t => canDelete(t.status)))

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
    // 轮询刷新时，清掉已不在当前列表中的选中项，避免对已消失的任务误操作
    const visible = new Set(tasks.value.map(t => t.id))
    selectedIds.value = new Set([...selectedIds.value].filter(id => visible.has(id)))
  } finally {
    loading.value = false
  }
}

function onFilterChange() {
  page.value = 1
  selectedIds.value = new Set()
  load()
}

function onPageChange(p: number) {
  page.value = p
  selectedIds.value = new Set()
  load()
}

function onPageSizeChange() {
  page.value = 1
  selectedIds.value = new Set()
  load()
}

function toggleSelect(id: number) {
  const s = new Set(selectedIds.value)
  if (s.has(id)) s.delete(id)
  else s.add(id)
  selectedIds.value = s
}

function toggleSelectAll() {
  if (allSelected.value) {
    selectedIds.value = new Set()
  } else {
    selectedIds.value = new Set(tasks.value.map(t => t.id))
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

function canRetry(s: string) {
  return s === 'failed'
}

// 批量操作：后端逐条执行并返回 applied/skipped，skipped 为状态不允许或不存在的数量
async function runBatch(path: string, ids: number[], label: string) {
  batchBusy.value = true
  try {
    const result = await api<BatchResult>(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ids }),
    })
    message.value = result.skipped > 0
      ? `${label} ${result.applied} 个，跳过 ${result.skipped} 个（状态不允许）`
      : `已${label} ${result.applied} 个任务`
    messageType.value = 'success'
    selectedIds.value = new Set()
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '批量操作失败'
    messageType.value = 'error'
  } finally {
    batchBusy.value = false
  }
}

async function batchCancel() {
  const ids = Array.from(selectedIds.value)
  if (!ids.length) return
  if (!confirm(`确定取消选中的 ${ids.length} 个任务？（仅排队中/生成中的会生效）`)) return
  await runBatch('/api/tasks/batch-cancel', ids, '取消')
}

async function batchRetry() {
  const ids = Array.from(selectedIds.value)
  if (!ids.length) return
  if (!confirm(`确定重跑选中的 ${ids.length} 个任务？（仅失败任务会生效）`)) return
  await runBatch('/api/tasks/batch-retry', ids, '重跑')
}

async function batchDelete() {
  const ids = Array.from(selectedIds.value)
  if (!ids.length) return
  if (!confirm(`确定删除选中的 ${ids.length} 个任务？此操作不可恢复。（生成中的会跳过）`)) return
  await runBatch('/api/tasks/batch-delete', ids, '删除')
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

async function retryTask(task: Task) {
  try {
    await api(`/api/tasks/${task.id}/retry`, { method: 'POST' })
    message.value = `任务 #${task.id} 已重新排队`
    messageType.value = 'success'
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '重跑失败'
    messageType.value = 'error'
  }
}

async function deleteTask(task: Task) {
  if (!confirm(`确定删除任务 #${task.id}？此操作不可恢复。`)) return
  try {
    await api(`/api/tasks/${task.id}`, { method: 'DELETE' })
    message.value = `任务 #${task.id} 已删除`
    messageType.value = 'success'
    selectedIds.value.delete(task.id)
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '删除失败'
    messageType.value = 'error'
  }
}

onMounted(() => {
  load()
  timer = setInterval(load, 5000)
})
onUnmounted(() => { if (timer) clearInterval(timer) })
</script>

<template>
  <PageHeader eyebrow="TASKS" title="任务列表" description="查看所有生成任务状态，支持多选批量取消、重跑、删除。" />

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

  <!-- 多选操作栏 -->
  <div class="batch-bar">
    <label class="checkbox-wrap">
      <input type="checkbox" :checked="allSelected" @change="toggleSelectAll" />
      <span class="text-xs">全选本页</span>
    </label>
    <span class="batch-info">已选 {{ selectedCount }} 条</span>
    <template v-if="selectedCount > 0">
      <button class="btn btn-ghost btn-sm btn-warn" :disabled="batchBusy || !canBatchCancel" @click="batchCancel">&#10005; 批量取消</button>
      <button class="btn btn-ghost btn-sm" :disabled="batchBusy || !canBatchRetry" @click="batchRetry">&#8635; 批量重跑</button>
      <button class="btn btn-ghost btn-sm btn-danger" :disabled="batchBusy || !canBatchDelete" @click="batchDelete">&#128465; 批量删除</button>
      <button class="btn btn-ghost btn-sm" @click="selectedIds = new Set()">取消选择</button>
    </template>
  </div>

  <div class="card">
    <div v-if="loading && !tasks.length" class="empty-state"><div class="empty-icon">&#8987;</div><p>加载中...</p></div>
    <div v-else-if="!tasks.length" class="empty-state">
      <div class="empty-icon">&#128640;</div>
      <p>暂无任务记录。</p>
    </div>
    <div v-else>
      <div class="list-row task-row" :class="{ selected: selectedIds.has(t.id) }" v-for="t in tasks" :key="t.id">
        <label class="checkbox-wrap" @click.stop>
          <input type="checkbox" :checked="selectedIds.has(t.id)" @change="toggleSelect(t.id)" />
        </label>
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
          <button v-if="canRetry(t.status)" class="btn btn-ghost btn-sm" @click="retryTask(t)">重跑</button>
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
.task-row.selected { background: var(--c-primary-light); }
.task-info { text-decoration: none; color: inherit; display: block; }
.task-head { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; flex-wrap: wrap; }
.task-id { font-weight: 700; font-size: 14px; color: var(--c-primary); }
.task-meta { line-height: 1.5; }
.task-actions { display: flex; gap: 4px; flex-shrink: 0; }
.checkbox-wrap { display: flex; align-items: center; gap: 6px; cursor: pointer; flex-shrink: 0; padding-top: 4px; }
.checkbox-wrap input[type="checkbox"] { width: 16px; height: 16px; accent-color: var(--c-primary); cursor: pointer; }
.batch-bar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; background: var(--c-primary-light); border: 1px solid var(--c-primary-border); border-radius: 8px; margin-bottom: 8px; flex-wrap: wrap; }
.batch-info { font-size: 13px; font-weight: 600; color: var(--c-primary); margin-right: 4px; }
.badge-muted { background: var(--c-bg-subtle); color: var(--c-muted); }
.badge-pending { background: #fef3c7; color: #92400e; }
.badge-running { background: #dbeafe; color: #1e40af; }
.badge-completed { background: #dcfce7; color: #166534; }
.badge-failed { background: #fee2e2; color: #991b1b; }
.badge-cancelled { background: #f3f4f6; color: #6b7280; }
.btn-warn { color: #d97706; }
.btn-danger { color: var(--c-danger); }
.btn:disabled { opacity: 0.45; cursor: not-allowed; }
</style>
