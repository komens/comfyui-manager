<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import ParamForm from '../components/ParamForm.vue'
import { api, type Workflow } from '../api/client'
import { buildRunMapping, pickDefaultWorkflow } from '../utils/workflowParams'

const router = useRouter()
const workflows = ref<Workflow[]>([])
const workflowId = ref(0)
const positive = ref('')
const title = ref('')
const parameters = ref<Record<string, any>>({})
const message = ref('')
const messageType = ref<'success' | 'error' | 'info'>('info')
const busy = ref(false)

const selectedWorkflow = computed(() => workflows.value.find(w => w.id === workflowId.value))
const mapping = computed(() => buildRunMapping(selectedWorkflow.value))
// 生成参数项数：用于折叠面板的标题提示与显隐判断
const paramCount = computed(() => Object.keys(mapping.value?.parameters ?? {}).length)

onMounted(async () => {
  const result = await api<{ items: Workflow[] }>('/api/workflows?page_size=200')
  workflows.value = result.items
  // 预选默认工作流（无默认时退回第一个启用项）
  const preferred = pickDefaultWorkflow(workflows.value)
  if (preferred) workflowId.value = preferred.id
})

watch(workflowId, () => {
  parameters.value = {}
})

async function submit() {
  busy.value = true
  message.value = ''
  try {
    const params: Record<string, any> = {
      workflow_id: workflowId.value,
      positive_prompt: positive.value,
      title: title.value,
      parameters: parameters.value,
    }
    const data = await api<{ id: number }>('/api/tasks/direct', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(params),
    })
    message.value = `任务 #${data.id} 已创建，正在跳转...`
    messageType.value = 'success'
    positive.value = ''
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
  <PageHeader eyebrow="NEW TASK" title="直接提交" description="输入提示词并选择工作流生成图片。" />

  <div class="card" style="max-width:760px;">
    <div class="card-body">
      <div class="form-group">
        <label for="workflow">工作流</label>
        <select id="workflow" v-model="workflowId">
          <option v-for="item in workflows" :key="item.id" :value="item.id">{{ item.name }}</option>
        </select>
        <p v-if="!workflows.length" class="form-hint" style="color:var(--c-danger);">请先创建工作流</p>
        <p v-if="selectedWorkflow?.negative_prompt" class="form-hint">
          负面提示词（工作流默认）：{{ selectedWorkflow.negative_prompt }}
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

      <!-- 生成参数：低频调整项，默认折叠。注意用 <details> 只是视觉隐藏，
           子组件仍会挂载，ParamForm 的默认值照常参与提交，不影响作图结果 -->
      <details v-if="paramCount > 0" class="collapse-panel">
        <summary>
          <span>生成参数</span>
          <span class="text-xs muted">{{ paramCount }} 项 · 默认折叠</span>
        </summary>
        <div class="collapse-body">
          <ParamForm :mapping="mapping" v-model="parameters" />
        </div>
      </details>

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
