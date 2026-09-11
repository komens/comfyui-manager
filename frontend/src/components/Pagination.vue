<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(defineProps<{
  total: number
  page: number
  pageSize: number
}>(), {})

const emit = defineEmits<{
  (e: 'update:page', page: number): void
}>()

const totalPages = computed(() => Math.max(1, Math.ceil(props.total / props.pageSize)))

// 生成页码按钮（含省略号）
const pages = computed<(number | '...')[]>(() => {
  const tp = totalPages.value
  const cur = props.page
  if (tp <= 7) {
    return Array.from({ length: tp }, (_, i) => i + 1)
  }
  const result: (number | '...')[] = [1]
  const start = Math.max(2, cur - 1)
  const end = Math.min(tp - 1, cur + 1)
  if (start > 2) result.push('...')
  for (let i = start; i <= end; i++) result.push(i)
  if (end < tp - 1) result.push('...')
  result.push(tp)
  return result
})

const startItem = computed(() => props.total === 0 ? 0 : (props.page - 1) * props.pageSize + 1)
const endItem = computed(() => Math.min(props.page * props.pageSize, props.total))

function go(p: number) {
  if (p < 1 || p > totalPages.value || p === props.page) return
  emit('update:page', p)
}
</script>

<template>
  <div v-if="total > 0" class="pagination">
    <span class="page-info">{{ startItem }}-{{ endItem }} / 共 {{ total }} 条</span>
    <div class="page-btns">
      <button class="page-btn" :disabled="page <= 1" @click="go(page - 1)" aria-label="上一页">&lsaquo;</button>
      <template v-for="(p, i) in pages" :key="i">
        <span v-if="p === '...'" class="page-ellipsis">…</span>
        <button
          v-else
          class="page-btn"
          :class="{ active: p === page }"
          @click="go(p as number)"
        >{{ p }}</button>
      </template>
      <button class="page-btn" :disabled="page >= totalPages" @click="go(page + 1)" aria-label="下一页">&rsaquo;</button>
    </div>
  </div>
</template>

<style scoped>
.pagination { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 16px; flex-wrap: wrap; }
.page-info { font-size: 13px; color: var(--c-muted); }
.page-btns { display: flex; align-items: center; gap: 4px; }
.page-btn { min-width: 32px; height: 32px; padding: 0 8px; border: 1px solid var(--c-border); background: var(--c-surface); border-radius: 8px; cursor: pointer; font-size: 14px; color: var(--c-text); display: inline-flex; align-items: center; justify-content: center; transition: all var(--transition); }
.page-btn:hover:not(:disabled):not(.active) { background: var(--c-surface-hover); border-color: var(--c-primary); }
.page-btn.active { background: var(--c-primary); border-color: var(--c-primary); color: #fff; font-weight: 600; }
.page-btn:disabled { opacity: 0.4; cursor: not-allowed; }
.page-ellipsis { min-width: 24px; text-align: center; color: var(--c-muted); }
</style>
