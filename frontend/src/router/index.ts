import { createRouter, createWebHistory } from 'vue-router'
import DashboardView from '../views/DashboardView.vue'
import DirectSubmitView from '../views/DirectSubmitView.vue'
import JsonFilesView from '../views/JsonFilesView.vue'
import WorkflowsView from '../views/WorkflowsView.vue'
import SettingsView from '../views/SettingsView.vue'
import TaskDetailView from '../views/TaskDetailView.vue'
import JsonFileDetailView from '../views/JsonFileDetailView.vue'
import GalleryView from '../views/GalleryView.vue'

export default createRouter({ history: createWebHistory(), routes: [
  { path: '/', component: DashboardView },
  { path: '/submit', component: DirectSubmitView },
  { path: '/json-files', component: JsonFilesView },
  { path: '/workflows', component: WorkflowsView },
  { path: '/settings', component: SettingsView },
  { path: '/tasks/:id', component: TaskDetailView },
  { path: '/json-files/:id', component: JsonFileDetailView },
  { path: '/gallery', component: GalleryView },
] })
