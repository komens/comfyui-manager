# ComfyUI 可视化作图服务

基于 Vue 3 + Go + SQLite 的 ComfyUI 作图管理工具，适用于个人 NAS 环境。支持提示词管理、批量提交、任务跟踪、图片浏览和工作流配置。

## 功能概览

- **提示词库** - 统一管理所有提示词，支持分组、搜索、分页、批量操作（删除/重跑）
- **直接作图** - 输入正向/负向提示词，选择工作流直接生成
- **工作流管理** - 保存 ComfyUI workflow JSON，自动识别参数，支持可视化编辑
- **任务列表** - 查看所有任务状态，支持取消排队中的任务（同步通知 ComfyUI 删除队列）
- **图片库** - 网格浏览、灯箱预览（左右分栏）、查看提示词、下载原图
- **JSON 文件** - 上传提示词 JSON 自动入库，按文件名分组，支持去重
- **负面提示词** - 绑定到工作流，每个工作流一份通用负面提示词
- **ComfyUI UI 格式兼容** - 自动将 ComfyUI UI 导出格式转换为 API 格式
- **断点恢复** - 服务重启后自动恢复未完成任务

## 环境要求

- **Go** 1.21+（推荐 1.23）
- **Node.js** 18+
- **ComfyUI** 实例（可访问的内网地址）
- **Docker**（可选，用于容器化部署）

## 本地开发

### 1. 启动后端

必须在 **`backend/` 目录**下运行。`DATA_DIR` 默认 `../data`，即项目根目录下的 `data/`：

```bash
cd backend
go run ./cmd/server
```

后端默认监听 `8080` 端口，首次启动会自动创建 SQLite 数据库和 data 目录。

可选环境变量：

```bash
SERVER_PORT=8080        # 监听端口，默认 8080
DATA_DIR=../data        # 数据目录，默认 ../data（相对启动目录，即项目根目录的 data/）
COMFYUI_URL=http://127.0.0.1:8188  # ComfyUI 地址
```

> `DATA_DIR` 是相对**启动时的工作目录**解析的。请固定从 `backend/` 目录启动；
> 若必须从其他目录启动，请显式传入绝对路径的 `DATA_DIR`，否则会读写到错误的目录。

### 2. 启动前端

```bash
cd frontend
npm install
npm run dev
```

前端开发服务运行在 `http://localhost:5173`，已配置 Vite 代理将 `/api` 请求转发到后端 `localhost:8080`。

### 3. 访问应用

打开浏览器访问 `http://localhost:5173`。

### 开发启动流程说明

```
┌─────────────┐     /api/* 请求      ┌─────────────┐       ┌─────────────┐
│   浏览器     │ ──────────────────→ │  Vite Dev    │ ────→ │  Go 后端     │
│ localhost:5173│ ←────────────────── │  Server:5173 │ ←──── │  :8080      │
└─────────────┘     代理转发响应      └─────────────┘       └──────┬──────┘
                                                                  │
                                                           ┌──────┴──────┐
                                                           │   SQLite    │
                                                           │  data/db/   │
                                                           └─────────────┘
```

1. **后端** (`go run ./cmd/server`) 启动后监听 8080 端口，自动初始化数据库表（`IF NOT EXISTS`，不会覆盖已有数据）
2. **前端** (`npm run dev`) 启动 Vite 开发服务器在 5173 端口，通过 `vite.config.ts` 中的 proxy 配置将 `/api` 请求转发到后端
3. 前端所有 API 请求（`/api/*`）由 Vite 代理转发到后端处理
4. 后端 `data/` 目录存放数据库、图片、JSON 备份等运行时数据

### 首次使用流程

1. 启动后端和前端
2. 打开「设置」页面，配置 ComfyUI 地址并测试连接
3. 打开「工作流」页面，新增工作流（粘贴 ComfyUI 导出的 JSON，点击「自动识别参数」）
4. 打开「直接提交」页面，输入提示词选择工作流开始作图
5. 或者打开「JSON 文件」页面，导入提示词 JSON 文件批量入库
6. 在「提示词库」中管理和批量执行提示词

## Docker 部署

### 快速开始

```bash
# 克隆项目后进入目录
cd comfyui-server

# 设置 ComfyUI 地址
export COMFYUI_URL=http://192.168.1.20:8188

# 构建并启动
docker compose up -d --build
```

访问 `http://localhost:8080` 即可使用。

### 构建说明

项目使用多阶段构建，前后端打包成单个 Docker 镜像：

1. **Stage 1** - `node:18-alpine` 构建前端（Vite build）
2. **Stage 2** - `golang:1.23` 构建后端（CGO_ENABLED=0 静态编译）
3. **Stage 3** - `distroless` 最终镜像，包含后端二进制 + 前端静态文件

### 🐳 NAS Docker 部署

如果需要在 NAS 上部署，可以使用以下命令：

```bash
# 构建镜像（NAS 部署）
docker build --platform linux/amd64 -t comfyui-server:1.0.0 .

# 运行容器（NAS 部署）
docker run -p 8080:8080 -v comfyui_server_data:/app/data comfyui-server:1.0.0

# 导出镜像（用于 NAS 部署）
docker save comfyui-server:1.0.0 -o comfyui-server.tar
```

**说明：**
- `--platform linux/amd64`：指定构建平台为 x86_64 架构（适用于大多数 NAS）
- `-p 8080:8080`：将容器的 8080 端口映射到主机的 8080 端口
- `-v comfyui_server_data:/app/data`：使用 Docker 卷挂载数据目录，持久化数据库和图片
- `docker save`：导出镜像为 tar 文件，方便传输到 NAS 设备

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `COMFYUI_URL` | `http://127.0.0.1:8188` | ComfyUI 服务地址（首次写入数据库） |
| `SERVER_PORT` | `8080` | 后端监听端口 |
| `DATA_DIR` | `/app/data` | 数据存储目录 |
| `DB_PATH` | `/app/data/db/comfyui.db` | SQLite 数据库路径 |
| `STATIC_DIR` | `/app/static` | 前端静态文件目录（Docker 内置） |

### 数据目录

```
data/
├── db/comfyui.db      # SQLite 数据库
├── images/            # 生成的图片
└── json/              # 上传的 JSON 文件备份
```

### 常用命令

```bash
# 查看日志
docker compose logs -f

# 停止服务
docker compose down

# 重新构建
docker compose up -d --build
```

## 项目结构

```
comfyui-server/
├── Dockerfile                 # 多阶段构建（前后端统一镜像）
├── docker-compose.yml
├── backend/
│   ├── go.mod
│   └── cmd/server/
│       ├── main.go            # 入口、路由、配置、数据库初始化、静态文件服务
│       ├── comfyui.go         # ComfyUI 客户端、格式转换、提示词注入、图片下载
│       ├── workflows.go       # 工作流 CRUD、参数自动识别
│       ├── tasks.go           # 任务创建、查询、分页
│       ├── prompts.go         # 提示词 CRUD、批量操作、分组
│       ├── json.go            # JSON 文件上传、解析、入库
│       ├── images.go          # 图片列表、详情、下载、删除（分页）
│       ├── events.go          # SSE 事件推送、任务取消（含 ComfyUI 队列删除）
│       └── ...
├── frontend/
│   ├── vite.config.ts         # Vite 配置，/api 代理到 localhost:8080
│   └── src/
│       ├── api/client.ts      # API 请求封装
│       ├── components/
│       │   ├── AppShell.vue   # 布局框架（侧栏导航）
│       │   ├── PageHeader.vue # 页面标题组件
│       │   ├── Pagination.vue # 通用分页组件
│       │   └── ParamForm.vue  # 动态参数表单
│       ├── views/
│       │   ├── DashboardView.vue    # 概览首页
│       │   ├── DirectSubmitView.vue # 直接提交
│       │   ├── PromptsView.vue      # 提示词库（分页、多选、批量操作）
│       │   ├── PromptDetailView.vue # 提示词详情
│       │   ├── PromptEditView.vue   # 提示词编辑
│       │   ├── WorkflowsView.vue    # 工作流管理（分页）
│       │   ├── TaskListView.vue     # 任务列表（分页、状态筛选）
│       │   ├── TaskDetailView.vue   # 任务详情（SSE 实时进度）
│       │   ├── GalleryView.vue      # 图片库（分页、灯箱预览）
│       │   ├── JsonFilesView.vue    # JSON 文件管理（分页）
│       │   └── SettingsView.vue     # 设置页
│       ├── router/index.ts    # 路由配置
│       ├── style.css          # 全局设计系统样式
│       └── main.ts            # 入口
└── data/                      # 运行时数据（自动创建，已在 .gitignore）
```

## API 接口

所有列表接口统一返回分页格式：

```json
{
  "items": [],
  "total": 100,
  "page": 1,
  "page_size": 20
}
```

支持 `page` 和 `page_size` 查询参数（page_size 范围 1-200，默认 20）。

### 提示词

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/prompts` | 列表（支持 group/status/q 筛选） |
| POST | `/api/prompts` | 创建 |
| GET | `/api/prompts/groups` | 分组列表 |
| GET | `/api/prompts/:id` | 详情 |
| PUT | `/api/prompts/:id` | 编辑 |
| DELETE | `/api/prompts/:id` | 删除 |
| POST | `/api/prompts/batch-delete` | 批量删除 |
| POST | `/api/prompts/batch-run` | 批量重跑 |
| POST | `/api/prompts/group-run` | 分组重跑 |
| POST | `/api/prompts/:id/run` | 单个重跑 |

### 工作流

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/workflows` | 列表 |
| POST | `/api/workflows` | 创建 |
| GET | `/api/workflows/:id` | 详情 |
| PUT | `/api/workflows/:id` | 编辑 |
| DELETE | `/api/workflows/:id` | 删除 |
| POST | `/api/workflows/detect-params` | 自动识别参数 |

### 任务

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/tasks/direct` | 直接提交 |
| GET | `/api/tasks` | 列表（支持 status 筛选） |
| GET | `/api/tasks/:id` | 详情 |
| POST | `/api/tasks/:id/cancel` | 取消（同步删除 ComfyUI 队列） |
| POST | `/api/tasks/:id/retry` | 重试 |
| GET | `/api/tasks/:id/events` | SSE 进度流 |

### 图片

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/images` | 列表 |
| GET | `/api/images/:id` | 详情（含提示词） |
| GET | `/api/images/:id/file` | 图片文件 |
| GET | `/api/images/:id/download` | 下载 |
| DELETE | `/api/images/:id` | 删除 |

### JSON 文件

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/json-files` | 列表 |
| POST | `/api/json-files/upload` | 上传（自动解析入库 + 去重） |
| GET | `/api/json-files/:id` | 详情（含内容） |
| GET | `/api/json-files/:id/download` | 下载 |
| DELETE | `/api/json-files/:id` | 删除（不影响已入库提示词） |

### 系统

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/health` | 健康检查 |
| GET | `/api/stats` | 统计数据 |
| GET | `/api/settings` | 获取设置 |
| PUT | `/api/settings/comfyui` | 更新 ComfyUI 地址 |
| POST | `/api/settings/comfyui/test` | 测试连接 |
