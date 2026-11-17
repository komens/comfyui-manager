<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Workflow } from '../api/client'
import { pickWorkflowId, rememberWorkflowId } from '../utils/workflowParams'

const router = useRouter()
const workflows = ref<Workflow[]>([])
const workflowId = ref(0)
const positive = ref('')
const negative = ref('')
const title = ref('')
const message = ref('')
const messageType = ref<'success' | 'error' | 'info'>('info')
const busy = ref(false)
const loadingWorkflows = ref(true)
const workflowError = ref(false)

const selectedWorkflow = computed(() => workflows.value.find(w => w.id === workflowId.value))

async function loadWorkflows() {
  loadingWorkflows.value = true
  workflowError.value = false
  try {
    const result = await api<{ items: Workflow[] }>('/api/workflows?page_size=200')
    workflows.value = result.items.filter(item => item.enabled)
    workflowId.value = pickWorkflowId(workflows.value)
  } catch {
    workflowError.value = true
  } finally {
    loadingWorkflows.value = false
  }
}
onMounted(loadWorkflows)

async function submit() {
  busy.value = true
  message.value = ''
  try {
    const data = await api<{ id: number }>('/api/tasks/direct', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        workflow_id: workflowId.value,
        positive_prompt: positive.value,
        negative_prompt: negative.value,
        title: title.value,
      }),
    })
    message.value = `任务 #${data.id} 已创建，正在跳转...`
    messageType.value = 'success'
    // 提交成功才算「上次用过的选择」，换页再回来仍是这条
    rememberWorkflowId(workflowId.value)
    positive.value = ''
    negative.value = ''
    title.value = ''
    setTimeout(() => router.push(`/tasks/${data.id}`), 800)
  } catch (e) {
    message.value = e instanceof Error ? e.message : '提交失败'
    messageType.value = 'error'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <PageHeader title="直接提交" description="输入提示词并选择工作流生成图片。" />

  <div class="card" style="max-width:760px;">
    <div class="card-body">
      <div class="form-group">
        <label for="workflow">工作流</label>
        <select id="workflow" v-model="workflowId" :disabled="loadingWorkflows || !workflows.length">
          <option v-for="item in workflows" :key="item.id" :value="item.id">{{ item.name }}</option>
        </select>
        <p v-if="loadingWorkflows" class="form-hint">正在加载工作流…</p>
        <div v-else-if="workflowError" class="form-hint">
          工作流加载失败。<button type="button" class="btn btn-ghost btn-sm" @click="loadWorkflows">重试</button>
        </div>
        <div v-else-if="!workflows.length" class="form-hint">
          请先创建或启用工作流。<RouterLink class="btn btn-secondary btn-sm" to="/workflows">管理工作流</RouterLink>
        </div>
        <p v-if="selectedWorkflow?.negative_prompt" class="form-hint">
          工作流默认负面：{{ selectedWorkflow.negative_prompt }}（下方留空则使用它）
        </p>
      </div>

      <div class="form-group">
        <label for="title">标题 <span class="text-xs muted">(可选，留空自动取提示词开头)</span></label>
        <input id="title" type="text" v-model="title" placeholder="给这条提示词起个名字，方便在提示词库查找" />
      </div>

      <div class="form-group">
        <label for="positive">正向提示词</label>
        <textarea id="positive" v-model="positive" rows="8" placeholder="描述你想要生成的图像内容..." />
      </div>

      <div class="form-group">
        <label for="negative">负面提示词 <span class="text-xs muted">(可选，留空则用工作流默认)</span></label>
        <textarea id="negative" v-model="negative" rows="4" placeholder="描述你想要避免的内容，如 blurry / extra fingers / watermark..." />
      </div>

      <div class="btn-group" style="margin-top:24px;">
        <button class="btn btn-primary" :disabled="busy || !workflowId || !positive.trim()" @click="submit">
          <span v-if="busy" class="spinner"></span>
          {{ busy ? '提交中...' : '提交作图' }}
        </button>
      </div>

      <div v-if="message" class="status-msg" :class="messageType">{{ message }}</div>
    </div>
  </div>
</template>
