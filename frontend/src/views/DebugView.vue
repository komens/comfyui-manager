<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import { api } from '../api/client'
import type { DebugFile, DebugStatus } from '../api/client'

/**
 * 调试页。
 *
 * 这个页面只在后端 DEBUG 开启时才有意义（否则接口返回 404），
 * 侧边栏入口也只在那种情况下出现 —— 但用户可能直接输 /debug，
 * 所以这里对「没开 DEBUG」单独给一份开启说明，而不是弹一个红字报错。
 */

const files = ref<DebugFile[]>([])
const dir = ref('')
const available = ref(true)
const loading = ref(false)
const error = ref('')
const busyName = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const data = await api<DebugStatus>('/api/debug/status')
    files.value = data.files
    dir.value = data.dir
    available.value = true
  } catch {
    // 接口不存在 = 后端没开 DEBUG，这是正常状态，不当成错误
    available.value = false
    files.value = []
    dir.value = ''
  } finally {
    loading.value = false
  }
}

onMounted(load)

const existing = computed(() => files.value.filter((f) => f.exists))

function formatBytes(bytes: number) {
  if (!bytes) return '0 B'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

function download(name: string) {
  const a = document.createElement('a')
  a.href = `/api/debug/files/${encodeURIComponent(name)}`
  a.download = ''
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
}

async function clearOne(name: string) {
  if (!window.confirm(`清空 ${name}？此操作不可撤销。`)) return
  busyName.value = name
  error.value = ''
  try {
    await api(`/api/debug/files/${encodeURIComponent(name)}`, { method: 'DELETE' })
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : '清空失败'
  } finally {
    busyName.value = ''
  }
}

async function clearAll() {
  if (!existing.value.length) return
  if (!window.confirm(`清空全部 ${existing.value.length} 个调试文件？此操作不可撤销。`)) return
  busyName.value = '*'
  error.value = ''
  try {
    for (const file of existing.value) {
      const base = file.name.replace(/\.1$/, '')
      await api(`/api/debug/files/${encodeURIComponent(base)}`, { method: 'DELETE' })
    }
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : '清空失败'
  } finally {
    busyName.value = ''
    await load()
  }
}

function copyDir() {
  if (!dir.value) return
  navigator.clipboard?.writeText(dir.value)
}

function describe(name: string) {
  if (name.startsWith('startup')) return '启动快照：路径、版本、库结构'
  if (name.startsWith('submit-payloads')) return '每次 POST /prompt 的完整请求体'
  if (name.startsWith('http')) return '所有出站请求的失败与非 2xx'
  if (name.startsWith('events')) return '任务状态流转与关键决策'
  return ''
}

function formatTime(value?: string) {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false })
}
</script>

<template>
  <PageHeader
    eyebrow="DEBUG"
    title="调试日志"
    description="后端在 DEBUG 模式下落盘的排查线索。目录挂在数据卷上，宿主机也能直接读。"
  />

  <!-- DEBUG 没开：给开启方法，而不是报错 -->
  <div v-if="!available" class="card" style="max-width:720px;">
    <div class="card-header">
      <h2>当前未开启 DEBUG</h2>
    </div>
    <div class="card-body">
      <p class="muted" style="margin-top:0;">
        调试日志默认关闭，此时本页对应的接口不存在，也不会有任何调试文件被写入。
      </p>
      <p class="muted">开启方式：在 <code>docker-compose.yml</code> 同目录建一个 <code>.env</code> 文件，写入</p>
      <pre class="code-block">DEBUG=1</pre>
      <p class="muted">然后重启服务：</p>
      <pre class="code-block">docker compose up -d</pre>
      <p class="muted" style="margin-bottom:0;">
        生效标志是启动日志里出现 <code>debug: ON</code> 那几行。如果没出现，说明镜像里还没有这段代码，需要重新构建镜像。
      </p>
      <div class="warn-box">
        日志会记录完整的提示词内容，排查完记得去掉 <code>DEBUG</code> 再重启一次。
      </div>
    </div>
  </div>

  <template v-else>
    <div class="card" style="max-width:900px;">
      <div class="card-header">
        <h2>落盘位置</h2>
        <div class="btn-group">
          <button class="btn btn-secondary" :disabled="loading" @click="load">
            <span v-if="loading" class="spinner"></span>
            {{ loading ? '刷新中...' : '刷新' }}
          </button>
          <button class="btn btn-ghost" :disabled="!existing.length || busyName === '*'" @click="clearAll">
            清空全部
          </button>
        </div>
      </div>
      <div class="card-body">
        <div class="dir-row">
          <code class="dir-path">{{ dir }}</code>
          <button class="btn btn-ghost btn-sm" @click="copyDir">复制</button>
        </div>
        <p class="form-hint" style="margin-bottom:0;">
          单个文件超过 16MB 会滚成 <code>.1</code>（只保留一代），避免忘记关闭时把数据卷写满。
        </p>
      </div>
    </div>

    <div v-if="error" class="status-msg error" style="max-width:900px;">{{ error }}</div>

    <div class="card" style="margin-top:20px; max-width:900px;">
      <div class="card-header">
        <h2>文件</h2>
      </div>
      <div class="card-body no-pad">
        <table class="debug-table">
          <thead>
            <tr>
              <th>文件</th>
              <th class="num">大小</th>
              <th>最近写入</th>
              <th class="actions">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="file in files" :key="file.name" :class="{ empty: !file.exists }">
              <td>
                <code>{{ file.name }}</code>
                <span v-if="file.truncated" class="tag-warn">已达上限</span>
                <div class="file-desc">{{ describe(file.name) }}</div>
              </td>
              <td class="num">{{ formatBytes(file.bytes) }}</td>
              <td class="muted">{{ file.exists ? formatTime(file.modified) : '—' }}</td>
              <td class="actions">
                <button class="btn btn-ghost btn-sm" :disabled="!file.exists" @click="download(file.name)">下载</button>
                <button
                  class="btn btn-ghost btn-sm"
                  :disabled="!file.exists || busyName === file.name"
                  @click="clearOne(file.name)"
                >清空</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div class="card" style="margin-top:20px; max-width:900px;">
      <div class="card-header">
        <h2>该看哪个</h2>
      </div>
      <div class="card-body">
        <ul class="guide">
          <li><strong>任务卡着不动</strong>：先看 <code>events.jsonl</code> 里的 <code>waiting_for_output</code>，再看 <code>http.jsonl</code> 有没有 <code>history</code> 的 404。ComfyUI 重启过的话 <code>/history</code> 是空的，任务取不回图。</li>
          <li><strong>提交被拒 / 400</strong>：<code>http.jsonl</code> 里带 <code>tag=prompt</code> 的记录有 ComfyUI 的原话；<code>submit-payloads.jsonl</code> 是实际发出去的完整载荷。</li>
          <li><strong>改了工作流却没生效</strong>：<code>startup.json</code> 里的 <code>data_dir</code> 与各目录可写性，能直接看出挂载点对不对。</li>
          <li><strong>出图了但没保存</strong>：<code>events.jsonl</code> 的 <code>save_retry</code>；<code>no_outputs</code> 则会带上 ComfyUI 给的 outputs 原文。</li>
          <li><strong>自动纠正改了什么</strong>：<code>events.jsonl</code> 的 <code>repair_applied</code>，含纠正前后每个字段的值。</li>
        </ul>
      </div>
    </div>
  </template>
</template>

<style scoped>
.dir-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.dir-path {
  flex: 1;
  background: var(--c-bg-subtle, #f5f5f5);
  border: 1px solid var(--c-border-light);
  border-radius: 6px;
  padding: 6px 10px;
  font-size: 13px;
  word-break: break-all;
}
.card-body.no-pad {
  padding: 0;
}
.debug-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 14px;
}
.debug-table th,
.debug-table td {
  text-align: left;
  padding: 10px 16px;
  border-bottom: 1px solid var(--c-border-light);
  vertical-align: top;
}
.debug-table th {
  font-size: 12px;
  font-weight: 600;
  color: var(--c-text-muted);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.debug-table tr:last-child td {
  border-bottom: none;
}
.debug-table tr.empty td {
  opacity: 0.45;
}
.debug-table .num {
  text-align: right;
  white-space: nowrap;
}
.debug-table .actions {
  text-align: right;
  white-space: nowrap;
}
.file-desc {
  font-size: 12px;
  color: var(--c-text-muted);
  margin-top: 2px;
}
.tag-warn {
  display: inline-block;
  margin-left: 6px;
  font-size: 11px;
  padding: 1px 6px;
  border-radius: 999px;
  background: #fff4d6;
  color: #8a6100;
}
.btn-sm {
  padding: 4px 10px;
  font-size: 13px;
}
.code-block {
  background: var(--c-bg-subtle, #f5f5f5);
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
}
.guide {
  margin: 0;
  padding-left: 18px;
  font-size: 14px;
  line-height: 1.9;
}
.guide code {
  background: var(--c-bg-subtle, #f5f5f5);
  padding: 1px 5px;
  border-radius: 4px;
  font-size: 13px;
}
</style>
