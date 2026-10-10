<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import { api } from '../api/client'
import type { CleanupPreview, CleanupStats, TrashBatch, Workflow } from '../api/client'

/**
 * 数据整理页。
 *
 * 核心约束（与后端 maintenance.go 一致，改动时两边都要看）：
 * - 一切都走「先预览、再确认、才删除」。预览不碰任何数据。
 * - 收藏永不删。后端已写死保护，这里只提供「包含收藏」的反向开关给高级用户。
 * - 删掉的图片文件进回收站，不是直接抹掉。
 *
 * 筛选按类型分组收进可折叠面板：工作流几十个、JSON 分组上百个时平铺会撑爆页面，
 * 所以工作流/分组都用「可搜索多选」，时间用范围选择器。
 */

type Target = 'images' | 'tasks' | 'prompts'

const target = ref<Target>('images')
const dateRange = ref<[Date, Date] | null>(null)
const keyword = ref('')
const groupNames = ref<string[]>([])
const workflowIds = ref<number[]>([])
const status = ref('')
const includeFavorite = ref(false)
const withPrompts = ref(true)
const cascadeImages = ref(false)
const minCount = ref(1)

const filtersOpen = ref(['filters'])
const previewing = ref(false)
const preview = ref<CleanupPreview | null>(null)
const running = ref(false)
const error = ref('')
const result = ref<any>(null)

const stats = ref<CleanupStats | null>(null)
const workflows = ref<Workflow[]>([])
const groups = ref<string[]>([])
const trashBatches = ref<TrashBatch[]>([])
const busyTrash = ref(0)

const targetLabel: Record<Target, string> = {
  images: '图片',
  tasks: '任务',
  prompts: '提示词',
}

const targetOptions = Object.entries(targetLabel).map(([value, label]) => ({ value, label }))

const statusOptions = [
  { value: '', label: '不限（不含运行中）' },
  { value: 'completed', label: '已完成' },
  { value: 'failed', label: '失败' },
  { value: 'cancelled', label: '已取消' },
  { value: 'pending', label: '排队中' },
]

const workflowOptions = computed(() => workflows.value.map((w) => ({ value: w.id, label: w.name })))
const groupOptions = computed(() => groups.value.map((g) => ({ value: g, label: g })))

/** yyyy-MM-dd：后端按字符串前缀比 created_at，必须送它认得的格式。 */
function fmtDate(d: Date) {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

function buildFilter(confirm: boolean) {
  const from = dateRange.value?.[0] ? fmtDate(dateRange.value[0]) : ''
  const to = dateRange.value?.[1] ? fmtDate(dateRange.value[1]) : ''
  return {
    target: target.value,
    date_from: from,
    date_to: to,
    keyword: keyword.value.trim(),
    group_names: groupNames.value,
    workflow_ids: workflowIds.value,
    status: target.value === 'tasks' ? status.value : '',
    only_unfavorite: !includeFavorite.value,
    with_prompts: withPrompts.value,
    cascade_images: cascadeImages.value,
    min_count: minCount.value,
    confirm,
  }
}

/** 折叠时显示的条件摘要：收起后仍知道筛的是什么，不用反复展开确认。 */
const filterSummary = computed(() => {
  const parts: string[] = [targetLabel[target.value]]
  if (dateRange.value?.[0] && dateRange.value?.[1]) {
    parts.push(`${fmtDate(dateRange.value[0])} ~ ${fmtDate(dateRange.value[1])}`)
  } else if (dateRange.value?.[0]) {
    parts.push(`${fmtDate(dateRange.value[0])} 起`)
  }
  if (keyword.value.trim()) parts.push(`“${keyword.value.trim()}”`)
  if (workflowIds.value.length) parts.push(`${workflowIds.value.length} 个工作流`)
  if (groupNames.value.length) parts.push(`${groupNames.value.length} 个分组`)
  if (status.value && target.value === 'tasks') parts.push(status.value)
  if (includeFavorite.value) parts.push('含收藏')
  return parts.join(' · ')
})

const activeFilterCount = computed(() => {
  let n = 0
  if (dateRange.value?.length === 2) n++
  if (keyword.value.trim()) n++
  if (workflowIds.value.length) n++
  if (groupNames.value.length) n++
  if (status.value && target.value === 'tasks') n++
  if (includeFavorite.value) n++
  return n
})

async function loadStats() {
  try {
    stats.value = await api<CleanupStats>('/api/maintenance/stats')
  } catch {
    // 统计只是辅助信息，拿不到不影响主流程
  }
}

async function loadTrash() {
  try {
    const data = await api<{ items: TrashBatch[] }>('/api/maintenance/trash')
    trashBatches.value = data.items
  } catch {
    trashBatches.value = []
  }
}

/** 拉 JSON 分组清单（来自 json_files）作为多选下拉的选项。 */
async function loadGroups() {
  try {
    const files = await api<{ items: { filename: string }[] }>('/api/json-files')
    groups.value = files.items.map((f) => f.filename)
  } catch {
    groups.value = []
  }
}

onMounted(async () => {
  await Promise.all([loadStats(), loadTrash(), loadGroups()])
  try {
    workflows.value = await api<{ items: Workflow[] }>('/api/workflows').then((d) => d.items)
  } catch {
    workflows.value = []
  }
})

async function doPreview() {
  previewing.value = true
  error.value = ''
  result.value = null
  try {
    preview.value = await api<CleanupPreview>('/api/maintenance/preview', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(buildFilter(false)),
    })
  } catch (e) {
    preview.value = null
    error.value = e instanceof Error ? e.message : '预览失败'
  } finally {
    previewing.value = false
  }
}

// 任何条件变化都让旧预览失效：否则用户会拿着上一个条件的预览点删除。
watch([target, dateRange, keyword, groupNames, workflowIds, status, includeFavorite, withPrompts, cascadeImages, minCount], () => {
  preview.value = null
  result.value = null
})

/** 真正要删的主体数量，不是级联后的图片数。 */
const primaryCount = computed(() => preview.value?.primary_count ?? 0)
const canRun = computed(() => !!preview.value && primaryCount.value > 0 && !preview.value.blocked && !running.value)

async function doCleanup() {
  if (!preview.value) return
  const extra = includeFavorite.value
    ? '\n\n⚠ 你勾选了「包含收藏」，收藏内容也会被删除。'
    : ''
  if (!window.confirm(`即将删除 ${primaryCount.value} 条${targetLabel[target.value]}。\n\n图片文件会移入回收站，可随时还原。${extra}\n\n确认继续？`)) {
    return
  }
  running.value = true
  error.value = ''
  try {
    result.value = await api('/api/maintenance/cleanup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(buildFilter(true)),
    })
    preview.value = null
    await Promise.all([loadStats(), loadTrash()])
  } catch (e) {
    error.value = e instanceof Error ? e.message : '清理失败'
  } finally {
    running.value = false
  }
}

function resetFilters() {
  dateRange.value = null
  keyword.value = ''
  groupNames.value = []
  workflowIds.value = []
  status.value = ''
  includeFavorite.value = false
  minCount.value = 1
  withPrompts.value = true
  cascadeImages.value = false
}

async function restoreBatch(id: number) {
  if (!window.confirm(`还原批次 #${id}？图片文件会移回图片目录。`)) return
  busyTrash.value = id
  error.value = ''
  try {
    await api(`/api/maintenance/trash/${id}/restore`, { method: 'POST' })
    await Promise.all([loadStats(), loadTrash()])
  } catch (e) {
    error.value = e instanceof Error ? e.message : '还原失败'
  } finally {
    busyTrash.value = 0
  }
}

async function purgeBatch(id: number) {
  if (!window.confirm(`彻底删除批次 #${id}？此操作不可恢复。`)) return
  busyTrash.value = id
  error.value = ''
  try {
    await api(`/api/maintenance/trash/${id}`, { method: 'DELETE' })
    await Promise.all([loadStats(), loadTrash()])
  } catch (e) {
    error.value = e instanceof Error ? e.message : '删除失败'
  } finally {
    busyTrash.value = 0
  }
}

async function purgeOrphans() {
  const names = stats.value?.orphan_files ?? []
  if (!names.length) return
  if (!window.confirm(`发现 ${names.length} 个没有任何记录引用的图片文件（孤儿文件），删除后可回收磁盘空间。\n\n确认删除？`)) return
  error.value = ''
  try {
    const data = await api<{ removed: number }>('/api/maintenance/orphans/purge', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: true }),
    })
    await loadStats()
    result.value = { note: `已清理 ${data.removed} 个孤儿图片文件` }
  } catch (e) {
    error.value = e instanceof Error ? e.message : '清理失败'
  }
}

async function runVacuum() {
  if (!window.confirm('整理数据库会重写整个 db 文件，期间数据库被独占。确定继续？')) return
  error.value = ''
  try {
    const data = await api<{ duration_ms: number }>('/api/maintenance/vacuum', { method: 'POST' })
    await loadStats()
    result.value = { note: `数据库整理完成，耗时 ${(data.duration_ms / 1000).toFixed(1)} 秒` }
  } catch (e) {
    error.value = e instanceof Error ? e.message : '整理失败'
  }
}

function formatBytes(bytes: number) {
  if (!bytes) return '0 B'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(2)} MB`
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`
}
</script>

<template>
  <div class="page">
    <PageHeader
      eyebrow="系统"
      title="数据整理"
      description="按条件批量清理图片、任务与提示词。删除的图片会先进回收站，确认无误后再彻底清除。"
    />

    <el-alert v-if="error" type="error" :closable="false" show-icon :title="error" />
    <el-alert v-if="result?.note" type="success" :closable="false" show-icon :title="result.note" />

    <!-- 数据概览 -->
    <section v-if="stats" class="card">
      <h2>数据概览</h2>
      <div class="card-body">
        <div class="stat-grid">
          <div class="stat-tile">
            <span class="v">{{ stats.counts.images }}</span>
            <span class="k">图片记录</span>
          </div>
          <div class="stat-tile">
            <span class="v">{{ stats.counts.generation_tasks }}</span>
            <span class="k">任务</span>
          </div>
          <div class="stat-tile">
            <span class="v">{{ stats.counts.prompts }}</span>
            <span class="k">提示词</span>
          </div>
          <div class="stat-tile">
            <span class="v">{{ formatBytes(stats.image_bytes) }}</span>
            <span class="k">图片占用</span>
          </div>
          <div class="stat-tile">
            <span class="v">{{ formatBytes(stats.db_bytes) }}</span>
            <span class="k">数据库</span>
          </div>
        </div>
        <p class="note">收藏图片 {{ stats.favorite_images }} 张、收藏提示词 {{ stats.favorite_prompts }} 条，永远不会被清理。</p>
        <div class="btn-row">
          <el-button v-if="stats.orphan_files.length" size="small" @click="purgeOrphans">
            清理 {{ stats.orphan_files.length }} 个孤儿文件
          </el-button>
          <el-button size="small" @click="runVacuum">整理数据库（VACUUM）</el-button>
        </div>
      </div>
    </section>

    <!-- 筛选条件：可折叠，折叠时显示条件摘要 -->
    <section class="card filter-card">
      <el-collapse v-model="filtersOpen">
        <el-collapse-item name="filters">
          <template #title>
            <div class="panel-title-wrap">
              <div class="panel-title-row">
                <span class="panel-title">筛选条件</span>
                <el-tag v-if="activeFilterCount" size="small" type="primary" effect="light">
                  {{ activeFilterCount }} 个条件
                </el-tag>
              </div>
              <span v-if="!filtersOpen.includes('filters')" class="panel-summary">{{ filterSummary }}</span>
            </div>
          </template>

          <div class="filter-body">
            <div class="filter-row">
              <label class="fl">清理对象</label>
              <div class="fc">
                <el-radio-group v-model="target" class="ctrl">
                  <el-radio-button v-for="opt in targetOptions" :key="opt.value" :value="opt.value">
                    {{ opt.label }}
                  </el-radio-button>
                </el-radio-group>
              </div>
            </div>

            <div class="filter-row">
              <label class="fl">时间范围</label>
              <div class="fc">
                <el-date-picker
                  v-model="dateRange"
                  type="daterange"
                  range-separator="至"
                  start-placeholder="开始日期"
                  end-placeholder="结束日期"
                  value-format="YYYY-MM-DD"
                  format="YYYY-MM-DD"
                  clearable
                  class="ctrl date"
                />
                <span class="note inline">按生成时间筛选，含起止当天</span>
              </div>
            </div>

            <div class="filter-row">
              <label class="fl">关键词</label>
              <div class="fc">
                <el-input
                  v-model="keyword"
                  placeholder="匹配标题 / 正向提示词 / 文件名，多个词用空格分开表示全部命中"
                  clearable
                  class="ctrl wide"
                />
              </div>
            </div>

            <div class="filter-row">
              <label class="fl">JSON 分组</label>
              <div class="fc">
                <el-select
                  v-model="groupNames"
                  multiple
                  filterable
                  collapse-tags
                  collapse-tags-tooltip
                  :max-collapse-tags="3"
                  clearable
                  placeholder="全部分组"
                  class="ctrl wide"
                >
                  <el-option v-for="g in groupOptions" :key="g.value" :label="g.label" :value="g.value" />
                </el-select>
                <span class="note inline">{{ groups.length }} 个分组</span>
              </div>
            </div>

            <div class="filter-row">
              <label class="fl">工作流</label>
              <div class="fc">
                <el-select
                  v-model="workflowIds"
                  multiple
                  filterable
                  collapse-tags
                  collapse-tags-tooltip
                  :max-collapse-tags="3"
                  clearable
                  placeholder="全部工作流"
                  class="ctrl wide"
                >
                  <el-option v-for="w in workflowOptions" :key="w.value" :label="w.label" :value="w.value" />
                </el-select>
                <span class="note inline">{{ workflows.length }} 个工作流</span>
              </div>
            </div>

            <div v-if="target === 'tasks'" class="filter-row">
              <label class="fl">任务状态</label>
              <div class="fc">
                <el-select v-model="status" placeholder="不限（不含运行中）" clearable class="ctrl">
                  <el-option v-for="opt in statusOptions" :key="opt.value" :label="opt.label" :value="opt.value" />
                </el-select>
              </div>
            </div>

            <div class="filter-row">
              <label class="fl">删除范围</label>
              <div class="fc">
                <div class="checks">
                  <el-checkbox v-model="includeFavorite">包含收藏内容（默认排除，收藏永不删除）</el-checkbox>
                  <el-checkbox v-if="target === 'tasks'" v-model="withPrompts">连带删除不再被引用的提示词</el-checkbox>
                  <el-checkbox v-if="target === 'prompts'" v-model="cascadeImages">连带删除已生成的图片</el-checkbox>
                </div>
              </div>
            </div>

            <div class="filter-row">
              <label class="fl">最少命中</label>
              <div class="fc">
                <el-input-number
                  v-model="minCount"
                  :min="0"
                  :step="10"
                  controls-position="right"
                  class="ctrl num"
                />
                <span class="note inline">安全阀：实际命中少于这个数就拒绝执行，防止条件写太宽误删。填 0 关闭。</span>
              </div>
            </div>

            <div class="filter-actions">
              <el-button type="primary" :loading="previewing" @click="doPreview">
                {{ previewing ? '统计中…' : '预览将删除的数据' }}
              </el-button>
              <el-button @click="resetFilters">重置条件</el-button>
            </div>
          </div>
        </el-collapse-item>
      </el-collapse>
    </section>

    <!-- 预览结果 -->
    <section v-if="preview" class="card">
      <h2>预览</h2>
      <div class="card-body">
        <el-alert v-if="preview.blocked" type="warning" :closable="false" show-icon :title="preview.blocked" class="mb" />
        <el-alert v-if="preview.warning" type="info" :closable="false" show-icon :title="preview.warning" class="mb" />

        <div class="preview-headline">
          <span class="big">{{ primaryCount }}</span>
          <span class="lbl">条{{ targetLabel[target] }}将被删除</span>
        </div>

        <div class="stat-grid">
          <div v-if="target !== 'images'" class="stat-tile">
            <span class="v">{{ preview.stats.images }}</span>
            <span class="k">连带图片</span>
          </div>
          <div class="stat-tile">
            <span class="v">{{ preview.stats.items }}</span>
            <span class="k">连带生成项</span>
          </div>
          <div v-if="target === 'tasks'" class="stat-tile">
            <span class="v">{{ preview.stats.prompts }}</span>
            <span class="k">连带提示词</span>
          </div>
          <div class="stat-tile">
            <span class="v">{{ formatBytes(preview.stats.bytes) }}</span>
            <span class="k">可回收体积</span>
          </div>
          <div v-if="preview.stats.protected > 0" class="stat-tile safe">
            <span class="v">{{ preview.stats.protected }}</span>
            <span class="k">因收藏被跳过</span>
          </div>
        </div>

        <el-collapse v-if="preview.samples.length" class="mt sample-collapse">
          <el-collapse-item title="查看样本（最多 8 条）" name="samples">
            <el-table :data="preview.samples" size="small" style="width: 100%">
              <el-table-column label="预览" width="64">
                <template #default="{ row }">
                  <img
                    class="sample-thumb"
                    :src="`/api/images/${row.id}/file`"
                    :alt="row.filename"
                    loading="lazy"
                    @error="(e: Event) => (e.target as HTMLImageElement).style.visibility = 'hidden'"
                  />
                </template>
              </el-table-column>
              <el-table-column prop="id" label="ID" width="70" />
              <el-table-column label="标题" show-overflow-tooltip>
                <template #default="{ row }">{{ row.title || '（无关联提示词）' }}</template>
              </el-table-column>
              <el-table-column prop="filename" label="文件名" show-overflow-tooltip />
              <el-table-column label="分组" width="180" show-overflow-tooltip>
                <template #default="{ row }">{{ row.group || '—' }}</template>
              </el-table-column>
            </el-table>
          </el-collapse-item>
        </el-collapse>

        <div v-if="result && !result.note" class="mt">
          <el-alert type="success" :closable="false" show-icon>
            <template #title>
              已删除 {{ result.images }} 张图片、{{ result.tasks }} 个任务、{{ result.prompts }} 条提示词，回收
              {{ formatBytes(result.bytes) }}，耗时 {{ result.duration_ms }} ms。批次号 #{{ result.batch_id }}，可在下方回收站还原。
            </template>
          </el-alert>
        </div>

        <div class="danger-zone">
          <el-button type="danger" :disabled="!canRun" :loading="running" @click="doCleanup">
            {{ running ? '清理中…' : `确认删除这 ${primaryCount} 条` }}
          </el-button>
        </div>
      </div>
    </section>

    <!-- 回收站 -->
    <section class="card">
      <h2>回收站</h2>
      <div class="card-body">
        <el-empty v-if="!trashBatches.length" description="回收站是空的" :image-size="64" />
        <el-table v-else :data="trashBatches" size="small" style="width: 100%">
          <el-table-column label="批次" width="80">
            <template #default="{ row }">#{{ row.id }}</template>
          </el-table-column>
          <el-table-column label="条件" prop="reason" show-overflow-tooltip />
          <el-table-column label="图片数" width="90">
            <template #default="{ row }">{{ row.deleted_images }}</template>
          </el-table-column>
          <el-table-column label="体积" width="100">
            <template #default="{ row }">{{ formatBytes(row.bytes) }}</template>
          </el-table-column>
          <el-table-column label="时间" width="170">
            <template #default="{ row }">{{ new Date(row.created_at).toLocaleString('zh-CN') }}</template>
          </el-table-column>
          <el-table-column label="操作" width="180" align="right">
            <template #default="{ row }">
              <el-button size="small" :loading="busyTrash === row.id" @click="restoreBatch(row.id)">还原</el-button>
              <el-button size="small" type="danger" plain :loading="busyTrash === row.id" @click="purgeBatch(row.id)">
                彻底删除
              </el-button>
            </template>
          </el-table-column>
        </el-table>
        <p class="note">还原只会把图片文件移回图片目录；生成项已被级联删除，图片库条目不会自动重建。</p>
      </div>
    </section>
  </div>
</template>

<style scoped>
/*
 * 页面骨架沿用 style.css 的设计系统（.card / .card-body / 变量），
 * 控件本身交给 Element Plus。主题色对齐见 element-theme.css。
 */
.page {
  display: flex;
  flex-direction: column;
  gap: 20px;
}
.card > h2 {
  padding: 18px 24px 0;
  margin: 0;
  font-size: 16px;
  font-weight: 600;
}
.panel-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--c-text);
}
/*
 * 筛选卡片：el-collapse 直接填满 .card（没有 .card-body 包裹），
 * 而 .card 自身无 padding、Element 折叠头默认 padding:0、内容区也无横向 padding，
 * 不补的话整个面板（标题/箭头/控件/按钮）全部贴着卡片边框。
 * 内边距对齐 .card-body 的 24px；同时去掉折叠组件自带边框，避免与卡片边框叠成双线。
 */
.filter-card > .el-collapse {
  border: none;
  border-radius: 0;
}
.filter-card :deep(.el-collapse-item__header) {
  padding: 0 24px;
  /* 折叠后标题区是两行（标题+摘要），EP 默认固定 48px 高会撑爆、摘要贴底边。
     改为自动高度 + 上下内边距，让两行内容有呼吸感 */
  height: auto;
  min-height: 48px;
  padding-top: 10px;
  padding-bottom: 10px;
  line-height: 1.5;
}
.filter-card :deep(.el-collapse-item__content) {
  padding: 4px 24px 24px;
}
/* 标题区竖向排布：第一行「标题 + 条件数」，第二行（仅折叠时）条件摘要。
   这样折叠后仍能看到筛了什么，移动端也不会把右侧展开箭头挤掉。 */
.panel-title-wrap {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
  flex: 1 1 auto;
}
.panel-title-row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
/* 桌面端：摘要单行省略，保持折叠头一行高度；移动端改为换行展开（见下方媒体查询） */
.panel-summary {
  min-width: 0;
  font-size: 12px;
  font-weight: 400;
  color: var(--c-text-secondary);
  line-height: 1.4;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* 条件按「标签 + 控件」两列对齐，比平铺更易扫读 */
.filter-body {
  display: flex;
  flex-direction: column;
  gap: 18px;
  padding-top: 4px;
}
.filter-row {
  display: grid;
  grid-template-columns: 84px minmax(0, 1fr);
  align-items: start;
  gap: 12px;
}
.fl {
  padding-top: 7px;
  font-size: 13px;
  font-weight: 600;
  color: var(--c-text-secondary);
  text-align: right;
}
/*
 * .fc 里控件一律定宽，不让它吃满整行。
 * Element 的 .el-input / .el-select 默认 width:100%，放在 flex 容器里会撑到满行，
 * 把右侧说明文字挤掉（实测 714px 容器里控件也是 714px）。
 * 所以：控件给具体宽度 → 说明文字用 flex:1 自���占据剩余空间。
 */
.fc {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}
/*
 * ⚠️ 宽度必须写成 `.fc .ctrl`（两个类，特异度 0-2-0），不能只写 `.ctrl`（0-1-0）。
 * Element 自己的 `.el-select{width:100%}` 来自全局样式表，同为 0-1-0，
 * 平手时按加载顺序它后加载所以胜出 —— 实测 scoped 里写 `.ctrl{width:260px}`
 * 完全不生效，控件仍是 714px 满行、右侧说明文字被挤没。
 * 注意是 `.fc`（本页的容器，带 data-v）+ `.ctrl` 组合，不是 `.fc .ctrl` 里 .ctrl 也要带属性。
 */
.fc .ctrl {
  flex-shrink: 0;
  width: 260px;
}
.fc .ctrl.wide {
  width: 100%;
}
.fc .ctrl.date {
  width: 320px;
}
.fc .ctrl.num {
  width: 150px;
}
.fc .note.inline {
  flex: 1;
  min-width: 0;
  overflow-wrap: anywhere;
}
.checks {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-top: 4px;
}
.filter-actions {
  display: flex;
  gap: 10px;
  padding-top: 4px;
  margin-left: 96px;
}

.stat-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
  gap: 12px;
}
.stat-tile {
  padding: 12px 14px;
  background: var(--c-bg-subtle);
  border: 1px solid var(--c-border-light);
  border-radius: var(--radius-md);
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.stat-tile .v {
  font-size: 20px;
  font-weight: 700;
  color: var(--c-text);
  line-height: 1.2;
}
.stat-tile .k {
  font-size: 12px;
  color: var(--c-text-secondary);
}
.stat-tile.safe .v {
  color: var(--c-success);
}

.preview-headline {
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 14px;
}
.preview-headline .big {
  font-size: 32px;
  font-weight: 700;
  color: var(--c-danger);
  line-height: 1;
}
.preview-headline .lbl {
  font-size: 14px;
  color: var(--c-text-secondary);
}
.danger-zone {
  margin-top: 18px;
  padding-top: 16px;
  border-top: 1px solid var(--c-border-light);
}
.note {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--c-text-muted);
}
.note.inline {
  margin: 0;
}
.btn-row {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  margin-top: 12px;
}
.mb {
  margin-bottom: 10px;
}
.mt {
  margin-top: 14px;
}
.sample-thumb {
  width: 44px;
  height: 44px;
  object-fit: cover;
  border-radius: 6px;
  border: 1px solid var(--c-border);
  background: var(--c-bg-subtle);
  display: block;
}
/* 样本折叠面板嵌在 .card-body 里（已有内边距），横向保持 0 与上方统计块对齐；
   只修纵向：Element 默认 padding-bottom:25px 导致底部一大块空白、顶部却贴边。 */
.sample-collapse :deep(.el-collapse-item__header) {
  padding: 0 12px;
}
.sample-collapse :deep(.el-collapse-item__content) {
  padding: 4px 12px 8px;
}
@media (max-width: 1024px) {
  /* 中等宽度：说明文字换到控件下方，避免与控件抢横向空间 */
  .fc {
    flex-wrap: wrap;
  }
  .fc .note.inline {
    flex-basis: 100%;
    margin-top: 2px;
  }
}
@media (max-width: 768px) {
  /* 卡片内边距收紧，给控件更多横向空间（手机屏本来就窄） */
  .card-body {
    padding: 16px;
  }
  .card > h2 {
    padding: 16px 16px 0;
  }
  /* 标签在上、控件在下，单列堆叠 */
  .filter-row {
    grid-template-columns: 1fr;
    gap: 6px;
  }
  .fl {
    text-align: left;
    padding-top: 0;
  }
  /* 单列后控件吃满整行；数字输入例外——满宽的步进器视觉上很怪，保持固定宽度 */
  .fc .ctrl,
  .fc .ctrl.date,
  .fc .ctrl.wide {
    width: 100%;
  }
  .fc .ctrl.num {
    width: 160px;
  }
  /* 折叠面板内边距同步收紧，与 .card-body 的移动端 16px 一致 */
  .filter-card :deep(.el-collapse-item__header) {
    padding: 10px 16px;
  }
  .filter-card :deep(.el-collapse-item__content) {
    padding: 4px 16px 20px;
  }
  /* 日期范围选择器内部有最小宽度，允许收缩避免小屏横向溢出 */
  .fc .ctrl.date :deep(.el-range-editor) {
    min-width: 0;
  }
  .filter-actions {
    margin-left: 0;
    flex-wrap: wrap;
  }
  .filter-actions .el-button {
    flex: 1 1 140px;
  }
  /* 折叠摘要改为换行展开，移动端仍能看到筛了什么（不再隐藏） */
  .panel-summary {
    white-space: normal;
    word-break: break-word;
    overflow: visible;
  }
}
@media (max-width: 480px) {
  /* 超窄屏：操作按钮竖排占满整行，避免互相挤压或被截断 */
  .filter-actions {
    flex-direction: column;
  }
  .filter-actions .el-button {
    width: 100%;
    flex: none;
  }
}
</style>