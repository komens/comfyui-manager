<script setup lang="ts">
import { computed, ref, watch } from 'vue'

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

// —— 跳转到指定页 ——
// 输入框自己维护一份草稿值：编辑时不能直接绑 props.page，否则父组件还没更新就会被覆盖；
// props.page 变化（翻页、父组件重置到第 1 页）时再同步回来。
const jumpDraft = ref(String(props.page))
watch(() => props.page, p => { jumpDraft.value = String(p) })

function commitJump() {
  // 注意：Number('') 是 0 不是 NaN，清空后失焦不能当成"跳第 1 页"
  const raw = jumpDraft.value.trim()
  const n = Number(raw)
  if (raw === '' || !Number.isFinite(n)) {
    jumpDraft.value = String(props.page)
    return
  }
  // 越界夹到边界而不是忽略：输 999 想看最后一页是很自然的操作
  const target = Math.min(totalPages.value, Math.max(1, Math.floor(n)))
  jumpDraft.value = String(target)
  go(target)
}
</script>

<template>
  <div v-if="total > 0" class="pagination">
    <span class="page-info">{{ startItem }}-{{ endItem }} / 共 {{ total }} 条</span>
    <div class="page-tail">
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
      <!-- 只有一页时跳转没有意义，隐藏 -->
      <label v-if="totalPages > 1" class="page-jump">
        跳至
        <input
          v-model="jumpDraft"
          class="page-jump-input"
          type="number"
          inputmode="numeric"
          :min="1"
          :max="totalPages"
          aria-label="跳转到指定页"
          @keydown.enter.prevent="commitJump"
          @blur="commitJump"
        />
        / {{ totalPages }} 页
      </label>
    </div>
  </div>
</template>

<style scoped>
.pagination { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-top: 16px; flex-wrap: wrap; }
.page-info { font-size: 13px; color: var(--c-muted); }
.page-tail { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.page-btns { display: flex; align-items: center; gap: 4px; flex-wrap: wrap; }
.page-jump { display: inline-flex; align-items: center; gap: 6px; font-size: 13px; color: var(--c-muted); white-space: nowrap; }
/* 全局 input[type="number"] 带 width:100% + padding:10px 14px，特异性 0,2,1；
   这里连 input 标签一起写上，把特异性抬到 0,3,1 才能压过它（原生控件，不涉及 Element）。 */
input[type="number"].page-jump-input {
  box-sizing: border-box;
  width: 58px;
  height: 32px;
  padding: 0 6px;
  text-align: center;
  border-radius: 8px;
  font-size: 13px;
  -moz-appearance: textfield;
  appearance: textfield;
}
input[type="number"].page-jump-input::-webkit-inner-spin-button,
input[type="number"].page-jump-input::-webkit-outer-spin-button { -webkit-appearance: none; margin: 0; }
.page-btn { min-width: 32px; height: 32px; padding: 0 8px; border: 1px solid var(--c-border); background: var(--c-surface); border-radius: 8px; cursor: pointer; font-size: 14px; color: var(--c-text); display: inline-flex; align-items: center; justify-content: center; transition: all var(--transition); }
.page-btn:hover:not(:disabled):not(.active) { background: var(--c-surface-hover); border-color: var(--c-primary); }
.page-btn.active { background: var(--c-primary); border-color: var(--c-primary); color: #fff; font-weight: 600; }
.page-btn:disabled { opacity: 0.4; cursor: not-allowed; }
.page-ellipsis { min-width: 24px; text-align: center; color: var(--c-muted); }
</style>
