<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import PlatformBadge from '../components/PlatformBadge.vue'
import { api } from '../api/client'
import { ensurePlatforms, platforms, platformMeta } from '../utils/platforms'
import { thumbUrl } from '../utils/imageUrls'

/** 平台选择的记忆键：只存在浏览器里，不占后端设置 */
const PLATFORM_KEY = 'external_import_platform'

type ImportResult = {
  image_id: number
  prompt_id: number
  platform: string
  platform_label: string
  filename: string
  width: number
  height: number
}

const platform = ref('')
const promptText = ref('')
const title = ref('')
const file = ref<File | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)
const previewUrl = ref('')
const dragging = ref(false)
const submitting = ref(false)
const errorMsg = ref('')
const recent = ref<ImportResult[]>([])

const available = computed(() => platforms())
const canSubmit = computed(() => !!file.value && !!platform.value && !!promptText.value.trim() && !submitting.value)

function setFile(next: File | null) {
  if (previewUrl.value) URL.revokeObjectURL(previewUrl.value)
  file.value = next
  previewUrl.value = next ? URL.createObjectURL(next) : ''
  if (next) errorMsg.value = ''
}

function onPick(e: Event) {
  const input = e.target as HTMLInputElement
  const picked = input.files?.[0] ?? null
  if (picked) setFile(picked)
  input.value = '' // 允许再次选择同一个文件
}

function onDrop(e: DragEvent) {
  dragging.value = false
  const dropped = e.dataTransfer?.files?.[0]
  if (dropped) setFile(dropped)
}

/** 支持 Ctrl/Cmd+V 直接粘贴截图：拿提示词时最顺手的路径 */
function onPaste(e: ClipboardEvent) {
  const item = Array.from(e.clipboardData?.items ?? []).find(i => i.type.startsWith('image/'))
  const pasted = item?.getAsFile()
  if (pasted) {
    e.preventDefault()
    setFile(pasted)
  }
}

function clearFile() {
  setFile(null)
}

async function submit() {
  if (!canSubmit.value || !file.value) return
  submitting.value = true
  errorMsg.value = ''
  try {
    const form = new FormData()
    form.append('platform', platform.value)
    form.append('positive_prompt', promptText.value.trim())
    if (title.value.trim()) form.append('title', title.value.trim())
    form.append('file', file.value)
    const result = await api<ImportResult>('/api/external/images', { method: 'POST', body: form })
    recent.value = [result, ...recent.value].slice(0, 20)
    // 只清内容，保留平台：连续录同一家平台的素材是常态
    setFile(null)
    promptText.value = ''
    title.value = ''
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : '添加失败'
  } finally {
    submitting.value = false
  }
}

// 平台选择持久化：刷新、换标签页都回到上次用的那家
watch(platform, v => {
  if (v) localStorage.setItem(PLATFORM_KEY, v)
})

onMounted(async () => {
  const list = await ensurePlatforms()
  const saved = localStorage.getItem(PLATFORM_KEY) || ''
  platform.value = list.some(p => p.id === saved) ? saved : (list[0]?.id ?? '')
  document.addEventListener('paste', onPaste)
})

onBeforeUnmount(() => {
  document.removeEventListener('paste', onPaste)
  if (previewUrl.value) URL.revokeObjectURL(previewUrl.value)
})
</script>

<template>
  <PageHeader
    eyebrow="EXTERNAL SOURCE"
    title="平台同步"
    description="把其他平台生成的图片和提示词单条录入素材库，会和本平台数据一起出现在图片库、提示词库和导出包里。"
  />

  <div class="import-layout">
    <div class="card">
      <div class="card-header">
        <h2>添加一条素材</h2>
      </div>
      <div class="card-body">
        <div class="form-group">
          <label for="ext-platform">来源平台</label>
          <select id="ext-platform" v-model="platform" class="input">
            <option v-for="p in available" :key="p.id" :value="p.id">{{ p.label }}</option>
          </select>
          <p class="form-hint">会记住上次的选择，下次打开自动带出。</p>
        </div>

        <div class="form-group">
          <label>图片</label>
          <div
            class="drop-zone"
            :class="{ dragging, filled: !!file }"
            @dragover.prevent="dragging = true"
            @dragleave.prevent="dragging = false"
            @drop.prevent="onDrop"
            @click="fileInput?.click()"
          >
            <template v-if="previewUrl">
              <img :src="previewUrl" class="drop-preview" alt="待上传图片预览" />
              <button class="btn btn-ghost btn-sm drop-clear" @click.stop="clearFile">重新选择</button>
            </template>
            <template v-else>
              <div class="drop-icon">&#128444;</div>
              <p class="drop-title">点击选择、拖入文件，或直接 Ctrl/⌘+V 粘贴截图</p>
              <p class="text-xs muted">支持 PNG / JPG / GIF / WebP，单张不超过 25MB</p>
            </template>
          </div>
          <input ref="fileInput" type="file" accept="image/png,image/jpeg,image/gif,image/webp" style="display:none" @change="onPick" />
          <p v-if="file" class="form-hint">{{ file.name }} · {{ (file.size / 1024 / 1024).toFixed(2) }} MB</p>
        </div>

        <div class="form-group">
          <label for="ext-prompt">提示词</label>
          <textarea
            id="ext-prompt"
            v-model="promptText"
            rows="6"
            placeholder="粘贴该平台生成这张图时使用的提示词..."
          ></textarea>
        </div>

        <div class="form-group">
          <label for="ext-title">标题（可选）</label>
          <input id="ext-title" v-model="title" type="text" placeholder="留空则取提示词前 50 个字" />
        </div>

        <div v-if="errorMsg" class="status-msg error">{{ errorMsg }}</div>

        <div class="btn-group">
          <button class="btn btn-primary" :disabled="!canSubmit" @click="submit">
            <span v-if="submitting" class="spinner"></span>
            {{ submitting ? '添加中...' : '添加到素材库' }}
          </button>
          <RouterLink :to="platform ? `/gallery?platform=${platform}` : '/gallery'" class="btn btn-ghost">
            去图片库查看
          </RouterLink>
        </div>
      </div>
    </div>

    <div class="card recent-card">
      <div class="card-header">
        <h2>本次已添加（{{ recent.length }}）</h2>
      </div>
      <div class="card-body">
        <div v-if="!recent.length" class="empty-state recent-empty">
          <div class="empty-icon">&#10133;</div>
          <p class="text-sm muted">还没有添加记录，左侧填完点「添加到素材库」。</p>
        </div>
        <div v-else class="recent-list">
          <div v-for="item in recent" :key="item.image_id" class="recent-item">
            <img class="recent-thumb" :src="thumbUrl(item.image_id)" :alt="item.filename" loading="lazy" decoding="async" />
            <div class="recent-meta">
              <div class="recent-head">
                <PlatformBadge :platform="item.platform" />
                <span class="text-xs muted">#{{ item.image_id }}</span>
              </div>
              <div class="text-xs muted">{{ platformMeta(item.platform).label }} · {{ item.width }}×{{ item.height }}</div>
              <div class="recent-links">
                <RouterLink :to="`/prompts/${item.prompt_id}`" class="text-xs link">查看提示词</RouterLink>
                <RouterLink :to="`/gallery?platform=${item.platform}`" class="text-xs link">图片库</RouterLink>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.import-layout { display: grid; grid-template-columns: minmax(0, 1fr) 320px; gap: 16px; align-items: start; }
@media (max-width: 900px) {
  .import-layout { grid-template-columns: 1fr; }
}

.drop-zone {
  position: relative;
  border: 1.5px dashed var(--c-border);
  border-radius: 10px;
  background: var(--c-bg-subtle);
  padding: 22px 16px;
  text-align: center;
  cursor: pointer;
  transition: border-color 0.15s, background 0.15s;
}
.drop-zone:hover, .drop-zone.dragging {
  border-color: var(--c-primary);
  background: var(--c-primary-light);
}
.drop-zone.filled { padding: 10px; }
.drop-icon { font-size: 28px; line-height: 1; margin-bottom: 8px; }
.drop-title { font-size: 13px; margin: 0 0 4px; }
.drop-preview {
  display: block;
  max-width: 100%;
  max-height: 300px;
  margin: 0 auto;
  border-radius: 8px;
  object-fit: contain;
}
.drop-clear { margin-top: 8px; }

.recent-card { position: sticky; top: 16px; }
.recent-empty { padding: 28px 8px; }
.recent-list { display: flex; flex-direction: column; gap: 10px; max-height: 60vh; overflow-y: auto; }
.recent-item {
  display: flex;
  gap: 10px;
  padding: 8px;
  border: 1px solid var(--c-border);
  border-radius: 8px;
  background: var(--c-bg-subtle);
}
.recent-thumb { width: 62px; height: 62px; border-radius: 6px; object-fit: cover; flex-shrink: 0; background: var(--c-surface-hover); }
.recent-meta { min-width: 0; display: flex; flex-direction: column; gap: 3px; }
.recent-head { display: flex; align-items: center; gap: 6px; }
.recent-links { display: flex; gap: 10px; margin-top: 2px; }
.link { color: var(--c-primary); text-decoration: none; }
.link:hover { text-decoration: underline; }
</style>
