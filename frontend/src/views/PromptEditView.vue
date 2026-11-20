<script setup lang="ts">
import ViewFeedback from '../components/ViewFeedback.vue'
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api } from '../api/client'
import { backToListOr } from '../utils/nav'

const route = useRoute()
const router = useRouter()
const title = ref('')
const description = ref('')
const positive = ref('')
const negative = ref('')
const groupName = ref('')
const groups = ref<{ group_name: string }[]>([])
const busy = ref(false)
const isEdit = ref(false)
const message = ref('')
const messageType = ref<'success' | 'error'>('success')

function showMessage(text: string, type: 'success' | 'error' = 'success') {
  message.value = text
  messageType.value = type
  setTimeout(() => { message.value = '' }, 5000)
}

async function load() {
  if (route.params.id) {
    isEdit.value = true
    const p = await api<any>(`/api/prompts/${route.params.id}`)
    title.value = p.title
    description.value = p.description
    positive.value = p.positive_prompt
    negative.value = p.negative_prompt ?? ''
    groupName.value = p.group_name
  }
}

async function loadGroups() {
  groups.value = await api<{ group_name: string }[]>('/api/prompts/groups')
}

function suggestGroups(query: string, done: (items: { value: string }[]) => void) {
  const keyword = query.trim().toLowerCase()
  done(groups.value
    .filter(group => group.group_name.toLowerCase().includes(keyword))
    .map(group => ({ value: group.group_name })))
}

async function save() {
  if (!positive.value.trim()) { showMessage('请输入正面提示词', 'error'); return }
  busy.value = true
  try {
    const body = JSON.stringify({
      title: title.value,
      description: description.value,
      positive_prompt: positive.value,
      negative_prompt: negative.value,
      group_name: groupName.value,
    })
    if (isEdit.value) {
      await api(`/api/prompts/${route.params.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body,
      })
    } else {
      await api('/api/prompts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body,
      })
    }
    // 返回时优先走历史（保留列表页的分页/筛选 query），直链进入则回列表第一页
    backToList()
  } catch (e) {
    showMessage(e instanceof Error ? e.message : '保存失败', 'error')
  } finally {
    busy.value = false
  }
}

function backToList() {
  backToListOr(router, '/prompts')
}

onMounted(() => {
  load()
  loadGroups()
})
</script>

<template>
  <PageHeader :title="isEdit ? '编辑提示词' : '新建提示词'" description="维护提示词内容和分组。" />

  <ViewFeedback :kind="messageType" :message="message" />

  <div class="card" style="max-width:760px;">
    <div class="card-body">
      <div class="form-group">
        <label for="p-title">标题</label>
        <el-input id="p-title" type="text" v-model="title" placeholder="留空则自动取提示词前50字" />
      </div>
      <div class="form-group">
        <label for="p-group">分组</label>
        <el-autocomplete
          id="p-group"
          v-model="groupName"
          :fetch-suggestions="suggestGroups"
          popper-class="bounded-select-options"
          style="width:100%;"
          placeholder="例如：人物、风景，或导入的文件名"
        />
      </div>
      <div class="form-group">
        <label for="p-desc">描述 <span class="text-xs muted">(可选)</span></label>
        <el-input id="p-desc" type="text" v-model="description" placeholder="补充说明" />
      </div>
      <div class="form-group">
        <label for="p-positive">正面提示词</label>
        <el-input type="textarea" id="p-positive" v-model="positive" :rows="10" placeholder="描述你想要生成的图像内容..." />
      </div>
      <div class="form-group">
        <label for="p-negative">负面提示词 <span class="text-xs muted">(可选)</span></label>
        <el-input type="textarea" id="p-negative" v-model="negative" :rows="4" placeholder="描述你想要避免的内容；提交时若为空则使用所属工作流的默认负面" />
      </div>
      <div class="btn-group" style="margin-top:20px;">
        <el-button type="primary" :disabled="busy || !positive.trim()" @click="save">
          {{ busy ? '保存中...' : '保存' }}
        </el-button>
        <el-button text @click="backToList">取消</el-button>
      </div>
    </div>
  </div>

</template>

<style scoped>
@keyframes fadeIn { from { opacity: 0; transform: translateY(-8px); } to { opacity: 1; transform: translateY(0); } }
</style>
