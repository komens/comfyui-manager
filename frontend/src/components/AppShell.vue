<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'
import { api } from '../api/client'

const route = useRoute()
const sidebarOpen = ref(false)
// 版本号由后端 ldflags 注入，只有 /api/version 拿得到（前端本身不打包版本）
const version = ref('')
// 调试入口只在后端 DEBUG 开启时出现（接口 404 即视为未开启）
const debugOn = ref(false)

onMounted(async () => {
  try {
    const data = await api<{ version: string }>('/api/version')
    version.value = data.version
  } catch {
    // 拿不到就不显示，不影响其它功能
  }
  try {
    const data = await api<{ on: boolean }>('/api/debug/status')
    debugOn.value = !!data.on
  } catch {
    // 未开启 DEBUG 时接口不存在，属正常状态
  }
})

const baseItems = [
  { path: '/', label: '概览', icon: '&#9633;' },
  { path: '/submit', label: '直接提交', icon: '&#9998;' },
  { path: '/prompts', label: '提示词库', icon: '&#128221;' },
  { path: '/json-files', label: 'JSON 文件', icon: '&#128196;' },
  { path: '/workflows', label: '工作流', icon: '&#9881;' },
  { path: '/tasks', label: '任务列表', icon: '&#9854;' },
  { path: '/gallery', label: '图片库', icon: '&#128247;' },
]

// 系统区永远有「设置」；调试项按需追加，开启 DEBUG 后才出现
const systemItems = computed(() => {
  const items = [{ path: '/settings', label: '设置', icon: '&#9881;' }]
  if (debugOn.value) {
    items.push({ path: '/debug', label: '调试日志', icon: '&#128295;' })
  }
  return items
})

function closeSidebar() { sidebarOpen.value = false }
</script>

<template>
  <!-- Mobile Header -->
  <div class="mobile-header">
    <button class="hamburger" @click="sidebarOpen = !sidebarOpen">&#9776;</button>
    <strong>ComfyUI Server</strong>
    <div style="width:36px"></div>
  </div>

  <!-- Sidebar Overlay (mobile) -->
  <div v-if="sidebarOpen" class="sidebar-overlay" @click="closeSidebar"></div>

  <div class="app-layout">
    <!-- Sidebar -->
    <aside class="sidebar" :class="{ open: sidebarOpen }">
      <div class="brand">
        <div class="brand-icon">C</div>
        <div class="brand-text">
          <strong>ComfyUI</strong>
          <small>作图服务控制台</small>
        </div>
      </div>

      <nav class="nav-section">
        <div class="nav-label">功能</div>
        <RouterLink
          v-for="item in baseItems"
          :key="item.path"
          :to="item.path"
          class="nav-link"
          @click="closeSidebar"
        >
          <span class="nav-icon" v-html="item.icon"></span>
          {{ item.label }}
        </RouterLink>

        <div class="nav-label">系统</div>
        <RouterLink
          v-for="item in systemItems"
          :key="item.path"
          :to="item.path"
          class="nav-link"
          @click="closeSidebar"
        >
          <span class="nav-icon" v-html="item.icon"></span>
          {{ item.label }}
        </RouterLink>
      </nav>

      <div class="sidebar-footer">
        ComfyUI Server{{ version ? ` v${version}` : '' }}
      </div>
    </aside>

    <!-- Main Content -->
    <main class="content">
      <RouterView />
    </main>
  </div>
</template>
