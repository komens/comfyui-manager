<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import Pagination from '../components/Pagination.vue'
import { api } from '../api/client'
import type { ScriptFile, ScriptRun, ScriptRunState, ScriptRunSummary, ScriptStatus } from '../api/client'

/**
 * 脚本运行台。
 *
 * 定位是「跑一次性脚本」，不是脚本管理：代码黏进来、点运行、看输出、拿走产出。
 * 每次运行都把代码快照存进库，所以「上次那个改两行再来一遍」直接点历史里的
 * 载入即可，不需要先上传成一个文件。
 *
 * 功能默认关闭（ENABLE_SCRIPTS），关闭时接口 404，这里给一份开启说明而不是报错。
 */

const DEFAULT_CODE = `import wfkit

# 库里有什么（只读）：想改数据请用 wfkit.api()，别直接写库
rows = wfkit.query("SELECT id, filename, storage_path FROM images ORDER BY id DESC LIMIT 5")
for row in rows:
    print(row["id"], wfkit.image_file(row))

print("参数：", wfkit.ARGS)
wfkit.save_output("sample.json", {"count": len(rows)})
`

const status = ref<ScriptStatus | null>(null)
const available = ref(true)
const loading = ref(true)

const code = ref(DEFAULT_CODE)
const language = ref('python')
const args = ref('')
const title = ref('')

const currentId = ref<number | null>(null)
const currentStatus = ref<ScriptRunState | ''>('')
const exitCode = ref(0)
const durationMS = ref(0)
const output = ref('')
const truncated = ref(false)
const runError = ref('')
const running = ref(false)
const submitting = ref(false)
const error = ref('')
const files = ref<ScriptFile[]>([])

const history = ref<ScriptRunSummary[]>([])
const historyTotal = ref(0)
const page = ref(1)
const pageSize = 10

const outputEl = ref<HTMLElement | null>(null)
let source: EventSource | null = null
let pollTimer: number | null = null

const runtimes = computed(() => status.value?.runtimes ?? [])
const currentRuntime = computed(() => runtimes.value.find((item) => item.language === language.value))

const statusLabel: Record<string, string> = {
  queued: '排队中',
  running: '运行中',
  succeeded: '成功',
  failed: '失败',
  timeout: '超时',
  cancelled: '已取消',
}

function badgeClass(state: string) {
  if (state === 'succeeded') return 'badge badge-success'
  if (state === 'failed' || state === 'timeout') return 'badge badge-failed'
  if (state === 'cancelled') return 'badge badge-cancelled'
  if (state === 'running') return 'badge badge-running'
  return 'badge badge-queued'
}

async function loadStatus() {
  loading.value = true
  try {
    const data = await api<ScriptStatus>('/api/scripts/status')
    status.value = data
    available.value = true
    if (!data.runtimes.some((item) => item.language === language.value) && data.runtimes.length) {
      language.value = data.runtimes[0].language
    }
  } catch {
    // 接口不存在 = 后端没开 ENABLE_SCRIPTS，属正常状态
    available.value = false
    status.value = null
  } finally {
    loading.value = false
  }
}

async function loadHistory() {
  if (!available.value) return
  try {
    const data = await api<{ items: ScriptRunSummary[]; total: number }>(
      `/api/scripts/runs?page=${page.value}&page_size=${pageSize}`,
    )
    history.value = data.items
    historyTotal.value = data.total
  } catch (e) {
    error.value = e instanceof Error ? e.message : '读取历史失败'
  }
}

function closeStream() {
  if (source) {
    source.close()
    source = null
  }
  if (pollTimer !== null) {
    window.clearTimeout(pollTimer)
    pollTimer = null
  }
}

function applyDone(payload: Record<string, unknown>) {
  const state = String(payload.status || '')
  currentStatus.value = state as ScriptRunState
  if (typeof payload.output === 'string' && payload.output.length > output.value.length) {
    output.value = payload.output as string
  }
  if (typeof payload.exit_code === 'number') exitCode.value = payload.exit_code
  if (typeof payload.duration_ms === 'number') durationMS.value = payload.duration_ms
  if (typeof payload.error === 'string') runError.value = payload.error
  truncated.value = !!payload.truncated
  running.value = false
  void loadFiles()
  void loadHistory()
}

function subscribe(id: number) {
  closeStream()
  const stream = new EventSource(`/api/scripts/runs/${id}/events?since=0`)
  source = stream
  stream.onmessage = (event) => {
    let payload: Record<string, unknown>
    try {
      payload = JSON.parse(event.data)
    } catch {
      return
    }
    if (payload.type === 'chunk') {
      output.value += String(payload.data ?? '')
      return
    }
    if (payload.type === 'done') {
      applyDone(payload)
      closeStream()
    }
  }
  stream.onerror = () => {
    // 直播断了（服务重启 / 网络切换）就退回轮询，别让页面停在「运行中」
    closeStream()
    pollRun(id)
  }
}

function pollRun(id: number) {
  pollTimer = window.setTimeout(async () => {
    try {
      const run = await api<ScriptRun>(`/api/scripts/runs/${id}`)
      if (run.status === 'queued' || run.status === 'running') {
        output.value = run.output
        pollRun(id)
        return
      }
      output.value = run.output
      currentStatus.value = run.status
      exitCode.value = run.exit_code
      durationMS.value = run.duration_ms
      runError.value = run.error
      truncated.value = run.output_truncated
      running.value = false
      void loadFiles()
      void loadHistory()
    } catch {
      running.value = false
    }
  }, 1000)
}

async function start() {
  error.value = ''
  if (!code.value.trim()) {
    error.value = '代码不能为空'
    return
  }
  submitting.value = true
  try {
    const data = await api<{ run_id: number }>('/api/scripts/runs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        code: code.value,
        language: language.value,
        args: args.value,
        title: title.value,
      }),
    })
    currentId.value = data.run_id
    currentStatus.value = 'queued'
    output.value = ''
    truncated.value = false
    runError.value = ''
    exitCode.value = 0
    durationMS.value = 0
    files.value = []
    running.value = true
    subscribe(data.run_id)
    void loadHistory()
  } catch (e) {
    error.value = e instanceof Error ? e.message : '提交失败'
  } finally {
    submitting.value = false
  }
}

async function cancel() {
  if (currentId.value == null) return
  try {
    await api(`/api/scripts/runs/${currentId.value}/cancel`, { method: 'POST' })
  } catch (e) {
    error.value = e instanceof Error ? e.message : '取消失败'
  }
}

async function openRun(id: number) {
  try {
    const run = await api<ScriptRun>(`/api/scripts/runs/${id}`)
    currentId.value = run.id
    currentStatus.value = run.status
    code.value = run.code
    language.value = run.language
    args.value = run.args
    title.value = run.title
    output.value = run.output
    exitCode.value = run.exit_code
    durationMS.value = run.duration_ms
    runError.value = run.error
    truncated.value = run.output_truncated
    if (run.status === 'queued' || run.status === 'running') {
      running.value = true
      subscribe(run.id)
    } else {
      running.value = false
      void loadFiles()
    }
  } catch (e) {
    error.value = e instanceof Error ? e.message : '读取失败'
  }
}

async function removeRun(id: number) {
  if (!window.confirm('删除这条运行记录和它的产出文件？')) return
  try {
    await api(`/api/scripts/runs/${id}`, { method: 'DELETE' })
    if (currentId.value === id) {
      currentId.value = null
      currentStatus.value = ''
      output.value = ''
      files.value = []
    }
    await loadHistory()
  } catch (e) {
    error.value = e instanceof Error ? e.message : '删除失败'
  }
}

async function loadFiles() {
  if (currentId.value == null) {
    files.value = []
    return
  }
  try {
    const data = await api<{ items: ScriptFile[] }>(`/api/scripts/runs/${currentId.value}/files`)
    files.value = data.items
  } catch {
    files.value = []
  }
}

function fileUrl(name: string) {
  return `/api/scripts/runs/${currentId.value}/files/download?name=${encodeURIComponent(name)}`
}

function copyOutput() {
  if (output.value) navigator.clipboard?.writeText(output.value)
}

function formatBytes(bytes: number) {
  if (!bytes) return '0 B'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

function formatDuration(ms: number) {
  if (!ms) return '—'
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

function formatTime(value: string) {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false })
}

// 输出跟着底部走，运行中才自动滚，避免用户往回翻日志时被拽走
watch(output, async () => {
  if (!running.value) return
  await nextTick()
  if (outputEl.value) outputEl.value.scrollTop = outputEl.value.scrollHeight
})

watch(page, () => void loadHistory())

onMounted(async () => {
  await loadStatus()
  await loadHistory()
})

onUnmounted(closeStream)
</script>

<template>
  <PageHeader
    eyebrow="SCRIPTS"
    title="脚本运行台"
    description="贴一段代码直接跑，跑完把产出带走。适合一次性的数据整理，不必为每种场景做一个功能。"
  />

  <!-- 未开启：给开启方法，而不是报错 -->
  <div v-if="!available && !loading" class="card" style="max-width:760px;">
    <div class="card-header">
      <h2>当前未开启脚本功能</h2>
    </div>
    <div class="card-body">
      <p class="muted" style="margin-top:0;">
        脚本功能默认关闭：关闭时这一段接口根本不存在，任何请求都拿不到执行能力。
      </p>
      <p class="muted">开启方式：在 <code>docker-compose.yml</code> 同目录的 <code>.env</code> 里写入</p>
      <pre class="code-block">ENABLE_SCRIPTS=1</pre>
      <p class="muted">然后重启服务：</p>
      <pre class="code-block">docker compose up -d</pre>
      <p class="muted" style="margin-bottom:0;">
        镜像里还需要 <code>python3</code>（Dockerfile 已包含）。生效标志是启动日志里出现 <code>scripts: ON</code>。
      </p>
      <div class="warn-box">
        开启后脚本以服务进程权限执行，能读写 <code>data/</code> 整个目录。
        这个服务没有登录鉴权，<strong>只在可信内网里开</strong>，别让它直接暴露到公网。
      </div>
    </div>
  </div>

  <template v-else>
    <div v-if="error" class="status-msg error">{{ error }}</div>

    <div class="script-grid">
      <div class="card">
        <div class="card-header">
          <h2>脚本</h2>
          <div class="btn-group">
            <select v-model="language" class="lang-select" :disabled="running">
              <option v-for="item in runtimes" :key="item.language" :value="item.language">
                {{ item.language }} · {{ item.version }}
              </option>
            </select>
            <button v-if="!running" class="btn btn-primary" :disabled="submitting" @click="start">
              <span v-if="submitting" class="spinner"></span>
              {{ submitting ? '提交中...' : '运行' }}
            </button>
            <button v-else class="btn btn-danger" @click="cancel">取消</button>
          </div>
        </div>
        <div class="card-body">
          <div class="meta-row">
            <input v-model="title" type="text" placeholder="标题（可留空，自动用时间）" />
            <input v-model="args" type="text" placeholder='参数，如 --days 7 "/data/我的 图"' />
          </div>
          <textarea v-model="code" class="code-editor" spellcheck="false"></textarea>
          <p class="form-hint" style="margin-bottom:0;">
            工作目录是本次运行专属的目录，产出请写到 <code>out/</code>；
            <code>import wfkit</code> 可以查库（只读）、调本服务接口、存文件。
            超时 {{ status?.limits.timeout_seconds ?? 300 }} 秒。
          </p>
        </div>
      </div>

      <div class="card">
        <div class="card-header">
          <h2>输出</h2>
          <div class="btn-group">
            <span v-if="currentStatus" :class="badgeClass(currentStatus)">
              {{ statusLabel[currentStatus] || currentStatus }}
            </span>
            <span v-if="currentStatus" class="run-meta">
              {{ formatDuration(durationMS) }}<template v-if="currentStatus !== 'queued' && currentStatus !== 'running'"> · 退出码 {{ exitCode }}</template>
            </span>
            <button class="btn btn-ghost btn-sm" :disabled="!output" @click="copyOutput">复制</button>
          </div>
        </div>
        <div class="card-body no-pad">
          <pre ref="outputEl" class="output" :class="{ empty: !output }">{{ output || '（还没有运行）' }}</pre>
          <div class="output-foot">
            <span v-if="truncated" class="tag-warn">输出超过 2MB，已截断</span>
            <span v-if="runError" class="run-error">{{ runError }}</span>
            <div v-if="files.length" class="files-row">
              <span class="files-label">产出</span>
              <a v-for="item in files" :key="item.name" :href="fileUrl(item.name)" :download="item.name" class="file-link">
                {{ item.name }} <small>{{ formatBytes(item.size) }}</small>
              </a>
            </div>
          </div>
        </div>
      </div>
    </div>

    <div class="card" style="margin-top:20px;">
      <div class="card-header">
        <h2>历史</h2>
        <span class="muted text-xs">只保留最近 {{ status?.limits.max_keep ?? 50 }} 次，多余的记录与目录自动清掉</span>
      </div>
      <div class="card-body no-pad">
        <div v-if="!history.length" class="empty-state">
          <p>还没有运行过。上面写段代码点「运行」就行。</p>
        </div>
        <table v-else class="data-table">
          <thead>
            <tr>
              <th>时间</th>
              <th>标题</th>
              <th>状态</th>
              <th class="num">耗时</th>
              <th class="actions">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in history" :key="item.id" :class="{ active: item.id === currentId }">
              <td class="muted text-sm">{{ formatTime(item.created_at) }}</td>
              <td>
                {{ item.title }}
                <div class="row-sub">{{ item.language }}<template v-if="item.error"> · {{ item.error }}</template></div>
              </td>
              <td><span :class="badgeClass(item.status)">{{ statusLabel[item.status] || item.status }}</span></td>
              <td class="num text-sm">{{ formatDuration(item.duration_ms) }}</td>
              <td class="actions">
                <button class="btn btn-ghost btn-sm" @click="openRun(item.id)">载入</button>
                <button class="btn btn-ghost btn-sm" :disabled="item.status === 'running' || item.status === 'queued'" @click="removeRun(item.id)">删除</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="historyTotal > pageSize" class="card-body" style="padding-top:0;">
        <Pagination :total="historyTotal" :page="page" :page-size="pageSize" @update:page="page = $event" />
      </div>
    </div>

    <div class="card" style="margin-top:20px;">
      <div class="card-header">
        <h2>脚本里能拿到什么</h2>
      </div>
      <div class="card-body">
        <ul class="guide">
          <li><code>DATA_DIR</code> / <code>DB_PATH</code> / <code>API_BASE</code> / <code>RUN_DIR</code> / <code>RUN_ID</code> 已注入环境变量，<code>wfkit.ARGS</code> 是上面填的参数。</li>
          <li><code>wfkit.query(sql)</code> 走只读连接。数据库请不要直接写：服务端用 WAL + 写锁排队，脚本里跑长事务会把提交卡住。</li>
          <li>要改数据请用 <code>wfkit.api("DELETE", "/api/images/12")</code> —— 只有走接口才会经过收藏保护和回收站。</li>
          <li><code>wfkit.image_file(row)</code> 负责把 <code>storage_path</code> 解析成真实路径（它可能是相对路径，也可能绝对路径）。</li>
          <li><code>wfkit.save_output("x.json", data)</code> 写进 <code>out/</code>，跑完之后在输出上方可以直接下载。</li>
        </ul>
      </div>
    </div>
  </template>
</template>

<style scoped>
.script-grid {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 20px;
  align-items: start;
}
@media (max-width: 1100px) {
  .script-grid { grid-template-columns: minmax(0, 1fr); }
}
.lang-select {
  width: auto;
  min-width: 130px;
  padding: 6px 30px 6px 10px;
  font-size: 13px;
}
.meta-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 10px;
  margin-bottom: 12px;
}
@media (max-width: 700px) {
  .meta-row { grid-template-columns: minmax(0, 1fr); }
}
.code-editor {
  width: 100%;
  min-height: 320px;
  font-family: var(--font-mono);
  font-size: 13px;
  line-height: 1.65;
  tab-size: 4;
}
@media (max-width: 1100px) {
  .code-editor { min-height: 240px; }
}
.card-body.no-pad { padding: 0; }
.output {
  margin: 0;
  padding: 14px 16px;
  height: 340px;
  overflow: auto;
  background: #1b1f24;
  color: #e6e6e6;
  font-family: var(--font-mono);
  font-size: 12.5px;
  line-height: 1.7;
  white-space: pre-wrap;
  word-break: break-word;
  border-radius: 0 0 var(--radius-md) var(--radius-md);
}
.output.empty {
  color: #6b7480;
}
.output-foot {
  padding: 10px 16px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  font-size: 12px;
}
.output-foot:empty { display: none; }
.run-meta {
  font-size: 12px;
  color: var(--c-text-muted);
}
.run-error {
  color: var(--c-danger);
}
.tag-warn {
  display: inline-block;
  font-size: 11px;
  padding: 1px 6px;
  border-radius: 999px;
  background: #fff4d6;
  color: #8a6100;
  align-self: flex-start;
}
.files-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.files-label {
  color: var(--c-text-muted);
}
.file-link {
  color: var(--c-primary);
  text-decoration: none;
  border-bottom: 1px dashed var(--c-primary-border);
}
.file-link small {
  color: var(--c-text-muted);
}
.data-table .num { text-align: right; white-space: nowrap; }
.data-table .actions { text-align: right; white-space: nowrap; }
.data-table tr.active td { background: var(--c-primary-light); }
.row-sub {
  font-size: 12px;
  color: var(--c-text-muted);
  margin-top: 2px;
}
.code-block {
  background: var(--c-bg-subtle);
  border: 1px solid var(--c-border-light);
  border-radius: 6px;
  padding: 10px 12px;
  font-size: 13px;
  overflow-x: auto;
  margin: 8px 0 14px;
}
.warn-box {
  margin-top: 16px;
  padding: 10px 12px;
  border-radius: 8px;
  background: #fff4d6;
  color: #8a6100;
  font-size: 13px;
  line-height: 1.7;
}
.guide {
  margin: 0;
  padding-left: 18px;
  font-size: 14px;
  line-height: 1.9;
}
.guide code {
  background: var(--c-bg-subtle);
  padding: 1px 5px;
  border-radius: 4px;
  font-size: 13px;
}
</style>
