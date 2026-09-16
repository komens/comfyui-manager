<script setup lang="ts">
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
    groupName.value = p.group_name
  }
}

async function loadGroups() {
  groups.value = await api<{ group_name: string }[]>('/api/prompts/groups')
}

async function save() {
  if (!positive.value.trim()) { showMessage('请输入正面提示词', 'error'); return }
  busy.value = true
  try {
    const body = JSON.stringify({
      title: title.value,
      description: description.value,
      positive_prompt: positive.value,
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
  <PageHeader :eyebrow="isEdit ? 'EDIT PROMPT' : 'NEW PROMPT'" :title="isEdit ? '编辑提示词' : '新建提示词'" description="维护提示词内容和分组。" />

  <div class="card" style="max-width:760px;">
    <div class="card-body">
      <div class="form-group">
        <label for="p-title">标题</label>
        <input id="p-title" type="text" v-model="title" placeholder="留空则自动取提示词前50字" />
      </div>
      <div class="form-group">
        <label for="p-group">分组</label>
        <input id="p-group" type="text" v-model="groupName" list="group-list" placeholder="例如：人物、风景，或导入的文件名" />
        <datalist id="group-list">
          <option v-for="g in groups" :key="g.group_name" :value="g.group_name" />
        </datalist>
      </div>
      <div class="form-group">
        <label for="p-desc">描述 <span class="text-xs muted">(可选)</span></label>
        <input id="p-desc" type="text" v-model="description" placeholder="补充说明" />
      </div>
      <div class="form-group">
        <label for="p-positive">正面提示词</label>
        <textarea id="p-positive" v-model="positive" rows="10" placeholder="描述你想要生成的图像内容..." />
      </div>
      <div class="btn-group" style="margin-top:20px;">
        <button class="btn btn-primary" :disabled="busy || !positive.trim()" @click="save">
          {{ busy ? '保存中...' : '保存' }}
        </button>
        <button class="btn btn-ghost" @click="backToList">取消</button>
      </div>
    </div>
  </div>

  <!-- Toast 消息 -->
  <Teleport to="body">
    <div v-if="message" class="toast-msg" :class="messageType">{{ message }}</div>
  </Teleport>
</template>

<style scoped>
.toast-msg {
  position: fixed;
  top: 20px;
  right: 20px;
  z-index: 300;
  padding: 10px 18px;
  border-radius: var(--radius-sm);
  font-size: 14px;
  font-weight: 500;
  box-shadow: var(--shadow-lg);
  animation: fadeIn .2s ease;
}
.toast-msg.success { background: var(--c-success-light); color: var(--c-success); border: 1px solid var(--c-success-border); }
.toast-msg.error { background: var(--c-danger-light); color: var(--c-danger); border: 1px solid var(--c-danger-border); }
@keyframes fadeIn { from { opacity: 0; transform: translateY(-8px); } to { opacity: 1; transform: translateY(0); } }
</style>
