<script setup lang="ts">
import { ref } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'

const route = useRoute()
const sidebarOpen = ref(false)

const navItems = [
  { path: '/', label: '概览', icon: '&#9633;' },
  { path: '/submit', label: '直接提交', icon: '&#9998;' },
  { path: '/prompts', label: '提示词库', icon: '&#128221;' },
  { path: '/json-files', label: 'JSON 文件', icon: '&#128196;' },
  { path: '/workflows', label: '工作流', icon: '&#9881;' },
  { path: '/tasks', label: '任务列表', icon: '&#9854;' },
  { path: '/gallery', label: '图片库', icon: '&#128247;' },
  { path: '/settings', label: '设置', icon: '&#9881;' },
]

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
          v-for="item in navItems.slice(0, 7)"
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
          v-for="item in navItems.slice(7)"
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
        ComfyUI Server v0.1
      </div>
    </aside>

    <!-- Main Content -->
    <main class="content">
      <RouterView />
    </main>
  </div>
</template>
