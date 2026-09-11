<script setup lang="ts">
import { onMounted, ref, computed } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api } from '../api/client'

type Prompt = {
  id: number
  title: string
  description: string
  positive_prompt: string
  group_name: string
  group_id: string
  status: string
  completed_at: string
  created_at: string
  run_count: number
  image_count: number
  is_favorite: boolean
}
type Group = { group_name: string; group_id: string; count: number }
type PageResult = { items: Prompt[]; total: number; page: number; page_size: number }
type Workflow = { id: number; name: string; enabled: boolean }

const prompts = ref<Prompt[]>([])
const groups = ref<Group[]>([])
const workflows = ref<Workflow[]>([])
const selectedGroup = ref('')
const statusFilter = ref('')
const search = ref('')
const favOnly = ref(false)
const message = ref('')
const messageType = ref<'success' | 'error'>('success')
const loading = ref(false)

const page = ref(1)
const pageSize = ref(20)
const total = ref(0)

// 多选
const selectedIds = ref<Set<number>>(new Set())
const allSelected = computed(() => prompts.value.length > 0 && prompts.value.every(p => selectedIds.value.has(p.id)))
const selectedCount = computed(() => selectedIds.value.size)

// 批量重跑弹窗
const showBatchRunModal = ref(false)
const batchRunWorkflow = ref(0)
const batchRunMode = ref<'selected' | 'group'>('selected')
const isBatchRunning = ref(false)

async function load() {
  loading.value = true
  try {
    const params = new URLSearchParams()
    if (selectedGroup.value) params.set('group', selectedGroup.value)
    if (statusFilter.value) params.set('status', statusFilter.value)
    if (search.value) params.set('q', search.value)
    if (favOnly.value) params.set('favorite', '1')
    params.set('page', String(page.value))
    params.set('page_size', String(pageSize.value))
    const result = await api<PageResult>(`/api/prompts?${params}`)
    prompts.value = result.items
    total.value = result.total
  } finally {
    loading.value = false
  }
}

async function loadGroups() {
  groups.value = await api<Group[]>('/api/prompts/groups')
}

async function loadWorkflows() {
  const result = await api<{ items: Workflow[] }>('/api/workflows?page_size=200')
  workflows.value = result.items
}

function resetAndLoad() {
  selectedIds.value.clear()
  page.value = 1
  load()
}

function selectGroup(name: string) {
  selectedGroup.value = name
  resetAndLoad()
}

function onPageChange(p: number) {
  page.value = p
  selectedIds.value.clear()
  load()
}

function onPageSizeChange() {
  page.value = 1
  selectedIds.value.clear()
  load()
}

// 多选操作
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
    selectedIds.value = new Set(prompts.value.map(p => p.id))
  }
}

// 单个删除
async function deletePrompt(p: Prompt) {
  if (!confirm(`确定删除提示词「${p.title}」？关联的生成记录会保留。`)) return
  try {
    await api(`/api/prompts/${p.id}`, { method: 'DELETE' })
    message.value = '已删除'
    messageType.value = 'success'
    if (prompts.value.length === 1 && page.value > 1) page.value--
    selectedIds.value.delete(p.id)
    await Promise.all([load(), loadGroups()])
  } catch (e) {
    message.value = e instanceof Error ? e.message : '删除失败'
    messageType.value = 'error'
  }
}

// 批量删除
async function batchDelete() {
  const ids = Array.from(selectedIds.value)
  if (!ids.length) return
  if (!confirm(`确定删除选中的 ${ids.length} 条提示词？关联的生成记录会保留。`)) return
  try {
    const result = await api<{ deleted: number }>('/api/prompts/batch-delete', {
      method: 'POST', body: JSON.stringify({ ids })
    })
    message.value = `已删除 ${result.deleted} 条`
    messageType.value = 'success'
    selectedIds.value.clear()
    if (prompts.value.length <= result.deleted && page.value > 1) page.value--
    await Promise.all([load(), loadGroups()])
  } catch (e) {
    message.value = e instanceof Error ? e.message : '批量删除失败'
    messageType.value = 'error'
  }
}

// 打开批量重跑弹窗
function openBatchRun(mode: 'selected' | 'group') {
  batchRunMode.value = mode
  batchRunWorkflow.value = workflows.value.length ? workflows.value[0].id : 0
  showBatchRunModal.value = true
}

// 确认批量重跑
async function confirmBatchRun() {
  if (!batchRunWorkflow.value) return
  isBatchRunning.value = true
  try {
    if (batchRunMode.value === 'selected') {
      const ids = Array.from(selectedIds.value)
      const result = await api<{ created: number }>('/api/prompts/batch-run', {
        method: 'POST',
        body: JSON.stringify({ ids, workflow_id: batchRunWorkflow.value })
      })
      message.value = `已提交 ${result.created} 个任务`
    } else {
      const result = await api<{ created: number }>('/api/prompts/group-run', {
        method: 'POST',
        body: JSON.stringify({ group_name: selectedGroup.value, workflow_id: batchRunWorkflow.value })
      })
      message.value = `已提交 ${result.created} 个分组任务`
    }
    messageType.value = 'success'
    selectedIds.value.clear()
    showBatchRunModal.value = false
    await load()
  } catch (e) {
    message.value = e instanceof Error ? e.message : '批量重跑失败'
    messageType.value = 'error'
  } finally {
    isBatchRunning.value = false
  }
}

async function toggleFavorite(item: Prompt) {
  try {
    const result = await api<{ is_favorite: boolean }>(`/api/prompts/${item.id}/favorite`, { method: 'PATCH' })
    item.is_favorite = result.is_favorite
  } catch (e) {
    message.value = e instanceof Error ? e.message : '操作失败'
    messageType.value = 'error'
  }
}

function statusBadge(status: string) {
  const map: Record<string, string> = { pending: '待生成', done: '已完成', failed: '失败', running: '生成中' }
  return map[status] || status
}

let searchTimer: any
function onSearch() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(resetAndLoad, 300)
}

onMounted(() => {
  loadGroups()
  loadWorkflows()
  load()
})
</script>

<template>
  <PageHeader eyebrow="PROMPT LIBRARY" title="提示词库" description="统一管理所有提示词，支持分组、搜索、批量操作。" />

  <div class="prompts-layout">
    <!-- 分组侧栏 -->
    <aside class="groups-sidebar card">
      <div class="groups-title">分组</div>
      <div class="groups-list">
        <button
          class="group-item"
          :class="{ active: selectedGroup === '' }"
          @click="selectGroup('')"
        >
          <span>全部提示词</span>
        </button>
        <button
          v-for="g in groups"
          :key="g.group_id"
          class="group-item"
          :class="{ active: selectedGroup === g.group_name }"
          @click="selectGroup(g.group_name)"
        >
          <span class="group-name">{{ g.group_name }}</span>
          <span class="group-count">{{ g.count }}</span>
        </button>
      </div>
      <!-- 分组全部重跑按钮 -->
      <div v-if="selectedGroup" style="padding: 8px 4px 0; flex-shrink: 0;">
        <button class="btn btn-secondary btn-sm" style="width:100%;" @click="openBatchRun('group')">
          &#9654; 重跑此分组待生成项
        </button>
      </div>
    </aside>

    <!-- 列表区 -->
    <div class="prompts-main">
      <div class="toolbar" style="flex-wrap:wrap; gap:10px;">
        <input
          v-model="search"
          class="input"
          type="text"
          placeholder="搜索标题或提示词内容..."
          style="flex:1; min-width:200px;"
          @input="onSearch"
        />
        <select v-model="statusFilter" class="input" style="width:auto;" @change="resetAndLoad">
          <option value="">全部状态</option>
          <option value="pending">待生成</option>
          <option value="running">生成中</option>
          <option value="done">已完成</option>
          <option value="failed">失败</option>
        </select>
        <button class="btn btn-sm" :class="{ 'btn-primary': favOnly }" @click="favOnly = !favOnly; resetAndLoad()">
          {{ favOnly ? '★ 仅收藏' : '☆ 仅收藏' }}
        </button>
        <RouterLink to="/submit" class="btn btn-primary">+ 新建提示词</RouterLink>
      </div>

      <!-- 操作栏 -->
      <div class="batch-bar">
        <label class="checkbox-wrap">
          <input type="checkbox" :checked="allSelected" @change="toggleSelectAll" />
          <span class="text-xs">全选</span>
        </label>
        <span class="batch-info">已选 {{ selectedCount }} 条</span>
        <template v-if="selectedCount > 0">
          <button class="btn btn-primary btn-sm" @click="openBatchRun('selected')">&#9654; 批量重跑</button>
          <button class="btn btn-ghost btn-sm btn-danger" @click="batchDelete">&#10005; 批量删除</button>
          <button class="btn btn-ghost btn-sm" @click="selectedIds = new Set()">取消选择</button>
        </template>
      </div>

      <span v-if="message" class="text-sm" :style="{ color: messageType === 'success' ? 'var(--c-success)' : 'var(--c-danger)', marginBottom: '8px', display:'block' }">{{ message }}</span>

      <div class="card">
        <div v-if="loading" class="empty-state"><div class="empty-icon">&#8987;</div><p>加载中...</p></div>
        <div v-else-if="!prompts.length" class="empty-state">
          <div class="empty-icon">&#128221;</div>
          <p>暂无提示词。可通过「直接提交」或「JSON 文件」导入。</p>
        </div>
        <div v-else>
          <div class="list-row prompt-row" v-for="p in prompts" :key="p.id" :class="{ selected: selectedIds.has(p.id) }">
            <label class="checkbox-wrap" @click.stop>
              <input type="checkbox" :checked="selectedIds.has(p.id)" @change="toggleSelect(p.id)" />
            </label>
            <RouterLink :to="`/prompts/${p.id}`" class="prompt-content" style="min-width:0; flex:1; display:block;">
              <div class="prompt-head">
                <span class="prompt-title">{{ p.title }}</span>
                <span class="badge" :class="'badge-' + p.status">{{ statusBadge(p.status) }}</span>
                <span v-if="p.image_count" class="badge badge-info">&#128247; {{ p.image_count }}</span>
                <span v-if="p.group_name" class="badge badge-muted">{{ p.group_name }}</span>
              </div>
              <div class="prompt-text">{{ p.positive_prompt }}</div>
            </RouterLink>
            <div class="prompt-actions">
              <button class="btn-icon" :class="{ 'fav-active': p.is_favorite }" @click.stop="toggleFavorite(p)" :title="p.is_favorite ? '取消收藏' : '收藏'">
                {{ p.is_favorite ? '★' : '☆' }}
              </button>
              <RouterLink :to="`/prompts/${p.id}`" class="btn btn-ghost btn-sm">查看</RouterLink>
              <RouterLink :to="`/prompts/${p.id}/edit`" class="btn btn-ghost btn-sm">&#9998;</RouterLink>
              <button class="btn btn-ghost btn-sm btn-danger" @click="deletePrompt(p)">&#10005;</button>
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
            <option :value="200">200</option>
          </select>
          <span class="text-xs muted">条</span>
        </div>
      </div>
    </div>
  </div>

  <!-- 批量重跑弹窗 -->
  <div v-if="showBatchRunModal" class="modal-overlay" @click.self="showBatchRunModal = false">
    <div class="modal">
      <h3 style="margin-top:0;">
        {{ batchRunMode === 'group' ? `重跑分组「${selectedGroup}」` : `批量重跑 ${selectedCount} 条` }}
      </h3>
      <label class="form-label">选择工作流</label>
      <select v-model.number="batchRunWorkflow" class="input">
        <option v-for="w in workflows.filter(x => x.enabled)" :key="w.id" :value="w.id">{{ w.name }}</option>
      </select>
      <p v-if="batchRunMode === 'group'" class="text-xs muted" style="margin-top:8px;">
        仅重跑状态为「待生成」和「失败」的条目。
      </p>
      <div style="display:flex; gap:8px; justify-content:flex-end; margin-top:16px;">
        <button class="btn btn-ghost" @click="showBatchRunModal = false">取消</button>
        <button class="btn btn-primary" :disabled="isBatchRunning || !batchRunWorkflow" @click="confirmBatchRun">
          {{ isBatchRunning ? '提交中...' : '开始生成' }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.prompts-layout { display: grid; grid-template-columns: 220px 1fr; gap: 16px; }
.groups-sidebar { padding: 12px; max-height: calc(100vh - 220px); display: flex; flex-direction: column; min-height: 0; }
.groups-title { font-size: 12px; font-weight: 700; text-transform: uppercase; letter-spacing: 0.08em; color: var(--c-muted); padding: 4px 10px; margin-bottom: 6px; flex-shrink: 0; }
.groups-list { overflow-y: auto; flex: 1; min-height: 0; margin: 0 -4px; padding: 0 4px; }
.groups-list .group-item:first-child { margin-top: 0; }
.group-item { display: flex; align-items: center; justify-content: space-between; width: 100%; padding: 8px 10px; border: none; background: none; border-radius: 8px; cursor: pointer; font-size: 14px; text-align: left; color: var(--c-text); }
.group-item:hover { background: var(--c-bg-hover); }
.group-item.active { background: var(--c-primary); color: #fff; }
.group-item.active .group-count { background: rgba(255,255,255,0.25); color: #fff; }
.group-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.group-count { font-size: 12px; background: var(--c-bg-subtle); color: var(--c-muted); padding: 1px 8px; border-radius: 10px; flex-shrink: 0; margin-left: 8px; }
.prompt-row { align-items: flex-start; }
.prompt-row.selected { background: var(--c-primary-light); }
.prompt-content { text-decoration: none; color: inherit; }
.prompt-head { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; flex-wrap: wrap; }
.prompt-title { font-weight: 600; }
.prompt-text { font-size: 13px; color: var(--c-muted); overflow: hidden; text-overflow: ellipsis; display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; line-height: 1.5; }
.prompt-actions { display: flex; gap: 4px; flex-shrink: 0; }
.btn-icon { background: none; border: none; cursor: pointer; font-size: 16px; padding: 4px; color: var(--c-muted); transition: color 0.2s; }
.btn-icon:hover { color: #f59e0b; }
.btn-icon.fav-active { color: #d97706; }
.btn-icon.fav-active:hover { color: #b45309; }
.btn-danger { color: var(--c-danger); }
.badge-muted { background: var(--c-bg-subtle); color: var(--c-muted); }
.badge-info { background: #e0f2fe; color: #0369a1; }
.select-row { padding: 6px 12px; border-bottom: 1px solid var(--c-border-light); display: flex; align-items: center; gap: 8px; }
.checkbox-wrap { display: flex; align-items: center; gap: 6px; cursor: pointer; flex-shrink: 0; }
.checkbox-wrap input[type="checkbox"] { width: 16px; height: 16px; accent-color: var(--c-primary); cursor: pointer; }
.batch-bar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; background: var(--c-primary-light); border: 1px solid var(--c-primary-border); border-radius: 8px; margin-bottom: 8px; }
.batch-info { font-size: 13px; font-weight: 600; color: var(--c-primary); margin-right: 4px; }
.modal-overlay { position: fixed; inset: 0; background: rgba(0,0,0,0.5); display: flex; align-items: center; justify-content: center; z-index: 100; padding: 20px; }
.modal { background: var(--c-card); border-radius: 12px; padding: 24px; width: 100%; max-width: 480px; max-height: 85vh; overflow-y: auto; box-shadow: var(--shadow-lg); }
@media (max-width: 720px) {
  .prompts-layout { grid-template-columns: 1fr; }
  .groups-sidebar { max-height: 200px; }
}
</style>
