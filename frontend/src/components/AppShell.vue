<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RouterLink, RouterView } from 'vue-router'
import { DataBoard, Delete, Document, EditPen, Files, Link, List, Monitor, Picture, SetUp, Setting, Tools } from '@element-plus/icons-vue'
import { api } from '../api/client'

const sidebarOpen = ref(false)
const menuButton = ref<HTMLButtonElement | null>(null)
// 版本号由后端 ldflags 注入，只有 /api/version 拿得到（前端本身不打包版本）
const version = ref('')
// 调试入口只在后端 DEBUG 开启时出现（接口 404 即视为未开启）
const debugOn = ref(false)
// 脚本运行台同理：ENABLE_SCRIPTS 未开启时接口 404，入口不出现
const scriptsOn = ref(false)

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
  try {
    const data = await api<{ enabled: boolean }>('/api/scripts/status')
    scriptsOn.value = !!data.enabled
  } catch {
    // 未开启 ENABLE_SCRIPTS 时接口不存在，属正常状态
  }
})

const baseItems = [
  { path: '/', label: '概览', icon: DataBoard },
  { path: '/submit', label: '直接提交', icon: EditPen },
  { path: '/external', label: '平台同步', icon: Link },
  { path: '/prompts', label: '提示词库', icon: Document },
  { path: '/json-files', label: 'JSON 文件', icon: Files },
  { path: '/workflows', label: '工作流', icon: SetUp },
  { path: '/tasks', label: '任务列表', icon: List },
  { path: '/gallery', label: '图片库', icon: Picture },
]

// 系统区永远有「设置」与「数据整理」；调试与脚本按需追加，对应开关打开后才出现
const systemItems = computed(() => {
  const items = [
    { path: '/settings', label: '设置', icon: Setting },
    { path: '/maintenance', label: '数据整理', icon: Delete },
  ]
  if (scriptsOn.value) {
    items.push({ path: '/scripts', label: '脚本运行台', icon: Monitor })
  }
  if (debugOn.value) {
    items.push({ path: '/debug', label: '调试日志', icon: Tools })
  }
  return items
})

function closeSidebar() {
  if (!sidebarOpen.value) return
  sidebarOpen.value = false
  menuButton.value?.focus()
}
</script>

<template>
  <!-- Mobile Header -->
  <div class="mobile-header">
    <button ref="menuButton" class="hamburger" type="button" aria-label="菜单" aria-controls="main-sidebar" :aria-expanded="sidebarOpen" @click="sidebarOpen = !sidebarOpen">&#9776;</button>
    <strong>素材管理平台</strong>
    <div style="width:36px"></div>
  </div>

  <!-- Sidebar Overlay (mobile) -->
  <div v-if="sidebarOpen" class="sidebar-overlay" @click="closeSidebar"></div>

  <div class="app-layout">
    <!-- Sidebar -->
    <aside id="main-sidebar" class="sidebar" :class="{ open: sidebarOpen }" @keydown.esc="closeSidebar">
      <div class="brand">
        <div class="brand-icon">素</div>
        <div class="brand-text">
          <strong>素材管理平台</strong>
          <small>工作流 · 提示词 · 图片</small>
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
          <component :is="item.icon" class="nav-icon" aria-hidden="true" />
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
          <component :is="item.icon" class="nav-icon" aria-hidden="true" />
          {{ item.label }}
        </RouterLink>
      </nav>

      <div class="sidebar-footer">
        素材管理平台{{ version ? ` v${version}` : '' }}
      </div>
    </aside>

    <!-- Main Content -->
    <main class="content">
      <RouterView />
    </main>
  </div>
</template>
