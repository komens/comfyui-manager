<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import PageHeader from '../components/PageHeader.vue'
import { api, type Task } from '../api/client'
const route = useRoute(); const task = ref<Task | null>(null); const message = ref(''); let stream: EventSource | undefined
async function load() { task.value = await api<Task>(`/api/tasks/${route.params.id}`) }
async function action(path: string) { try { await api(path, { method: 'POST' }); await load(); message.value = '操作已提交' } catch (e) { message.value = e instanceof Error ? e.message : '操作失败' } }
onMounted(async () => { await load(); stream = new EventSource(`/api/tasks/${route.params.id}/events`); stream.onmessage = async () => load(); stream.onerror = () => { message.value = '实时连接已断开，可手动刷新' } })
onUnmounted(() => stream?.close())
</script>
<template><PageHeader eyebrow="TASK DETAIL" :title="`任务 #${route.params.id}`" description="实时查看提交、生成和下载状态。" /><section v-if="task" class="panel"><div class="task-summary"><strong>{{ task.status }}</strong><span>{{ task.success_count }} / {{ task.total_count }} 成功，{{ task.failed_count }} 失败</span></div><div class="actions"><button v-if="task.status === 'pending' || task.status === 'running'" class="secondary" @click="action(`/api/tasks/${task.id}/cancel`)">取消任务</button><button v-if="task.status === 'failed'" @click="action(`/api/tasks/${task.id}/retry`)">重试</button></div><pre class="payload">{{ JSON.stringify(task.parameters, null, 2) }}</pre><p class="status">{{ message }}</p></section><section v-else class="panel muted">正在读取任务...</section></template>
