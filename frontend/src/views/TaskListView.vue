<script setup lang="ts">
import ViewFeedback from '../components/ViewFeedback.vue'
import { MoreFilled, Picture } from '@element-plus/icons-vue'
import { computed, onMounted, ref, onUnmounted } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api, type Task } from '../api/client'
import { openImageViewer, type ViewerRef } from '../utils/imageViewer'
import { thumbUrl } from '../utils/imageUrls'
import { useUrlState } from '../composables/useUrlState'

type PageResult = { items: Task[]; total: number; page: number; page_size: number }
type BatchResult = { applied: number; skipped: number }

const failedCovers = ref(new Set<number>())
let pendingPreview: AbortController | undefined

async function openTaskImage(task: Task) {
  const cover = task.image_id
  if (!cover) return
  pendingPreview?.abort()
  const request = new AbortController()
  pendingPreview = request
  let images: ViewerRef[] = [cover]
  let start = 0
  if ((task.image_count ?? 1) > 1) {
    try {
      const detail = await api<{ items: { images?: { id: number; width?: number; height?: number }[] }[] }>(
        `/api/tasks/${task.id}`, { signal: request.signal },
      )
      const results = detail.items.flatMap(item => item.images ?? [])
      if (results.length) {
        images = results
        start = Math.max(0, results.findIndex(image => image.id === cover))
      }
    } catch {
      // The cover is still available if fetching the other results failed.
    }
  }
  if (!request.signal.aborted) openImageViewer(images, start, { mutated: load })
}

function coverLabel(task: Task) {
  if (task.image_id) return '缩略图不可用'
  if (task.status === 'pending') return '排队中'
  if (task.status === 'running') return '等待出图'
  return '暂无可预览图片'
}

const tasks = ref<Task[]>([])
const loading = ref(true)
const loadError = ref('')
let requestID = 0
const message = ref('')
const messageType = ref<'success' | 'error' | 'info'>('success')
const batchBusy = ref(false)
const total = ref(0)
let timer: ReturnType<typeof setInterval> | null = null

// 分页与筛选进 URL query：从任务详情返回、刷新、分享链接都保持页码与筛选
const state = useUrlState({
  page: 1,
  page_size: 50,
  status: '',
  group: '',
}, {
  onExternalSync: () => {
    selectedIds.value = new Set()
    load()
  },
})

type Group = { group_name: string; group_id: string; count: number }
const groups = ref<Group[]>([])

async function loadGroups() {
  try {
    groups.value = await api<Group[]>('/api/prompts/groups')
  } catch {
    groups.value = []
  }
}

// 多选（仅作用于当前页；翻页/改筛选时清空）
const selectedIds = ref<Set<number>>(new Set())
const allSelected = computed(() => tasks.value.length > 0 && tasks.value.every(t => selectedIds.value.has(t.id)))
const selectedCount = computed(() => selectedIds.value.size)
const selectedTasks = computed(() => tasks.value.filter(t => selectedIds.value.has(t.id)))
const canBatchCancel = computed(() => selectedTasks.value.some(t => canCancel(t.status)))
const canBatchRetry = computed(() => selectedTasks.value.some(canRetry))
const canBatchDelete = computed(() => selectedTasks.value.some(t => canDelete(t.status)))

async function load() {
  const id = ++requestID
  loading.value = true
  loadError.value = ''
  try {
    const params = new URLSearchParams()
    if (state.status) params.set('status', state.status)
    if (state.group) params.set('group', state.group)
    params.set('page', String(state.page))
    params.set('page_size', String(state.page_size))
    const result = await api<PageResult>(`/api/tasks?${params}`)
    if (id !== requestID) return
    tasks.value = result.items
    total.value = result.total
    // 轮询刷新时，清掉已不在当前列表中的选中项，避免对已消失的任务误操作
    const visible = new Set(tasks.value.map(t => t.id))
    selectedIds.value = new Set([...selectedIds.value].filter(id => visible.has(id)))
  } catch (e) {
    if (id === requestID) loadError.value = e instanceof Error ? e.message : '无法加载任务'
  } finally {
    if (id === requestID) loading.value = false
  }
}

function onFilterChange() {
  state.page = 1
  selectedIds.value = new Set()
  load()
}

function onPageChange(p: number) {
  state.page = p
  selectedIds.value = new Set()
  load()
}

function onPageSizeChange() {
  state.page = 1
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

function isActive(task: Task) {
  return task.status === 'pending' || task.status === 'running'
}

function handleTaskAction(command: string, task: Task) {
  if (command === 'retry') void retryTask(task)
  if (command === 'cancel') void cancelTask(task)
  if (command === 'delete') void deleteTask(task)
}

function sourceLabel(source: string) {
  return source === 'direct' ? '直接提交' : source === 'prompt' ? '提示词库' : '其他来源'
}

function doneCount(task: Task) {
  return Math.min(task.total_count, task.success_count + task.failed_count)
}

function progress(task: Task) {
  return task.total_count > 0 ? Math.round(doneCount(task) * 100 / task.total_count) : 0
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

function canRetry(task: Task) {
  return task.status === 'failed' || (task.status === 'completed' && task.failed_count > 0)
}

function formatTime(t: string) {
  if (!t) return ''
  const d = new Date(t)
  return d.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
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
  if (!confirm(`确定重跑选中的 ${ids.length} 个任务？（只补跑失败的生成项）`)) return
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
    const result = await api<{ remote_warning?: string }>(`/api/tasks/${task.id}/cancel`, { method: 'POST' })
    message.value = result.remote_warning || `任务 #${task.id} 已取消`
    messageType.value = result.remote_warning ? 'info' : 'success'
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
  loadGroups()
  timer = setInterval(load, 5000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  pendingPreview?.abort()
})
</script>

<template>
  <PageHeader title="任务列表" description="查看所有生成任务状态，支持多选批量取消、重跑、删除。" />

  <div class="toolbar" style="flex-wrap:wrap; gap:10px; margin-bottom:16px;">
    <el-select v-model="state.status" class="filter-select" @change="onFilterChange" aria-label="状态" :empty-values="[]">
      <el-option value="" label="全部状态" />
      <el-option value="pending" label="排队中" />
      <el-option value="running" label="生成中" />
      <el-option value="completed" label="已完成" />
      <el-option value="failed" label="失败" />
      <el-option value="cancelled" label="已取消" />
    </el-select>
    <el-select v-model="state.group" class="filter-select filter-select-wide" @change="onFilterChange" aria-label="分组" :empty-values="[]">
      <el-option value="" label="全部分组" />
      <el-option v-for="g in groups" :key="g.group_name" :value="g.group_name" :label="`${g.group_name}（${g.count}）`" />
    </el-select>
    <el-button text @click="load()">&#8635; 刷新</el-button>
  </div>

  <ViewFeedback :kind="messageType" :message="message" />

  <ViewFeedback v-if="loadError && tasks.length" kind="error" :message="loadError">
    <el-button text size="small" @click="load">重试</el-button>
  </ViewFeedback>

  <!-- 多选操作栏 -->
  <div class="batch-bar">
    <el-checkbox class="checkbox-wrap" :model-value="allSelected" @change="toggleSelectAll">全选本页</el-checkbox>
    <span class="batch-info">已选 {{ selectedCount }} 条</span>
    <template v-if="selectedCount > 0">
      <el-button type="warning" text size="small" :disabled="batchBusy || !canBatchCancel" @click="batchCancel">&#10005; 批量取消</el-button>
      <el-button text size="small" :disabled="batchBusy || !canBatchRetry" @click="batchRetry">&#8635; 批量重跑</el-button>
      <el-button type="danger" text size="small" :disabled="batchBusy || !canBatchDelete" @click="batchDelete">&#128465; 批量删除</el-button>
      <el-button text size="small" @click="selectedIds = new Set()">取消选择</el-button>
    </template>
  </div>

  <div class="card">
    <ViewFeedback v-if="loading && !tasks.length" kind="loading" />
    <ViewFeedback v-else-if="loadError && !tasks.length" kind="error" :message="loadError">
      <el-button text size="small" @click="load">重试</el-button>
    </ViewFeedback>
    <ViewFeedback v-else-if="!tasks.length" kind="empty" :message="state.status || state.group ? '没有匹配的任务' : '暂无任务记录'" />
    <div v-else>
      <div class="list-row task-row" :class="{ selected: selectedIds.has(t.id) }" v-for="t in tasks" :key="t.id">
        <el-checkbox class="checkbox-wrap" :model-value="selectedIds.has(t.id)" :aria-label="'选择任务 ' + t.id" @change="toggleSelect(t.id)" />
        <button v-if="t.image_id" class="task-cover" type="button" :aria-label="`查看任务 ${t.id} 的 ${t.image_count ?? 1} 张结果图`" @click="openTaskImage(t)">
          <img v-if="!failedCovers.has(t.image_id)" :src="thumbUrl(t.image_id)" alt="" loading="lazy" decoding="async"
            @error="failedCovers.add(t.image_id)" />
          <span v-else class="task-cover-placeholder"><el-icon><Picture /></el-icon>{{ coverLabel(t) }}</span>
          <span v-if="(t.image_count ?? 0) > 1" class="task-image-count">共 {{ t.image_count }} 张</span>
        </button>
        <div v-else class="task-cover">
          <span class="task-cover-placeholder"><el-icon><Picture /></el-icon>{{ coverLabel(t) }}</span>
        </div>
        <div class="task-main">
          <div class="task-head">
            <RouterLink :to="`/tasks/${t.id}`" class="task-title" :title="t.workflow_name">
              <span class="task-id">#{{ t.id }}</span>
              <span class="task-workflow">{{ t.workflow_name || '任务详情' }}</span>
            </RouterLink>
            <el-dropdown v-if="canRetry(t) || canCancel(t.status) || canDelete(t.status)" trigger="click"
              @command="command => handleTaskAction(command, t)">
              <el-button text size="small" :icon="MoreFilled" :aria-label="`任务 ${t.id} 的操作`" />
              <template #dropdown>
                <el-dropdown-menu>
                  <el-dropdown-item v-if="canRetry(t)" command="retry">重跑失败项</el-dropdown-item>
                  <el-dropdown-item v-if="canCancel(t.status)" command="cancel">取消任务</el-dropdown-item>
                  <el-dropdown-item v-if="canDelete(t.status)" command="delete" class="task-delete">删除任务</el-dropdown-item>
                </el-dropdown-menu>
              </template>
            </el-dropdown>
          </div>
          <RouterLink v-if="t.prompt_id" :to="`/prompts/${t.prompt_id}`" class="task-prompt" :title="t.prompt_title">
            <span class="prompt-title">{{ t.prompt_title || '未命名提示词' }}</span>
            <span v-if="(t.prompt_count || 0) > 1" class="prompt-more">等 {{ t.prompt_count }} 个</span>
            <span aria-hidden="true">›</span>
          </RouterLink>
          <div class="task-result text-xs">
            <el-tag
              size="small"
              :type="t.connection_issue || (t.status === 'completed' && t.failed_count > 0) ? 'warning' : t.status === 'failed' ? 'danger' : t.status === 'completed' ? 'success' : 'info'"
            >{{ t.connection_issue ? '连接异常' : t.status === 'completed' && t.failed_count > 0 ? '部分失败' : statusLabel(t.status) }}</el-tag>
            <span v-if="!isActive(t) && (t.image_count || t.success_count)" class="muted">
              {{ t.image_count ? `产出 ${t.image_count} 张` : `成功 ${t.success_count} 项` }}
            </span>
            <span v-if="t.failed_count" class="task-failure">失败 {{ t.failed_count }} 项</span>
          </div>
          <div v-if="isActive(t) && t.total_count > 0" class="task-progress">
            <div class="text-xs muted">已处理 {{ doneCount(t) }}/{{ t.total_count }}</div>
            <el-progress :aria-label="`任务 ${t.id} 进度`" :percentage="progress(t)" :stroke-width="5" :show-text="false"
              :status="t.failed_count || t.connection_issue ? 'warning' : undefined" />
          </div>
          <p v-if="t.failure_reason || t.connection_issue || t.cancel_unconfirmed" class="task-reason text-xs">
            <strong>{{ t.connection_issue ? '等待连接恢复' : t.cancel_unconfirmed ? '远端取消未确认' : '失败原因' }}</strong>
            <span v-if="t.failure_reason">{{ t.failure_reason }}</span>
          </p>
          <div class="task-meta text-xs muted"><span>{{ sourceLabel(t.source_type) }}</span><time :datetime="t.created_at">{{ formatTime(t.created_at) }}</time></div>
        </div>
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
      <el-select v-model="state.page_size" class="page-size-select" size="small" aria-label="每页条数" @change="onPageSizeChange">
        <el-option :value="20" label="20" />
        <el-option :value="50" label="50" />
        <el-option :value="100" label="100" />
        <el-option :value="200" label="200" />
      </el-select>
      <span class="text-xs muted">条</span>
    </div>
  </div>
</template>

<style scoped>
.task-row { display: grid; grid-template-columns: auto 120px minmax(0, 1fr); align-items: start; gap: 12px; }
.task-row.selected { background: var(--c-primary-light); }
.task-cover { position: relative; display: grid; place-items: center; width: 100%; aspect-ratio: 1; padding: 0; overflow: hidden; border: 1px solid var(--c-border); border-radius: 9px; background: var(--c-bg-subtle); }
button.task-cover { cursor: zoom-in; }
button.task-cover:focus-visible { outline: 2px solid var(--c-primary); outline-offset: 2px; }
.task-cover img { position: absolute; inset: 0; display: block; width: 100%; height: 100%; object-fit: contain; object-position: center; }
.task-cover-placeholder { display: grid; justify-items: center; gap: 4px; padding: 5px; font-size: 11px; color: var(--c-muted); text-align: center; }
.task-cover-placeholder .el-icon { font-size: 19px; }
.task-image-count { position: absolute; right: 4px; bottom: 4px; padding: 1px 5px; border-radius: 4px; background: rgb(0 0 0 / 72%); color: white; font-size: 11px; }
.task-main { display: grid; gap: 4px; min-width: 0; }
.task-head, .task-result, .task-meta { display: flex; align-items: center; gap: 6px; min-width: 0; }
.task-head { justify-content: space-between; }
.task-title { display: flex; align-items: baseline; gap: 6px; min-width: 0; font-weight: 700; color: var(--c-text); text-decoration: none; }
.task-id { flex-shrink: 0; font-size: 12px; color: var(--c-muted); }
.task-workflow, .prompt-title { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.task-title:hover .task-workflow { color: var(--c-primary); }
.task-head .el-button { min-width: 30px; min-height: 30px; padding: 4px; }
.task-prompt { display: flex; align-items: center; gap: 4px; min-width: 0; font-size: 13px; color: var(--c-primary); text-decoration: none; }
.prompt-more { flex-shrink: 0; color: var(--c-muted); }
.task-result, .task-meta { flex-wrap: wrap; }
.task-meta { column-gap: 10px; }
.task-progress { max-width: 440px; }
.task-progress .el-progress { margin-top: 3px; }
.task-failure, :global(.task-delete) { color: var(--c-danger); }
.task-reason { display: flex; flex-wrap: wrap; gap: 1px 6px; margin: 0; color: var(--c-text-secondary); overflow-wrap: anywhere; }
.task-reason strong { color: var(--c-danger); }
.task-reason span { display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; overflow: hidden; }
.checkbox-wrap { flex-shrink: 0; margin-right: 0; }
@media (max-width: 640px) {
  .task-row { grid-template-columns: auto 96px minmax(0, 1fr); gap: 8px; padding: 10px; }
}
.batch-bar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; background: var(--c-primary-light); border: 1px solid var(--c-primary-border); border-radius: 8px; margin-bottom: 8px; flex-wrap: wrap; }
.batch-info { font-size: 13px; font-weight: 600; color: var(--c-primary); margin-right: 4px; }
</style>
