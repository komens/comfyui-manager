<script setup lang="ts">
import FilterToolbar from '../components/FilterToolbar.vue'
import ViewFeedback from '../components/ViewFeedback.vue'
import { Delete } from '@element-plus/icons-vue'
import { onBeforeUnmount, onMounted, ref, computed } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import PlatformBadge from '../components/PlatformBadge.vue'
import { api, type Workflow } from '../api/client'
import { fetchWorkflows, pickWorkflowId, rememberWorkflowId } from '../utils/workflowParams'
import { openImageViewer } from '../utils/imageViewer'
import { thumbUrl } from '../utils/imageUrls'
import { useUrlState } from '../composables/useUrlState'
import { ensurePlatforms, platforms, type Platform } from '../utils/platforms'

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
  cover_image_id: number
  is_favorite: boolean
  source_platform: string
}
type Group = { group_name: string; group_id: string; count: number }
type PageResult = { items: Prompt[]; total: number; page: number; page_size: number }
type Workflow = { id: number; name: string; enabled: boolean }

const prompts = ref<Prompt[]>([])
const groups = ref<Group[]>([])
const workflows = ref<Workflow[]>([])
const message = ref('')
const messageType = ref<'success' | 'error'>('success')
const loading = ref(false)
const total = ref(0)

// 分页与筛选统一进 URL query：从详情页返回、刷新、分享链接都保持当前页码与筛选条件
const state = useUrlState({
  page: 1,
  page_size: 50,
  group: '',
  status: '',
  q: '',
  favorite: false,
  platform: '',
}, {
  onExternalSync: () => {
    // 侧边栏重新点击进入等外部导航：state 已按 URL 重置，清掉勾选并重新加载
    selectedIds.value.clear()
    load()
  },
})

// 来源下拉：本平台 + 各外部平台（清单来自后端）
const platformOptions = ref<Platform[]>([])
const activeFilters = computed(() => [
  state.q.trim() && `搜索 · ${state.q.trim()}`,
  state.group && `分组 · ${state.group}`,
  state.status && `状态 · ${statusBadge(state.status)}`,
  state.platform && `来源 · ${state.platform === 'local' ? '本平台' : platformOptions.value.find(p => p.id === state.platform)?.label || state.platform}`,
  state.favorite && '仅收藏',
].filter((value): value is string => !!value))
const loadError = ref('')
let listRequest: AbortController | undefined

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
  listRequest?.abort()
  const request = new AbortController()
  listRequest = request
  loading.value = true
  loadError.value = ''
  try {
    const params = new URLSearchParams()
    if (state.group) params.set('group', state.group)
    if (state.status) params.set('status', state.status)
    if (state.q) params.set('q', state.q)
    if (state.favorite) params.set('favorite', '1')
    if (state.platform) params.set('platform', state.platform)
    params.set('page', String(state.page))
    params.set('page_size', String(state.page_size))
    const result = await api<PageResult>(`/api/prompts?${params}`, { signal: request.signal })
    if (request.signal.aborted) return
    prompts.value = result.items
    total.value = result.total
  } catch (e) {
    if (!request.signal.aborted) loadError.value = e instanceof Error ? e.message : '无法加载提示词'
  } finally {
    if (!request.signal.aborted) loading.value = false
  }
}

async function loadGroups() {
  groups.value = await api<Group[]>('/api/prompts/groups')
}

async function loadWorkflows() {
  workflows.value = await fetchWorkflows()
}

function resetAndLoad() {
  clearTimeout(searchTimer)
  selectedIds.value.clear()
  state.page = 1
  load()
}

function clearFilters() {
  Object.assign(state, { group: '', q: '', status: '', platform: '', favorite: false })
  resetAndLoad()
}

function selectGroup(name: string) {
  state.group = name
  resetAndLoad()
}

function onPageChange(p: number) {
  state.page = p
  selectedIds.value.clear()
  load()
}

function onPageSizeChange() {
  state.page = 1
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
    if (prompts.value.length === 1 && state.page > 1) state.page--
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
    if (prompts.value.length <= result.deleted && state.page > 1) state.page--
    await Promise.all([load(), loadGroups()])
  } catch (e) {
    message.value = e instanceof Error ? e.message : '批量删除失败'
    messageType.value = 'error'
  }
}

// 打开批量重跑弹窗
function openBatchRun(mode: 'selected' | 'group') {
  batchRunMode.value = mode
  // 下拉只列启用项，所以记忆项被停用时自动退回默认
  batchRunWorkflow.value = pickWorkflowId(workflows.value, { requireEnabled: true })
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
        body: JSON.stringify({ group_name: state.group, workflow_id: batchRunWorkflow.value })
      })
      message.value = `已提交 ${result.created} 个分组任务`
    }
    messageType.value = 'success'
    rememberWorkflowId(batchRunWorkflow.value)
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

function formatTime(t: string) {
  if (!t) return ''
  return new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

// 封面（最新一张结果图）点击打开大图查看器
function openCover(p: Prompt) {
  if (p.cover_image_id) openImageViewer([p.cover_image_id], 0, { mutated: () => load() })
}

let searchTimer: ReturnType<typeof setTimeout> | undefined
function onSearch() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(resetAndLoad, 300)
}

onBeforeUnmount(() => {
  clearTimeout(searchTimer)
  listRequest?.abort()
})

onMounted(() => {
  loadGroups()
  loadWorkflows()
  ensurePlatforms().then(list => { platformOptions.value = list })
  load()
})
</script>

<template>
  <PageHeader title="提示词库" description="统一管理所有提示词，支持分组、搜索、批量操作。" />

  <div class="prompts-layout">
    <!-- 分组侧栏 -->
    <aside class="groups-sidebar card">
      <div class="groups-title">分组</div>
      <div class="groups-list">
        <button
          class="group-item"
          :class="{ active: state.group === '' }"
          @click="selectGroup('')"
        >
          <span>全部提示词</span>
        </button>
        <button
          v-for="g in groups"
          :key="g.group_name"
          class="group-item"
          :class="{ active: state.group === g.group_name }"
          @click="selectGroup(g.group_name)"
        >
          <span class="group-name">{{ g.group_name }}</span>
          <span class="group-count">{{ g.count }}</span>
        </button>
      </div>
      <!-- 分组全部重跑按钮 -->
      <div v-if="state.group" style="padding: 8px 4px 0; flex-shrink: 0;">
        <el-button size="small" style="width:100%;" @click="openBatchRun('group')">
          &#9654; 重跑此分组待生成项
        </el-button>
      </div>
    </aside>

    <!-- 列表区 -->
    <div class="prompts-main">
      <FilterToolbar :active="activeFilters" @clear="clearFilters">
        <el-input
          v-model="state.q"
          type="text"
          placeholder="搜索标题或提示词内容..."
          class="toolbar-search"
          aria-label="搜索标题或提示词内容"
          @input="onSearch" />
        <el-button size="small" :type="state.favorite ? 'primary' : 'default'" @click="state.favorite = !state.favorite; resetAndLoad()">
          {{ state.favorite ? '★ 仅收藏' : '☆ 仅收藏' }}
        </el-button>
        <RouterLink to="/prompts/new" class="btn btn-primary">+ 新建提示词</RouterLink>
        <template #secondary>
          <el-select v-model="state.status" class="filter-select" @change="resetAndLoad" aria-label="状态" :empty-values="[]">
            <el-option value="" label="全部状态" />
            <el-option value="pending" label="待生成" />
            <el-option value="running" label="生成中" />
            <el-option value="done" label="已完成" />
            <el-option value="failed" label="失败" />
          </el-select>
          <el-select v-model="state.platform" class="filter-select" @change="resetAndLoad" aria-label="来源" :empty-values="[]">
            <el-option value="" label="全部来源" />
            <el-option value="local" label="本平台" />
            <el-option v-for="p in platformOptions" :key="p.id" :value="p.id" :label="p.label" />
          </el-select>
        </template>
      </FilterToolbar>

      <!-- 操作栏 -->
      <div class="batch-bar">
        <el-checkbox class="checkbox-wrap" :model-value="allSelected" @change="toggleSelectAll">全选</el-checkbox>
        <span class="batch-info">已选 {{ selectedCount }} 条</span>
        <template v-if="selectedCount > 0">
          <el-button type="primary" size="small" @click="openBatchRun('selected')">&#9654; 批量重跑</el-button>
          <el-button type="danger" text size="small" @click="batchDelete">&#10005; 批量删除</el-button>
          <el-button text size="small" @click="selectedIds = new Set()">取消选择</el-button>
        </template>
      </div>

      <ViewFeedback :kind="messageType" :message="message" />

      <div class="card">
        <ViewFeedback v-if="loading" kind="loading" />
        <ViewFeedback v-else-if="loadError" kind="error" :message="loadError">
          <el-button text size="small" @click="load">重试</el-button>
        </ViewFeedback>
        <ViewFeedback v-else-if="!prompts.length" kind="empty"
          :message="activeFilters.length ? '没有匹配的提示词' : '暂无提示词，可通过直接提交或 JSON 文件导入'" />
        <div v-else class="prompt-list">
          <div class="prompt-card" v-for="p in prompts" :key="p.id" :class="{ selected: selectedIds.has(p.id) }">
            <div class="pc-head">
              <el-checkbox class="checkbox-wrap" :model-value="selectedIds.has(p.id)" :aria-label="'选择提示词 ' + p.id" @click.stop @change="toggleSelect(p.id)" />
              <RouterLink :to="`/prompts/${p.id}`" class="pc-title" :title="p.title">{{ p.title }}</RouterLink>
              <PlatformBadge v-if="p.source_platform" :platform="p.source_platform" />
              <span class="badge" :class="'badge-' + p.status">{{ statusBadge(p.status) }}</span>
              <span v-if="p.group_name" class="badge badge-muted">{{ p.group_name }}</span>
              <div class="pc-actions">
                <button class="btn-icon" :class="{ 'fav-active': p.is_favorite }" @click.stop="toggleFavorite(p)" :aria-label="p.is_favorite ? '取消收藏' : '收藏'">
                  {{ p.is_favorite ? '★' : '☆' }}
                </button>
                <RouterLink :to="`/prompts/${p.id}`" class="btn btn-ghost btn-sm">查看</RouterLink>
                <RouterLink :to="`/prompts/${p.id}/edit`" class="btn btn-ghost btn-sm">&#9998;</RouterLink>
                <el-button text type="danger" size="small" :icon="Delete" aria-label="删除提示词" @click="deletePrompt(p)" />
              </div>
            </div>
            <div class="pc-body">
              <div class="pc-cover" :class="{ clickable: p.cover_image_id }" @click.stop="openCover(p)" :title="p.cover_image_id ? '查看最新结果图' : ''">
                <img v-if="p.cover_image_id" :src="thumbUrl(p.cover_image_id)" loading="lazy" decoding="async" :alt="p.title" />
                <div v-else class="pc-cover-empty">无图</div>
              </div>
              <div class="pc-meta">
                <div class="pc-meta-line">
                  <span>创建 {{ formatTime(p.created_at) }}</span>
                  <span v-if="p.completed_at"> · 完成 {{ formatTime(p.completed_at) }}</span>
                  <span> · 执行 {{ p.run_count }} 次</span>
                  <span> · 图片 {{ p.image_count }} 张</span>
                </div>
                <div class="pc-prompt">{{ p.positive_prompt }}</div>
              </div>
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
    </div>
  </div>

  <el-dialog
    v-model="showBatchRunModal"
    :title="batchRunMode === 'group' ? `重跑分组「${state.group}」` : `批量重跑 ${selectedCount} 条`"
    class="app-dialog"
    width="min(480px, calc(100% - 24px))"
    top="6vh"
    append-to-body
  >
    <label class="form-label" for="batch-workflow">选择工作流</label>
    <el-select id="batch-workflow" v-model="batchRunWorkflow" :empty-values="[0]" placeholder="请选择工作流" filterable popper-class="bounded-select-options">
      <el-option v-for="w in workflows.filter(x => x.enabled)" :key="w.id" :value="w.id" :label="w.name" />
    </el-select>
    <p v-if="batchRunMode === 'group'" class="text-xs muted" style="margin-top:8px;">
      仅重跑状态为「待生成」和「失败」的条目。
    </p>
    <template #footer>
      <el-button @click="showBatchRunModal = false">取消</el-button>
      <el-button type="primary" :loading="isBatchRunning" :disabled="!batchRunWorkflow" @click="confirmBatchRun">开始生成</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.prompts-layout { display: grid; grid-template-columns: 220px 1fr; gap: 16px; }
/* grid 子项默认 min-width:auto，刚性内容会撑爆列宽导致页面横向滚动 */
.prompts-main { min-width: 0; }
.groups-sidebar { min-width: 0; padding: 12px; max-height: calc(100vh - 220px); display: flex; flex-direction: column; min-height: 0; }
.groups-title { font-size: 12px; font-weight: 700; text-transform: uppercase; letter-spacing: 0.08em; color: var(--c-muted); padding: 4px 10px; margin-bottom: 6px; flex-shrink: 0; }
.groups-list { overflow-y: auto; flex: 1; min-height: 0; margin: 0 -4px; padding: 0 4px; }
.groups-list .group-item:first-child { margin-top: 0; }
.group-item { display: flex; align-items: center; justify-content: space-between; width: 100%; padding: 8px 10px; border: none; background: none; border-radius: 8px; cursor: pointer; font-size: 14px; text-align: left; color: var(--c-text); }
.group-item:hover { background: var(--c-bg-hover); }
.group-item.active { background: var(--c-primary); color: #fff; }
.group-item.active .group-count { background: rgba(255,255,255,0.25); color: #fff; }
.group-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.group-count { font-size: 12px; background: var(--c-bg-subtle); color: var(--c-muted); padding: 1px 8px; border-radius: 10px; flex-shrink: 0; margin-left: 8px; }
.prompt-list { display: flex; flex-direction: column; }
.prompt-card { padding: 12px 14px; border-bottom: 1px solid var(--c-border-light); }
.prompt-card:last-child { border-bottom: none; }
.prompt-card.selected { background: var(--c-primary-light); }
.pc-head { display: flex; align-items: center; gap: 8px; min-width: 0; }
.pc-title { font-weight: 600; font-size: 15px; text-decoration: none; color: var(--c-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; }
.pc-title:hover { color: var(--c-primary); }
.pc-actions { display: flex; gap: 4px; flex-shrink: 0; margin-left: auto; }
.pc-body { display: flex; gap: 12px; margin-top: 10px; min-width: 0; }
.pc-cover { width: 88px; height: 88px; border-radius: 8px; border: 1px solid var(--c-border); overflow: hidden; flex-shrink: 0; background: var(--c-bg-subtle); }
.pc-cover img { width: 100%; height: 100%; object-fit: cover; display: block; }
.pc-cover.clickable { cursor: zoom-in; }
.pc-cover.clickable:hover { border-color: var(--c-primary); }
.pc-cover-empty { width: 100%; height: 100%; display: grid; place-items: center; font-size: 11px; color: var(--c-muted); }
.pc-meta { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 6px; }
.pc-meta-line { display: flex; gap: 4px; flex-wrap: wrap; font-size: 12px; color: var(--c-muted); }
.pc-prompt { font-size: 13px; color: var(--c-text-secondary, var(--c-muted)); line-height: 1.5; white-space: pre-wrap; word-break: break-word; display: -webkit-box; -webkit-line-clamp: 4; -webkit-box-orient: vertical; overflow: hidden; }
.btn-icon { background: none; border: none; cursor: pointer; font-size: 16px; padding: 4px; color: var(--c-muted); transition: color 0.2s; }
.btn-icon:hover { color: #f59e0b; }
.btn-icon.fav-active { color: #d97706; }
.btn-icon.fav-active:hover { color: #b45309; }
.badge-muted { background: var(--c-bg-subtle); color: var(--c-muted); }
.badge-info { background: #e0f2fe; color: #0369a1; }
.select-row { padding: 6px 12px; border-bottom: 1px solid var(--c-border-light); display: flex; align-items: center; gap: 8px; }
.checkbox-wrap { display: flex; align-items: center; gap: 6px; cursor: pointer; flex-shrink: 0; }
.batch-bar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; background: var(--c-primary-light); border: 1px solid var(--c-primary-border); border-radius: 8px; margin-bottom: 8px; flex-wrap: wrap; }
.batch-info { font-size: 13px; font-weight: 600; color: var(--c-primary); margin-right: 4px; }
@media (max-width: 768px) {
  /* 移动端防横向溢出：首排允许换行、按钮组收紧（对齐全局 .list-row 的保护策略） */
  .prompt-card { padding: 12px; }
  .pc-head { flex-wrap: wrap; row-gap: 8px; }
  /* 断行按 flex-basis 假想宽度计算：标题 basis 设为整行减勾选框宽，
     保证「勾选框+标题」恒为第一行（标题拉伸填满），徽章与操作恒为第二行；
     若用 flex:1 1 0，短标题时徽章会留在第一行，行结构不稳定 */
  .pc-title { flex: 1 1 calc(100% - 40px); }
  /* 第二行：徽章靠左、操作靠右 */
  .pc-actions { flex-wrap: wrap; justify-content: flex-end; }
  .pc-cover { width: 72px; height: 72px; }
  .pc-prompt { -webkit-line-clamp: 3; }
}
@media (max-width: 720px) {
  .prompts-layout { grid-template-columns: 1fr; }
  .groups-sidebar { max-height: 200px; }
}
</style>
