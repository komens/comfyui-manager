<script setup lang="ts">
import { ref, watch } from 'vue'

interface ParamDef {
  node_id: string
  field: string
  type?: string
  default?: any
  min?: number
  max?: number
  step?: number
  label?: string
  options?: string[]
  visible?: boolean
}

interface Mapping {
  parameters?: Record<string, ParamDef>
}

const props = defineProps<{
  mapping: Mapping | null
  modelValue: Record<string, any>
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: Record<string, any>): void
}>()

const params = ref<Record<string, any>>({})

watch(() => props.mapping, (mapping) => {
  if (!mapping?.parameters) {
    params.value = {}
    return
  }
  const initial: Record<string, any> = {}
  for (const [key, def] of Object.entries(mapping.parameters)) {
    if (def.visible === false) continue
    initial[key] = props.modelValue[key] ?? def.default ?? getDefaultValue(def.type)
  }
  params.value = initial
}, { immediate: true })

watch(params, (val) => {
  emit('update:modelValue', { ...val })
}, { deep: true })

function getDefaultValue(type?: string) {
  switch (type) {
    case 'integer': return 0
    case 'number': return 0
    case 'boolean': return false
    case 'string': return ''
    case 'seed': return 0
    default: return ''
  }
}

function randomSeed(key: string) {
  params.value[key] = Math.floor(Math.random() * 999999999999999)
}

function getVisibleParams() {
  if (!props.mapping?.parameters) return []
  return Object.entries(props.mapping.parameters).filter(([, def]) => def.visible !== false)
}
</script>

<template>
  <div v-if="getVisibleParams().length > 0" class="param-form">
    <div v-for="[key, def] in getVisibleParams()" :key="key" class="form-group">
      <label :for="`param-${key}`">
        {{ def.label || key }}
        <span v-if="def.type" class="text-xs muted">({{ def.type }})</span>
      </label>

      <!-- Integer -->
      <template v-if="def.type === 'integer'">
        <input
          :id="`param-${key}`"
          type="number"
          v-model.number="params[key]"
          :min="def.min"
          :max="def.max"
          :step="def.step ?? 1"
        />
      </template>

      <!-- Number -->
      <template v-else-if="def.type === 'number'">
        <input
          :id="`param-${key}`"
          type="number"
          v-model.number="params[key]"
          :min="def.min"
          :max="def.max"
          :step="def.step ?? 0.1"
        />
      </template>

      <!-- Seed -->
      <template v-else-if="def.type === 'seed'">
        <div class="seed-input">
          <input
            :id="`param-${key}`"
            type="number"
            v-model.number="params[key]"
            min="0"
          />
          <button class="btn btn-secondary btn-sm" @click="randomSeed(key)" type="button">
            随机
          </button>
        </div>
      </template>

      <!-- Boolean -->
      <template v-else-if="def.type === 'boolean'">
        <label class="checkbox-label">
          <input
            type="checkbox"
            v-model="params[key]"
          />
          {{ def.label || key }}
        </label>
      </template>

      <!-- Select -->
      <template v-else-if="def.type === 'select' && def.options">
        <select :id="`param-${key}`" v-model="params[key]">
          <option v-for="opt in def.options" :key="opt" :value="opt">{{ opt }}</option>
        </select>
      </template>

      <!-- String (default) -->
      <template v-else>
        <input
          :id="`param-${key}`"
          type="text"
          v-model="params[key]"
        />
      </template>

      <p v-if="def.min !== undefined || def.max !== undefined" class="form-hint">
        范围：{{ def.min ?? '无限制' }} ~ {{ def.max ?? '无限制' }}
      </p>
    </div>
  </div>
</template>

<style scoped>
.param-form {
  display: flex;
  flex-direction: column;
  gap: 0;
}

.seed-input {
  display: flex;
  gap: 8px;
  align-items: center;
}

.seed-input input {
  flex: 1;
}
</style>
