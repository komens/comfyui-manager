<script setup lang="ts">
import ViewFeedback from '../components/ViewFeedback.vue'
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Workflow } from '../api/client'
import { showTaskSubmissionMessage } from '../utils/taskSubmissionMessage'
import { fetchWorkflows, pickWorkflowId, rememberWorkflowId } from '../utils/workflowParams'

const router = useRouter()
const workflows = ref<Workflow[]>([])
const workflowId = ref(0)
const positive = ref('')
const negative = ref('')
const title = ref('')
const message = ref('')
const busy = ref(false)
const loadingWorkflows = ref(true)
const workflowError = ref(false)

const selectedWorkflow = computed(() => workflows.value.find(w => w.id === workflowId.value))

async function loadWorkflows() {
  loadingWorkflows.value = true
  workflowError.value = false
  try {
    workflows.value = (await fetchWorkflows()).filter(item => item.enabled)
    workflowId.value = pickWorkflowId(workflows.value)
  } catch {
    workflowError.value = true
  } finally {
    loadingWorkflows.value = false
  }
}
onMounted(loadWorkflows)

async function submit() {
  if (busy.value) return
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
    // 提交成功才算「上次用过的选择」，换页再回来仍是这条
    rememberWorkflowId(workflowId.value)
    positive.value = ''
    negative.value = ''
    title.value = ''
    showTaskSubmissionMessage(router, `任务 #${data.id} 已提交`, '查看任务', `/tasks/${data.id}`)
  } catch (e) {
    message.value = e instanceof Error ? e.message : '提交失败'
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
        <el-select id="workflow" v-model="workflowId" :empty-values="[0]" placeholder="请选择工作流" :disabled="loadingWorkflows || !workflows.length" filterable popper-class="bounded-select-options">
          <el-option v-for="item in workflows" :key="item.id" :value="item.id" :label="item.name" />
        </el-select>
        <ViewFeedback v-if="loadingWorkflows" kind="loading" message="正在加载工作流" />
        <div v-else-if="workflowError" class="form-hint">
          工作流加载失败。<el-button type="button" text size="small" @click="loadWorkflows">重试</el-button>
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
        <el-input id="title" type="text" v-model="title" placeholder="给这条提示词起个名字，方便在提示词库查找" />
      </div>

      <div class="form-group">
        <label for="positive">正向提示词</label>
        <el-input type="textarea" id="positive" v-model="positive" :rows="8" placeholder="描述你想要生成的图像内容..." />
      </div>

      <div class="form-group">
        <label for="negative">负面提示词 <span class="text-xs muted">(可选，留空则用工作流默认)</span></label>
        <el-input type="textarea" id="negative" v-model="negative" :rows="4" placeholder="描述你想要避免的内容，如 blurry / extra fingers / watermark..." />
      </div>

      <div class="btn-group" style="margin-top:24px;">
        <el-button type="primary" :disabled="busy || !workflowId || !positive.trim()" @click="submit">
          <span v-if="busy" class="spinner"></span>
          {{ busy ? '提交中...' : '提交作图' }}
        </el-button>
      </div>

      <ViewFeedback kind="error" :message="message" />
    </div>
  </div>
</template>
