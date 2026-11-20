<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{
  total: number
  page: number
  pageSize: number
}>()

const emit = defineEmits<{
  (e: 'update:page', page: number): void
}>()

const startItem = computed(() => (props.page - 1) * props.pageSize + 1)
const endItem = computed(() => Math.min(props.page * props.pageSize, props.total))
const layout = computed(() => props.total > props.pageSize ? 'prev, pager, next, jumper' : 'prev, pager, next')

function commitPageJump(event: KeyboardEvent) {
  const input = event.target
  if (!(input instanceof HTMLInputElement) || input.type !== 'number' || event.isComposing) return

  event.preventDefault()
  input.dispatchEvent(new Event('change', { bubbles: true }))
  input.blur()
}
</script>

<template>
  <div v-if="total > 0" class="pagination" @keydown.enter="commitPageJump">
    <span class="page-info">{{ startItem }}-{{ endItem }} / 共 {{ total }} 条</span>
    <el-pagination
      class="page-tail"
      :layout="layout"
      :total="total"
      :current-page="page"
      :page-size="pageSize"
      :pager-count="5"
      size="small"
      background
      @update:current-page="emit('update:page', $event)"
    />
  </div>
</template>

<style scoped>
.pagination { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 16px; flex-wrap: wrap; }
.page-info { font-size: 13px; color: var(--c-muted); }
.page-tail { flex-wrap: wrap; row-gap: 8px; min-width: 0; }

@media (max-width: 640px) {
  .page-info { font-size: 12px; }
  .page-tail :deep(.el-pagination__jump) { margin-left: 0; }
  .page-tail :deep(.el-pager li),
  .page-tail :deep(.btn-prev),
  .page-tail :deep(.btn-next) { min-width: 30px; height: 32px; }
}
</style>
