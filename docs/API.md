# comfyui-server 接口文档

> 后端：Go（`net/http` + `modernc.org/sqlite`）｜前端：Vue 3
> 文档基于源码逐字段核对（`backend/cmd/server/`），共 **57 个接口**（54 个常驻 + 3 个仅 `DEBUG` 开启时注册）。
> 最后更新：2026-10-01

---

## 目录

- [1. 概述](#1-概述)
- [2. 快速开始](#2-快速开始)
- [3. 接口总览](#3-接口总览)
- [4. 系统类接口](#4-系统类接口)
- [5. 工作流接口](#5-工作流接口)
- [6. 任务接口](#6-任务接口)
- [7. 提示词接口](#7-提示词接口)
- [8. JSON 导入接口](#8-json-导入接口)
- [9. 图片接口](#9-图片接口)
- [10. 数据导入导出](#10-数据导入导出)
- [11. 附录：错误码与行为细节](#11-附录错误码与行为细节)

---

## 1. 概述

### 1.1 基础信息

| 项 | 值 |
|---|---|
| 默认端口 | `8080`（环境变量 `SERVER_PORT`） |
| Base URL | `http://<host>:8080` |
| 路径前缀 | `/api` |
| 数据格式 | JSON（图片 / 备份 / 导出为二进制流） |
| 字符集 | `application/json; charset=utf-8` |
| 鉴权 | **无**。服务本身不做认证，建议只在内网或反向代理后暴露 |

前端静态文件由同一个服务托管（设置 `STATIC_DIR` 时）；非 `/api` 路径命中不到文件时会回退到 `index.html`（SPA 路由）。

### 1.2 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DATA_DIR` | `../data` | 数据根目录（**按启动时的 cwd 解析**，建议用绝对路径） |
| `DB_PATH` | `<DATA_DIR>/db/comfyui.db` | SQLite 数据库路径 |
| `COMFYUI_URL` | `http://127.0.0.1:8188` | 首次初始化时写入 settings 的 ComfyUI 地址 |
| `SERVER_PORT` | `8080` | 监听端口 |
| `STATIC_DIR` | 空 | 前端静态目录；空则不启用静态托管 |
| `DEBUG` | 关闭 | 调试总开关。开启后把排查线索落盘到 `<DATA_DIR>/debug/`（4 个文件），并注册 3 个 `/api/debug/*` 只读接口。取值：`1` / `true` / `on` / `yes` 开启；**其它任意值也开启，且该值作为 `submit-payloads.jsonl` 的自定义路径**；空 / `off` / `0` / `false` / `no` 关闭 |

数据目录结构：

```
data/
├── db/comfyui.db          # SQLite
├── images/                # 生成的图片
├── workflows/             # 导入的工作流 JSON
├── json/                  # JSON 导入的原始备份
└── debug/                 # 仅 DEBUG 开启时存在（见 4.11）
```

### 1.3 通用约定

**成功响应**：直接返回业务 JSON，无信封结构。

**错误响应**：所有错误统一为

```json
{ "error": "错误消息" }
```

**分页**：列表类接口统一支持 `page` / `page_size` 两个 query 参数。

| 参数 | 类型 | 默认 | 规则 |
|---|---|---|---|
| `page` | int | `1` | 解析失败或 `< 1` 时按 1 处理 |
| `page_size` | int | `20` | 解析失败、`< 1` 或 `> 200` 时按 20 处理 |

响应统一附带 `page`、`page_size` 回显。

**请求体限制**：

| 场景 | 上限 |
|---|---|
| 普通 JSON body | 1 MiB |
| `POST /api/json-files/import-text` | 10 MiB |
| `POST /api/json-files/upload`（multipart） | 50 MiB |
| `POST /api/import`（multipart） | 500 MiB |

> ⚠️ **JSON body 拒绝未知字段**（`DisallowUnknownFields`）。多传一个结构体里没声明的字段会导致解析失败，
> 但 `POST /api/tasks/direct` 这类接口只会返回 `workflow_id and positive_prompt are required`，
> 容易被误读成「缺必填」。排查时请先确认没有多余字段。

**批量接口**：请求体统一为 `{"ids": [...]}`，返回 `{"applied": N, "skipped": M}`。

| 约定 | 说明 |
|---|---|
| 单个请求 id 上限 | 500（超出 → 400 `too many ids, max 500 per request`） |
| 「不存在」与「状态不允许」 | 计入 `skipped`，**不返回 404 / 409** |
| 服务端故障 | 立即中断，返回 500 `batch operation failed` |

> ⚠️ 例外：`POST /api/prompts/batch-delete` 与 `POST /api/prompts/batch-run` 未走统一批量流程，**不校验 500 上限**。

**CORS**：中间件只在来源命中白名单时才回显跨域头。当前代码里白名单**始终为空**（配置项未被赋值），因此实际**不输出任何 CORS 头**——跨域访问需要自行加反向代理。任何 `OPTIONS` 请求返回 `204`。

---

## 2. 快速开始

一个完整的最小流程：导入工作流 → 确认提示词落点 → 提交 → 查看任务。

```bash
BASE=http://127.0.0.1:8080

# 1) 导入工作流（落点会自动识别，无需手工配置）
curl -s -X POST $BASE/api/workflows \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"我的工作流\",\"workflow_json\":$(cat my-workflow.json)}"
# → 201 {"id":8,"name":"我的工作流","workflow_path":"我的工作流-8.json",
#        "mapping":{"positive_prompt":{"node_id":"6","field":"text"},
#                   "negative_prompt":{"node_id":"7","field":"text"}},
#        "negative_prompt":""}
# workflow_json 也可以直接给导出文件的原文（外层引号会被自动剥掉），
# 效果与上面完全一致：
#   python3 -c 'import json,sys;print(json.dumps({"name":"我的工作流",
#       "workflow_json":open(sys.argv[1],encoding="utf-8").read()}))' my-workflow.json \
#     | curl -s -X POST $BASE/api/workflows -H 'Content-Type: application/json' -d @-

# 2) 查看自动识别出的提示词落点与候选节点
curl -s $BASE/api/workflows/8/prompt-targets

# 3) 设置负向提示词（可选，存在 workflow 上）
curl -s -X PUT $BASE/api/workflows/8 \
  -H 'Content-Type: application/json' \
  -d '{"negative_prompt":"worst quality, blurry"}'

# 4) 直接提交一条提示词
curl -s -X POST $BASE/api/tasks/direct \
  -H 'Content-Type: application/json' \
  -d '{"workflow_id":8,"positive_prompt":"a cat, cinematic lighting"}'
# → 202 {"id":11,"prompt_id":9,"status":"pending","comfyui_url":"http://127.0.0.1:8188"}

# 5) 查询任务（含每个 item 的状态与结果图）
curl -s $BASE/api/tasks/11
```

任务创建后由后台协程异步提交给 ComfyUI，**不会立刻出图**。两种跟踪方式：

- 轮询 `GET /api/tasks/{id}`；
- 或订阅 `GET /api/tasks/{id}/events`（SSE 实时推送）。

---

## 3. 接口总览

| # | 方法 | 路径 | 说明 |
|---|---|---|---|
| **系统** | | | |
| 1 | GET | `/api/health` | 健康检查 |
| 2 | GET | `/api/version` | 版本号 |
| 3 | GET | `/api/stats` | 汇总统计 |
| 4 | GET | `/api/settings` | 读取设置 |
| 5 | PUT | `/api/settings/comfyui` | 保存 ComfyUI 地址 |
| 6 | POST | `/api/settings/comfyui/test` | 测试 ComfyUI 连通性 |
| 7 | GET | `/api/settings/backup` | 下载数据库备份 |
| **工作流** | | | |
| 8 | GET | `/api/workflows` | 工作流列表 |
| 9 | POST | `/api/workflows` | 新建工作流 |
| 10 | GET | `/api/workflows/{id}` | 工作流详情 |
| 11 | PUT | `/api/workflows/{id}` | 更新工作流 |
| 12 | DELETE | `/api/workflows/{id}` | 删除工作流 |
| 13 | PUT | `/api/workflows/{id}/default` | 设置 / 取消默认工作流 |
| 14 | POST | `/api/workflows/prompt-targets` | 识别落点（未保存的工作流） |
| 15 | GET | `/api/workflows/{id}/prompt-targets` | 识别落点 + 已存映射 |
| 16 | POST | `/api/workflows/validate` | 校验（未保存） |
| 17 | POST | `/api/workflows/{id}/validate` | 校验（已保存） |
| **任务** | | | |
| 18 | POST | `/api/tasks/direct` | 直接提交提示词 |
| 19 | GET | `/api/tasks` | 任务列表 |
| 20 | GET | `/api/tasks/{id}` | 任务详情 |
| 21 | GET | `/api/tasks/{id}/events` | 任务事件（SSE） |
| 22 | POST | `/api/tasks/{id}/cancel` | 取消任务 |
| 23 | POST | `/api/tasks/{id}/retry` | 重试任务 |
| 24 | DELETE | `/api/tasks/{id}` | 删除任务 |
| 25 | POST | `/api/tasks/batch-cancel` | 批量取消 |
| 26 | POST | `/api/tasks/batch-retry` | 批量重试 |
| 27 | POST | `/api/tasks/batch-delete` | 批量删除 |
| **提示词** | | | |
| 28 | GET | `/api/prompts` | 提示词列表 |
| 29 | POST | `/api/prompts` | 新建提示词 |
| 30 | GET | `/api/prompts/groups` | 分组列表 |
| 31 | GET | `/api/prompts/{id}` | 提示词详情 |
| 32 | PUT | `/api/prompts/{id}` | 更新提示词 |
| 33 | DELETE | `/api/prompts/{id}` | 删除提示词 |
| 34 | PATCH | `/api/prompts/{id}/favorite` | 切换收藏 |
| 35 | POST | `/api/prompts/batch-delete` | 批量删除 |
| 36 | POST | `/api/prompts/batch-run` | 批量出图 |
| 37 | POST | `/api/prompts/group-run` | 按分组出图 |
| 38 | POST | `/api/prompts/{id}/run` | 单条出图 |
| **JSON 导入** | | | |
| 39 | GET | `/api/json-files` | 备份文件列表 |
| 40 | POST | `/api/json-files/upload` | 上传 JSON 文件 |
| 41 | POST | `/api/json-files/import-text` | 粘贴 JSON 文本导入 |
| 42 | GET | `/api/json-files/{id}` | 查看备份内容 |
| 43 | GET | `/api/json-files/{id}/download` | 下载备份文件 |
| 44 | DELETE | `/api/json-files/{id}` | 删除备份记录 |
| **图片** | | | |
| 45 | GET | `/api/images` | 图片列表 |
| 46 | GET | `/api/images/{id}` | 图片详情 |
| 47 | GET | `/api/images/{id}/file` | 图片文件（内联） |
| 48 | GET | `/api/images/{id}/download` | 图片下载 |
| 49 | DELETE | `/api/images/{id}` | 删除图片 |
| 50 | POST | `/api/images/batch-delete` | 批量删除 |
| 51 | POST | `/api/images/batch-favorite` | 批量收藏 |
| 52 | PATCH | `/api/images/{id}/favorite` | 切换收藏 |
| **导入导出** | | | |
| 53 | GET | `/api/export` | 导出 ZIP |
| 54 | POST | `/api/import` | 导入 ZIP |
| **调试**（**仅 `DEBUG` 开启时注册**，关闭时均 404） | | | |
| 55 | GET | `/api/debug/status` | 调试状态与文件清单 |
| 56 | GET | `/api/debug/files/{name}` | 下载调试文件 |
| 57 | DELETE | `/api/debug/files/{name}` | 清空调试文件 |

---

## 4. 系统类接口

### 4.1 健康检查

```
GET /api/health
```

**响应 200**

```json
{ "status": "ok" }
```

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 200 | 数据库 `Ping` 正常 | — |
| 503 | 数据库不可用 | `database unavailable` |

### 4.2 版本号

```
GET /api/version
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `version` | string | 编译期注入；未注入时为 `dev` |

```json
{ "version": "1.1.0" }
```

### 4.3 汇总统计

```
GET /api/stats
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `total_tasks` | int | 任务总数 |
| `total_images` | int | 图片总数 |
| `pending_entries` | int | 状态为 `pending` 的提示词数 |
| `running_tasks` | int | 状态为 `running` 的任务数 |

> 各统计项查询失败时静默忽略，计数保持 0，接口始终返回 200。

### 4.4 读取设置

```
GET /api/settings
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `comfyui_url` | string | 当前 ComfyUI 地址 |

```json
{ "comfyui_url": "http://192.168.1.10:8188" }
```

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 500 | 读取失败 | 原始数据库错误文本（非固定文案） |

### 4.5 保存 ComfyUI 地址

```
PUT /api/settings/comfyui
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `url` | string | 是 | ComfyUI 地址 |

**URL 规范化规则**：去首尾空格、去尾部 `/`；必须是 `http` / `https`；**必须含端口**；不能含路径、查询串、用户信息。

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `comfyui_url` | string | 规范化后的地址 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | body 非法 | `invalid JSON body` |
| 400 | 格式不合法 | `ComfyUI 地址必须是 http(s)://主机:端口，不能包含路径或查询参数` |
| 400 | 缺端口 | `ComfyUI 地址必须包含端口` |
| 500 | 写库失败 | `save setting failed` |

> 地址变更只影响**后续**提交。已在跑的任务用任务创建时的快照做展示，但实际出站请求一律实时读取当前设置。

### 4.6 测试 ComfyUI 连通性

```
POST /api/settings/comfyui/test?url=http://127.0.0.1:8188
```

**Query 参数**

| 参数 | 必填 | 说明 |
|---|---|---|
| `url` | 否 | 指定则临时测试该地址；省略则测试已保存的地址 |

请求 `GET <url>/system_stats`，超时 5 秒，2xx 视为可达。

**响应 200（可达）**

| 字段 | 类型 | 说明 |
|---|---|---|
| `reachable` | bool | `true` |
| `url` | string | 实际测试的地址 |
| `latency_ms` | int | 耗时（毫秒） |

**响应 502（不可达）**

| 字段 | 类型 | 说明 |
|---|---|---|
| `reachable` | bool | `false` |
| `url` | string | |
| `latency_ms` | int | |
| `error` | string | 错误详情，如 `ComfyUI returned HTTP 502` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `url` 参数格式不合法 | 同 4.5 的中文提示 |
| 500 | 读取已保存设置失败 | 原始数据库错误文本 |

> 这是排查「连不上 ComfyUI」的推荐入口。注意区分三类失败：
> - `connection refused` → 端口无人监听（服务没起）；
> - `context deadline exceeded` → TCP 通但无响应（服务假死或防火墙丢包）；
> - HTTP 400 → 连得上但载荷校验失败（属于工作流问题，不是网络问题）。

### 4.7 下载数据库备份

```
GET /api/settings/backup
```

**响应 200**：直接返回 SQLite 数据库文件流。

| 响应头 | 值 |
|---|---|
| `Content-Type` | `application/octet-stream` |
| `Content-Disposition` | `attachment; filename="comfyui-backup-<YYYYMMDD_HHMMSS>.db"` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 404 | 数据库文件不存在 | `database file not found` |

### 4.8 调试状态

```
GET /api/debug/status
```

> ⚠️ **仅当 `DEBUG` 开启时该路由才被注册**。未开启时本请求命中不到任何 handler，返回 404（`404 page not found` 纯文本）。这不是错误，是正常状态。

**响应 200**：

| 字段 | 类型 | 说明 |
|---|---|---|
| `on` | bool | 恒为 `true`（关闭时这个接口不存在） |
| `dir` | string | 调试文件落盘目录的**绝对路径** |
| `files[]` | array | 白名单内每个文件的落盘情况 |

`files[]` 每项：

| 字段 | 类型 | 说明 |
|---|---|---|
| `name` | string | 文件名（滚动副本为 `<名字>.1`） |
| `exists` | bool | 是否已经生成过 |
| `bytes` | int | 文件大小；不存在时为 0 |
| `modified` | string | 最近写入时间（RFC3339）；不存在时为空 |
| `truncated` | bool | 是否已达滚动阈值（16 MiB） |

```json
{
  "on": true,
  "dir": "/app/data/debug",
  "files": [
    { "name": "startup.json", "bytes": 2065, "exists": true, "modified": "2026-10-01T22:12:41+08:00", "truncated": false },
    { "name": "events.jsonl", "bytes": 1200, "exists": true, "modified": "2026-10-01T22:08:49+08:00", "truncated": false }
  ]
}
```

### 4.9 下载调试文件

```
GET /api/debug/files/{name}
```

`{name}` 只能是白名单里的名字，滚动副本带 `.1` 后缀：

| 允许的 `name` |
|---|
| `startup.json` |
| `events.jsonl` |
| `http.jsonl` |
| `submit-payloads.jsonl` |
| 上述任一 + `.1` |

**响应 200**：文件流。

| 响应头 | 值 |
|---|---|
| `Content-Type` | `application/octet-stream` |
| `Content-Disposition` | `attachment; filename="<name>"` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 404 | `DEBUG` 未开启 | `unknown debug file` |
| 404 | 文件名不在白名单（含任何带 `/`、`\` 的路径） | `unknown debug file` |
| 404 | 文件不在磁盘上 | `debug file not found` |

### 4.10 清空调试文件

```
DELETE /api/debug/files/{name}
```

删除该文件及其滚动副本（`.1`），下次写入时自动重建。

**响应 200**：

```json
{ "cleared": ["http.jsonl", "http.jsonl.1"] }
```

`cleared` 只列出**确实被删掉**的文件；本来就不存在的不会出现在里面。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 404 | `DEBUG` 未开启，或文件名不在白名单 | `unknown debug file` |
| 500 | 删除失败（权限等） | 系统错误原文 |

---

### 4.11 DEBUG 模式落盘了什么

`DEBUG` 开启后，`<DATA_DIR>/debug/` 下会有四份文件。之所以一次装这么多，是因为服务部署在云端，加一行日志就要重新 build → save → load → compose up，排查能力必须一次装够。

| 文件 | 内容 |
|---|---|
| `startup.json` | 每次启动**覆盖写**的一份快照：版本、监听地址、`data_dir` / `db_path` / 各子目录的**绝对路径与可写性**、`comfyui_url`、`journal_mode`、各表行数、关键列是否存在 |
| `submit-payloads.jsonl` | 每次 `POST /prompt` 的完整请求体。**写盘在请求发出之前**，所以被 400 拒掉的载荷同样看得到 |
| `http.jsonl` | 所有出站请求。**非 2xx 与传输错误必然记录**（带响应体，截断 64 KiB）；2xx 默认不记 |
| `events.jsonl` | 语义事件（见下表） |

`http.jsonl` 的每条记录：

| 字段 | 说明 |
|---|---|
| `tag` | 调用方标签：`prompt` / `history` / `view` / `object_info` / `queue` / `system_stats` |
| `method` / `url` | 请求方法与完整 URL |
| `status` | HTTP 状态码（传输错误时为 0） |
| `duration_ms` | 耗时 |
| `error` | 传输错误原文（连不上、超时） |
| `body` | 非 2xx 的响应体（`view` 是二进制，不记） |
| `note` | 成功场景下调用方补的说明，如 `prompt_id=...`、`downloaded` |

> ⚠️ 2xx 默认不记是因为 `/history` 每 5 秒轮询一次，全记会把真正的问题埋进噪音。成功但需要留痕的场景（下载字节数、拿到的 `prompt_id`）由调用方显式补一行。

`events.jsonl` 的 `event` 取值：

| `event` | 含义 | 关键字段 |
|---|---|---|
| `item_submitted` | 提交成功 | `item_id` / `task_id` / `comfy_prompt_id` / `notes` |
| `item_failed` | item 进入 failed | `item_id` / `task_id` / `error` |
| `repair_applied` | 提交被拒后自动纠正了串位值 | `rejection`（ComfyUI 原话）、`repairs[]`（每处改动的字段/原值/新值）、`retry_ok` |
| `repair_not_possible` | 被拒但**无处可修**（值已无归属或有歧义） | `rejection` |
| `comfy_execution_error` | ComfyUI 自己报执行错（`status_str == "error"`） | `history`（裁剪后的 `status` + `messages` 末尾 40 条） |
| `no_outputs` | ComfyUI 说做完了但输出里没有图 | `history`（含 `outputs` 原文） |
| `save_retry` | 图已产出但本轮没能保存，保留 `submitted` 重试 | `item_id` / `attempt` / `error` |
| `waiting_for_output` | item 卡在 `submitted` 的周期心跳（每 60 轮 ≈5 分钟） | `item_id` / `rounds` / `reason` |
| `node_def_missing` | 拉 `object_info/<类型>` 失败 | `class_type` / `url` / `error` |
| `widget_slots_unparsed` | 节点定义的控件顺序解析不出来（老格式工作流会因此还原不出值） | `class_type` / `error` |
| `recover` | 重启时的任务恢复 | `kept_in_flight` / `reset_pending` |

**输出上限**：单个文件超过 16 MiB 滚成 `.1`，只保留一代。`DEBUG` 忘记关闭不会把数据卷写满。

> ⚠️ 开启 `DEBUG` 会把**完整提示词内容**持续写入这些文件，排查完请去掉 `DEBUG` 并重启。

---

## 5. 工作流接口

### 5.1 核心概念：提示词落点

一份工作流的可配置内容只有两样：**正向提示词写到哪里**、**负向提示词写到哪里**。落点存在 `workflows.mapping_json`：

```json
{
  "positive_prompt": { "node_id": "6", "field": "text" },
  "negative_prompt": { "node_id": "7", "field": "text" }
}
```

**落点自动识别**：从「`positive` 与 `negative` 两个输入都接了线」的节点（即条件采样器）出发，沿连线上溯找到第一个承载提示词的文本节点。识别结果在**导入时自动写入**，无需手工配置。

**必须同时记录 `node_id` 与 `field`**：有些节点（如 `TextEncodeQwenImage21`）在同一个节点上既有 `prompt` 也有 `negative_prompt` 两个输入框，只记节点会在注入时让负向覆盖正向。

**负向同源**：部分工作流的负向是从正向 zero-out（`ConditioningZeroOut`）派生出来的，没有独立的负向文本框。此时 `negative_shared` 为 `true`，负向落点不写入映射，提交时负向提示词不会生效——接口会通过 `negative_unavailable` 字段明确说明。

### 5.2 数据结构

**落点对象**

```json
{ "node_id": "6", "field": "text" }
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `node_id` | string | 节点 id |
| `field` | string | 输入字段名；**可能为空**（老格式导出离线时只能拿到节点 id，字段名留到提交时解析） |

**落点结果 `promptTargets`**

| 字段 | 类型 | 说明 |
|---|---|---|
| `positive` | object \| null | 正向落点 |
| `negative` | object \| null | 负向落点 |
| `negative_shared` | bool | 为 `true` 表示负向与正向落在**同一节点同一字段**，负向无处可写 |

**候选节点 `promptCandidate`**

| 字段 | 类型 | 说明 |
|---|---|---|
| `node_id` | string | 节点 id |
| `class_type` | string | 节点类型名 |
| `field` | string | 字段名；老格式可能缺失 |
| `preview` | string | 控件原文预览（最多 80 字符） |
| `role` | string | 自动识别给出的角色：`positive` / `negative` / 空 |

> 候选列表已剔除前端专用节点（`Note` / `Reroute` / `PrimitiveNode`）与静音（mode=2）、旁路（mode=4）节点——它们不会进入提交载荷，选中也无效。

### 5.3 工作流列表

```
GET /api/workflows?page=1&page_size=20
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `items` | array | 工作流列表 |
| `total` | int | 总数 |
| `page` | int | 当前页 |
| `page_size` | int | 每页条数 |

`items[]` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | 主键 |
| `name` | string | 名称 |
| `description` | string | 描述 |
| `workflow_path` | string | 数据库中的原始值（**未**取 basename） |
| `mapping` | object | 提示词落点映射 |
| `negative_prompt` | string | 工作流级负向提示词 |
| `enabled` | bool | 是否启用 |
| `is_default` | bool | 是否为默认工作流 |
| `created_at` | string | 创建时间 |
| `updated_at` | string | 更新时间 |
| `task_count` | int | 引用该工作流的任务总数 |
| `in_flight` | int | 排队中 / 生成中的任务数 |

排序：`id` 倒序。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 500 | 查询失败 | `query workflows failed` |
| 500 | 行读取失败 | `read workflow failed` |

### 5.4 新建工作流

```
POST /api/workflows
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 是 | 名称（自动去首尾空格） |
| `workflow_json` | object \| string | 是 | 工作流 JSON。两种形态都接受：直接给对象，或把导出文件的原文当字符串塞进来（外层引号会被自动剥掉）；不是对象一律 400 |
| `description` | string | 否 | 描述，默认 `""` |
| `negative_prompt` | string | 否 | 工作流级负向提示词，默认 `""` |
| `mapping` | object | 否 | 手工指定的落点；**未指定的一侧由自动识别补全** |
| `params_schema` | object | 否 | 已废弃，仅为兼容旧前端保留，服务端忽略 |

**响应 201**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | 新工作流 id |
| `name` | string | 名称 |
| `workflow_path` | string | 生成的文件名，格式 `<安全化的名称>-<id>.json` |
| `mapping` | object | 最终落点映射 |
| `negative_prompt` | string | 负向提示词 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | body 非法 / `name` 为空 / 缺 `workflow_json` | `name and workflow_json are required` |
| 400 | `workflow_json` 不是合法 JSON | `workflow_json must be valid JSON` |
| 400 | `workflow_json` 是合法 JSON 但不是对象（字符串/数组/数字） | `workflow_json must be a JSON object` |
| 400 | `mapping` 非空但非法 | `mapping must be valid JSON` |
| 500 | 写库或写文件失败 | `save workflow failed` / `save workflow file failed` |

> 文件名的 id 后缀是必需的：只按名称生成会让两个同名工作流指向同一个文件，改其中一个会静默改掉另一个。

### 5.5 工作流详情

```
GET /api/workflows/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | |
| `name` | string | |
| `description` | string | |
| `workflow_path` | string | 这里是 **basename**（与列表接口不同） |
| `workflow_json` | object | 工作流 JSON 全文 |
| `mapping` | object | 落点映射 |
| `negative_prompt` | string | |
| `enabled` | bool | |
| `is_default` | bool | |
| `created_at` / `updated_at` | string | |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 不是整数 | `invalid workflow id` |
| 404 | 记录不存在 | `workflow not found` |
| 500 | 读工作流文件失败 | `read workflow file failed` |

### 5.6 更新工作流

```
PUT /api/workflows/{id}
```

**请求体**（全部可选）

| 字段 | 类型 | 缺省行为 |
|---|---|---|
| `name` | string | 去空格后为空 → 保持原值 |
| `description` | string | **不传 → 保持原值；传空串 → 清空** |
| `workflow_json` | object \| string | 不传 → 不重写文件。形态规则同创建接口 |
| `mapping` | object | 不传 → 用库中已有映射 |
| `negative_prompt` | string | **不传 → 保持原值；传空串 → 清空** |

> `description` 与 `negative_prompt` 用「是否传该字段」区分「不改」和「清空」，`null` 与省略等价。

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | |
| `name` | string | 最终名称 |
| `workflow_path` | string | 最终文件名 |
| `mapping` | object | 最终落点映射 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 / body 非法 | `invalid workflow id` / `invalid JSON body` |
| 400 | 新 `workflow_json` 非法 | `workflow_json must be valid JSON` / `workflow_json must be a JSON object` |
| 404 | 记录不存在 | `workflow not found` |
| 500 | 写文件 / 更新失败 | `save workflow file failed` / `update workflow failed` |

> 替换了 `workflow_json` 时，服务端会用**新的工作流**重新做落点识别（沿用旧映射会导致提示词写到已不存在的节点上）。

### 5.7 删除工作流

```
DELETE /api/workflows/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | |
| `deleted` | bool | 恒为 `true` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 409 | 存在排队中 / 生成中的任务 | `该工作流还有 N 个排队中/生成中的任务，请等待完成或先取消这些任务` |
| 404 | 记录不存在 | `workflow not found` |
| 500 | 删除失败 | `delete workflow failed` |

> 只拦「在途」任务。已完成的历史任务不受影响（任务表冗余了工作流名，展示不依赖此表）。
> 删除顺序为先删库、后删文件——即使删文件失败，也只会留下可人工清理的孤儿文件，不会出现删不掉的记录。
> 若历史数据中多个工作流共用同一文件，删除时会检查引用，不会误删他人文件。

### 5.8 设置 / 取消默认工作流

```
PUT /api/workflows/{id}/default
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `is_default` | bool | 是 | `true` 设为默认；`false` 取消默认 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | 工作流 id |
| `is_default` | bool | 回显请求值 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 / body 非法 | `invalid workflow id` / `invalid JSON body` |
| 404 | 记录不存在 | `workflow not found` |
| 500 | 操作失败 | `set default failed` |

> 默认为**全局唯一**：设为默认时会先把其它工作流的默认标记清零。

### 5.9 识别落点（未保存的工作流）

```
POST /api/workflows/prompt-targets
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `workflow_json` | object \| string | 是 | 工作流 JSON。两种形态都接受（对象，或导出文件原文当字符串） |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `detected` | object | 自动识别结果（`promptTargets`） |
| `stored` | object | 已保存的映射；此入口不读库，**恒为 `{"positive_prompt":{},"negative_prompt":{}}`** |
| `candidates` | array | 候选节点列表 |
| `negative_unavailable` | string | 负向为何不可用；空串表示无此问题 |

`negative_unavailable` 的两种取值：

| 条件 | 文案 |
|---|---|
| 负向与正向同源 | `这份工作流的负向由正向派生（ConditioningZeroOut 之类），没有独立的负向提示词节点，负向提示词不会生效。` |
| 未识别出负向 | `未能自动识别负向提示词节点，请手动选择；选好后提交时会写进去。` |

```json
{
  "detected": {
    "positive": { "node_id": "6", "field": "text" },
    "negative": { "node_id": "7", "field": "text" },
    "negative_shared": false
  },
  "stored": { "positive_prompt": {}, "negative_prompt": {} },
  "candidates": [
    { "node_id": "6", "class_type": "CLIPTextEncode", "field": "text",
      "preview": "a photo of a cat", "role": "positive" },
    { "node_id": "7", "class_type": "CLIPTextEncode", "field": "text",
      "preview": "blurry, low quality", "role": "negative" }
  ],
  "negative_unavailable": ""
}
```

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | 缺 `workflow_json` | `workflow_json is required` |
| 400 | 不是合法 JSON | `workflow_json must be valid JSON` |

> 该入口不读库，用于「还没保存的新工作流」先看落点。识别不需要 ComfyUI 在线。

### 5.10 识别落点（已保存的工作流）

```
GET /api/workflows/{id}/prompt-targets
```

响应结构同 5.9，区别在于 `stored` 来自数据库，且**已保存的映射优先**——用户手工选过的落点不会被自动识别覆盖。

若工作流导入时未能识别出任何落点，`stored` 会是空对象：

```json
{ "positive_prompt": {}, "negative_prompt": {} }
```

提交时服务端会再自动解析一次。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid workflow id` |
| 404 | 记录不存在 | `workflow not found` |
| 500 | 读文件 / 解析失败 | `read workflow file failed` / `parse workflow failed` |

### 5.11 校验工作流

```
POST /api/workflows/validate          # 校验 body 里的工作流
POST /api/workflows/{id}/validate     # 校验已保存的工作流
```

**请求体**（可选）

| 字段 | 类型 | 说明 |
|---|---|---|
| `workflow_json` | object \| string | 传了优先用它；不传则读库中那份。**两种形态都接受**：直接给对象，或把导出文件的原文当字符串塞进来（后者的外层引号会被自动剥掉） |

**响应 200**

> ⚠️ **校验发现问题时 HTTP 状态码依然是 200**，结果体现在 `ok` / `errors` / `warnings` 中。

| 字段 | 类型 | 说明 |
|---|---|---|
| `ok` | bool | 无 errors 时为 `true` |
| `unreachable` | bool | ComfyUI 不可达时为 `true`（此时 `ok=false`） |
| `message` | string | 不可达时的说明 |
| `workflow_id` | int64 | 不带 id 的入口为 0 |
| `workflow_name` | string | 不带 id 的入口为空串 |
| `comfyui_url` | string | 校验时使用的 ComfyUI 地址 |
| `checked_at` | string | 校验时间（RFC3339） |
| `errors` | array | 致命问题，会导致提交失败 |
| `warnings` | array | 提示性问题，不影响提交 |
| `skipped_nodes` | array | 提交时会被自动跳过的节点 |

`errors[]` / `warnings[]` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `node_id` | string | 节点 id |
| `class_type` | string | 节点类型 |
| `field` | string | 字段名（可能缺失） |
| `kind` | string | 问题种类，见下表 |
| `message` | string | 可读描述 |
| `value` | string | 相关取值（可能缺失） |
| `candidates` | array\<string\> | combo 候选列表（可能缺失） |

`kind` 取值：

| kind | 归类 | 含义 |
|---|---|---|
| `workflow_format` | errors | 工作流不是合法 JSON / 无法转换为提交格式 / 没有任何可提交节点 |
| `unsupported_feature` | errors | 使用了不支持的特性（如子图） |
| `node_type_missing` | errors | 目标 ComfyUI 上没有该节点类型（缺自定义节点/插件） |
| `value_not_in_list` | errors | 取值不在候选列表里 |
| `required_missing` | errors | 缺少必填输入（既没有值也没有连线） |
| `unknown_field` | warnings | 节点定义里没有这个输入名，会被忽略 |
| `widget_values_mismatch` | warnings | 旧格式导出，控件值个数与当前节点定义不符 |
| `auto_repairable` | warnings | 导出把控件值串位了：值不在本字段的候选里，却是同一节点另一个字段的合法取值。**提交时会自动纠正后重试一次**，工作流文件不会被改动 |

`skipped_nodes[]` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `node_id` | string | 节点 id |
| `class_type` | string | 节点类型 |
| `reason` | string | `静音（mode=2）` / `旁路（mode=4）` / `前端专用节点` |

**示例：校验失败**

```json
{
  "ok": false,
  "workflow_id": 3,
  "workflow_name": "写实人像",
  "comfyui_url": "http://127.0.0.1:8188",
  "checked_at": "2026-10-01T18:30:00+08:00",
  "errors": [
    {
      "node_id": "8", "class_type": "CLIPLoader", "field": "clip_name",
      "kind": "value_not_in_list",
      "message": "取值 \"krea2\" 不在候选列表里",
      "value": "krea2",
      "candidates": ["umt5_xxl_fp8_e4m3fn.safetensors"]
    }
  ],
  "warnings": [
    {
      "node_id": "6", "class_type": "CLIPTextEncode", "field": "legacy_field",
      "kind": "unknown_field",
      "message": "节点定义里没有这个输入名，会被 ComfyUI 忽略（通常说明导出格式与当前版本不匹配）"
    }
  ],
  "skipped_nodes": [
    { "node_id": "1", "class_type": "Note", "reason": "前端专用节点" }
  ]
}
```

**示例：ComfyUI 不可达**

```json
{
  "ok": false,
  "unreachable": true,
  "message": "无法从 ComfyUI(http://127.0.0.1:8188) 读取节点定义：dial tcp 127.0.0.1:8188: connect: connection refused",
  "workflow_id": 3,
  "workflow_name": "写实人像",
  "comfyui_url": "http://127.0.0.1:8188",
  "checked_at": "2026-10-01T18:30:00+08:00",
  "errors": [],
  "warnings": []
}
```

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid workflow id` |
| 404 | 带 id 但不存在 | `workflow not found` |
| 400 | 既无 id 也无 `workflow_json` | `workflow_json is required` |
| 400 | 未配置 ComfyUI 地址 | `comfyui_url is not configured` |
| 500 | 读文件失败 | `read workflow file failed` |

> 「连不上」与「工作流有问题」被刻意分开报告——服务没起时若报一堆「节点不存在」，会把排查方向带偏。
> 校验是**只读检查**，不会阻拦保存或提交；前端把它呈现为提示而非闸门。

---

## 6. 任务接口

### 6.1 状态机

**任务（task）**

```
pending ──→ running ──→ completed
   │           │
   │           └──→ failed ──(retry)──→ pending
   └──→ cancelled
```

| 状态 | 说明 |
|---|---|
| `pending` | 已入库，等待后台提交 |
| `running` | 正在提交 / 等待 ComfyUI 出图 |
| `completed` | 所有 item 都已终结（部分成功部分失败也算完成） |
| `failed` | 全部 item 失败 |
| `cancelled` | 用户取消 |

**任务项（item）**

```
pending ──→ running ──→ submitted ──→ success
   │                        │
   └──→ failed ←────────────┘
```

| 状态 | 说明 |
|---|---|
| `pending` | 等待提交 |
| `running` | 正在提交给 ComfyUI |
| `submitted` | 已提交，等待出图 / 等待下载 |
| `success` | 出图并已保存 |
| `failed` | 失败（见 `error_message`） |

**下载重试语义**：图片已在 ComfyUI 出好但保存失败（如磁盘错误）时，item **保持 `submitted`**，下一轮自动重试；连续失败 40 次才判定为失败。

### 6.2 直接提交提示词

```
POST /api/tasks/direct
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `workflow_id` | int64 | 是 | 必须是存在且启用的工作流 |
| `positive_prompt` | string | 是 | 正向提示词，去空格后不能为空 |
| `title` | string | 否 | 标题；省略时自动取正向提示词前 50 个字符 |
| `parameters` | object | 否 | 透传参数，原样存入任务记录 |

> ⚠️ 由于请求体拒绝未知字段，**不要传 `negative_prompt`**——它不在结构体里，会导致整个请求解析失败并返回
> `workflow_id and positive_prompt are required`。负向提示词配置在**工作流**上。

**响应 202**

> 注意是 **202 Accepted**，不是 201。

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | 任务 id |
| `prompt_id` | int64 | 同时入库的提示词 id（分组为「手动提交」） |
| `status` | string | 固定 `pending` |
| `comfyui_url` | string | 当前 ComfyUI 地址快照 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | body 非法 / 缺必填 | `workflow_id and positive_prompt are required` |
| 400 | 工作流不存在或已禁用 | `workflow not found or disabled` |
| 500 | 读设置 / 写库失败 | `read ComfyUI URL failed` / `create task failed` / `create prompt failed` / `create task item failed` / `commit task failed` |

> 建任务只是入队。真正的提交由后台 submitter 每 3 秒扫 `pending` 的 item 完成，
> 提交失败会写进该 item 的 `error_message`（原文形如
> `submit to ComfyUI failed: ComfyUI /prompt HTTP 400: node 8 (CLIPLoader): clip_name: 'krea2' not in [...]`）。

#### 提交被拒时的自动纠正

工作流从 ComfyUI 导出时，控件值会按「位置数组 + 名字映射」两份一起写出。节点定义在版本间加了控件、
或这份 JSON 是脚本/更早版本拼出来的时候，值会落到隔壁字段上——文件在 ComfyUI 里点运行是好的，
导出的 JSON 却过不了 `/prompt` 的校验。

服务在**被 ComfyUI 明确以「取值不在列表」拒绝**时，会做一次保守的纠正后重试一次：

- 若这个错位的值，恰好是**同一个节点另一个输入**的合法取值 → 把它挪过去；
- 腾空的字段用该字段自己的默认值补上（显式 `default` 优先，否则取候选列表第一项）。

例：`clip_name` 拿到了 `"krea2"`（那是 `type` 的合法取值），`type` 停在默认值上 →
把 `krea2` 挪到 `type`，`clip_name` 补上候选第一项（正是本来的文本编码器）。

边界（写死的行为，不会越界）：

- **只影响这一次提交的载荷**。`data/workflows/*.json` 一个字节都不会改。
- **只重试一次**。纠正本身是确定性的一步，再失败说明不是串位问题。
- **值已经没有归属时不动**（模型被删、选项下线）。那种情况没有正确答案，静默换成别的模型会让人
  以为跑的是自己选的东西——直接把 ComfyUI 的原话原样报回 `error_message`。
- 该值有**多个**字段都接受、或拿不到节点定义 → 不动，同样原样报错。

改了什么会记在 item 的 `notes` 里，任务详情可见：

```
提交时自动纠正了导出串位的控件值——node 8 (CLIPLoader)：clip_name 的 "krea2" 不在候选里，
但 type 接受它 → 挪到 type（原值 "stable_diffusion"），clip_name 用默认值
"qwen3VL4BAbliteratedComfyui_v10_full_fp8.safetensors" 补上
```

同样的判断会在 `POST /api/workflows/validate` 里提前以 `auto_repairable` 警告给出，不必等提交。

### 6.3 任务列表

```
GET /api/tasks?status=running&group=风景&page=1&page_size=20
```

**Query 参数**

| 参数 | 类型 | 说明 |
|---|---|---|
| `status` | string | 精确匹配任务状态 |
| `group` | string | 按提示词分组筛选：任务下任一 item 的提示词属于该分组即命中 |
| `page` / `page_size` | int | 见 1.3 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `items` | array | 任务列表 |
| `total` / `page` / `page_size` | int | |

`items[]` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | 任务 id |
| `source_type` | string | 来源类型，如 `direct` |
| `workflow_id` | int | 工作流 id |
| `workflow_name` | string | 工作流名（工作流已删除时回退任务上的冗余名，可能为空串） |
| `status` | string | 任务状态 |
| `total_count` / `success_count` / `failed_count` | int | 计数 |
| `created_at` | string | 创建时间 |
| `image_id` | int64 | 代表图 id；无图时该字段**不存在** |
| `prompt_id` | int64 | 该任务第一个提示词 id；无则字段不存在 |
| `prompt_title` | string | 提示词标题 |
| `prompt_count` | int64 | 去重后的提示词数量 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 500 | 查询失败 | `query tasks failed` / `read task failed` |

### 6.4 任务详情

```
GET /api/tasks/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | |
| `source_type` | string | |
| `workflow_id` | int | |
| `comfyui_url` | string | 任务创建时的地址快照 |
| `parameters` | object | 原始参数，形如 `{"positive_prompt":"...","parameters":{...}}` |
| `status` | string | |
| `total_count` / `success_count` / `failed_count` | int | |
| `created_at` | string | |
| `items` | array | 任务项列表 |

`items[]` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | item id |
| `prompt_id` | int64 | 关联提示词 id，无关联为 0 |
| `positive_prompt` | string | 正向提示词 |
| `comfy_prompt_id` | string | ComfyUI 返回的 prompt id，未提交时为空串 |
| `status` | string | item 状态 |
| `error_message` | string | 失败原因 |
| `notes` | string | 提交时的中性说明（目前只有「自动纠正了导出串位的控件值」这一种），无说明时为空串 |
| `images` | array | 结果图，元素为 `{"id":int64,"filename":string}`；无图时该字段不存在 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid task id` |
| 404 | 任务不存在 | `task not found` |

> 此接口**不返回** `workflow_name`（只有列表接口返回）。

### 6.5 任务事件流（SSE）

```
GET /api/tasks/{id}/events
```

**响应 200**：`Content-Type: text/event-stream`

| 响应头 | 值 |
|---|---|
| `Content-Type` | `text/event-stream` |
| `Cache-Control` | `no-cache` |
| `Connection` | `keep-alive` |

建立连接后服务端先发送握手注释：

```
: connected
```

随后每个事件形如 `data: <JSON>\n\n`：

| `status` | 携带字段 |
|---|---|
| `running` | `task_id`, `item_id`, `status` |
| `success` | `task_id`, `item_id`, `status` |
| `failed` | `task_id`, `item_id`, `status`, `error` |
| `cancelled` | `task_id`, `status`（**无 `item_id`**） |
| `pending` | `task_id`, `status`（重试时推送） |

示例：

```
data: {"task_id":11,"item_id":23,"status":"running"}

data: {"task_id":11,"item_id":23,"status":"success"}
```

订阅者缓冲为 8；缓冲满时新事件会被丢弃（不阻塞服务端）。客户端断开时自动注销订阅。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid task id` |
| 500 | 运行环境不支持流式响应 | `streaming unsupported` |

### 6.6 取消任务

```
POST /api/tasks/{id}/cancel
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | |
| `status` | string | 固定 `cancelled` |

**前置条件**：任务状态必须是 `pending` 或 `running`。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 404 | 任务不存在 | `task not found` |
| 409 | 状态不允许取消 | `task is not cancellable` |
| 500 | 其它错误 | `cancel task failed` |

> 取消时会异步通知 ComfyUI 从队列中移除已提交的任务（仅移除排队项，不中断正在执行的）。

### 6.7 重试任务

```
POST /api/tasks/{id}/retry
```

**响应 202**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | |
| `status` | string | 固定 `pending` |

**前置条件**：任务状态必须是 `failed`，且其关联工作流仍然存在。

已成功的 item 保持不变（不会重复出图），只有失败的 item 被重置。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 404 | 任务不存在 | `task not found` |
| 409 | 状态不是 `failed` | `only failed tasks can be retried` |
| 409 | 关联工作流已删除 | `工作流已删除，无法重试该任务` |
| 500 | 其它错误 | `retry task failed` |

### 6.8 删除任务

```
DELETE /api/tasks/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | |
| `status` | string | 固定 `deleted` |

**前置条件**：任务状态不能是 `running`。

删除会级联清理该任务的 items、图片记录与磁盘图片文件。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 404 | 任务不存在 | `task not found` |
| 409 | 任务正在运行 | `cannot delete running task, cancel it first` |
| 500 | 其它错误 | `delete task failed` |

### 6.9 批量操作

```
POST /api/tasks/batch-cancel
POST /api/tasks/batch-retry
POST /api/tasks/batch-delete
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `ids` | array\<int64\> | 是 | 任务 id 列表，最多 500 个 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `applied` | int | 成功处理的数量 |
| `skipped` | int | 被跳过的数量 |

**计入 `skipped` 的情况**：任务不存在、状态不允许取消、状态不是 failed、正在运行不可删除、关联工作流已删除。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | body 非法或 `ids` 为空 | `ids is required` |
| 400 | 超过 500 个 | `too many ids, max 500 per request` |
| 500 | 出现非可跳过的服务端错误 | `batch operation failed` |

---

## 7. 提示词接口

### 7.1 提示词列表

```
GET /api/prompts?group=风景&status=pending&q=cat&favorite=1&page=1&page_size=20
```

**Query 参数**

| 参数 | 类型 | 说明 |
|---|---|---|
| `group` | string | 精确匹配分组名 |
| `status` | string | 精确匹配状态 |
| `q` | string | 模糊搜索标题或正向提示词 |
| `favorite` | string | 值为 `1` 时只返回收藏项 |
| `page` / `page_size` | int | 见 1.3 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `items` | array | 提示词列表 |
| `total` / `page` / `page_size` | int | |

`items[]` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `title` | string | |
| `description` | string | |
| `positive_prompt` | string | |
| `group_name` | string | 分组名 |
| `group_id` | string | 分组 id（由分组名生成，纯中文时可能为空串） |
| `status` | string | |
| `is_favorite` | bool | |
| `completed_at` | string | 可能为空串 |
| `created_at` | string | |
| `run_count` | int | 关联生成次数 |
| `image_count` | int | 关联图片数 |
| `cover_image_id` | int | 封面图 id；无图为 0 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 500 | 查询失败 | `query prompts failed` / `read prompt failed` |

### 7.2 新建提示词

```
POST /api/prompts
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `positive_prompt` | string | 是 | 正向提示词 |
| `title` | string | 否 | 省略时自动取正向提示词前 50 个字符 |
| `description` | string | 否 | |
| `group_name` | string | 否 | 分组名，决定 `group_id` |

**响应 201**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `title` | string | 最终标题 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | body 非法或正向提示词为空 | `positive_prompt is required` |
| 500 | 插入失败 | `create prompt failed` |

### 7.3 分组列表

```
GET /api/prompts/groups
```

**响应 200**：**裸数组**（不是 `{items:...}`）

| 字段 | 类型 | 说明 |
|---|---|---|
| `group_name` | string | 分组名 |
| `group_id` | string | 分组 id |
| `count` | int | 该组提示词数量 |

```json
[
  { "group_name": "手动提交", "group_id": "manual", "count": 12 },
  { "group_name": "风景", "group_id": "fengjing", "count": 40 }
]
```

「手动提交」分组固定排在最前，其余按最近创建时间倒序。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 500 | 查询失败 | `query groups failed` / `read group failed` |

### 7.4 提示词详情

```
GET /api/prompts/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `title` | string | |
| `description` | string | |
| `positive_prompt` | string | |
| `group_name` / `group_id` | string | |
| `status` | string | |
| `is_favorite` | bool | |
| `completed_at` | string | |
| `created_at` / `updated_at` | string | |
| `runs` | array | 生成历史，倒序 |

`runs[]` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `item_id` | int | 任务项 id |
| `task_id` | int | 任务 id |
| `status` | string | |
| `comfy_prompt_id` | string | |
| `error_message` | string | |
| `workflow_id` | int | |
| `workflow_name` | string | |
| `images` | array | 元素为 `{"id":int,"filename":string}` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid prompt id` |
| 404 | 不存在 | `prompt not found` |

### 7.5 更新提示词

```
PUT /api/prompts/{id}
```

**请求体**：同 7.2。

> 各字段**传空串表示保留原值**（不清空）。

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `updated` | bool | 恒为 `true` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 / body 非法 | `invalid prompt id` / `invalid JSON body` |
| 404 | 不存在 | `prompt not found` |
| 500 | 更新失败 | `update prompt failed` |

### 7.6 删除提示词

```
DELETE /api/prompts/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `deleted` | bool | 恒为 `true` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid prompt id` |
| 404 | 不存在 | `prompt not found` |
| 500 | 删除失败 | `delete prompt failed` |

### 7.7 切换收藏

```
PATCH /api/prompts/{id}/favorite
```

**请求体**：无。

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `is_favorite` | bool | 切换后的状态 |

> **收藏是联动的**：切换提示词收藏会同步该提示词名下的**所有图片**；反过来切换某张图片的收藏，也会同步其所属提示词的收藏态并级联到同组其它图片。手动提交的图片（无关联提示词）只影响自身。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid prompt id` |
| 404 | 不存在 | `prompt not found` |
| 500 | 失败 | `toggle favorite failed` |

### 7.8 批量删除提示词

```
POST /api/prompts/batch-delete
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `ids` | array\<int64\> | 是 | 提示词 id 列表 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `deleted` | int | 实际删除行数 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | body 非法或 `ids` 为空 | `ids is required` |
| 500 | 删除失败 | `batch delete failed` |

> ⚠️ 此接口**不校验 500 上限**，与其它批量接口不同。

### 7.9 批量出图

```
POST /api/prompts/batch-run
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `ids` | array\<int64\> | 是 | 提示词 id 列表 |
| `workflow_id` | int64 | 是 | 使用的工作流 |
| `parameters` | object | 否 | 透传参数 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `created` | int | 成功创建的任务数 |

> ⚠️ 单条创建失败会被**静默跳过**（不计入 `created`，也不报错）。此接口同样**不校验 500 上限**。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | 缺参数 | `ids and workflow_id are required` |
| 500 | 读设置失败 | `read ComfyUI URL failed` |

### 7.10 按分组出图

```
POST /api/prompts/group-run
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `group_name` | string | 是 | 分组名 |
| `workflow_id` | int64 | 是 | 使用的工作流 |
| `parameters` | object | 否 | 透传参数 |

对分组下状态为 `pending` 或 `failed` 的提示词批量建任务。

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `created` | int | 成功创建的任务数 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | 缺参数 | `group_name and workflow_id are required` |
| 500 | 读设置 / 查询失败 | `read ComfyUI URL failed` / `query group prompts failed` / `read group prompts failed` |

### 7.11 单条出图

```
POST /api/prompts/{id}/run
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `workflow_id` | int64 | 是 | 使用的工作流 |
| `parameters` | object | 否 | 透传参数 |

**响应 202**

| 字段 | 类型 | 说明 |
|---|---|---|
| `task_id` | int | 新任务 id |
| `prompt_id` | int | 提示词 id |
| `status` | string | 固定 `pending` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 / 缺 `workflow_id` | `invalid prompt id` / `workflow_id is required` |
| 404 | 提示词不存在 | `prompt not found` |
| 500 | 创建失败 | `read ComfyUI URL failed` / `create task failed` / `create task item failed` / `commit task failed` |

---

## 8. JSON 导入接口

### 8.1 输入格式

支持两种等价写法：

```json
{ "entries": [ { "id": "1", "title": "标题", "desc": "描述", "positive": "提示词" } ] }
```

```json
[ { "id": "1", "title": "标题", "desc": "描述", "positive": "提示词" } ]
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `positive` | 是 | 正向提示词，不能为空 |
| `title` | 否 | 省略时取正向提示词前 50 个字符 |
| `desc` | 否 | 描述 |
| `id` | 否 | 仅用于文件内部去重 |

**关键行为**：

| 行为 | 说明 |
|---|---|
| 分组名 = 文件名 | 上传 `<文件名>.json` 后，其中的提示词归入名为 `<文件名>` 的分组 |
| 去重键 | `(分组名, 正向提示词)` —— **同组内相同提示词重复导入是幂等的** |
| 同名文件再导入 | 是**追加**而非覆盖：改了内容再用同名文件导入，会新增记录而不是替换 |

> ⚠️ 因此重新生成数据集时请**换一个文件名**，否则新旧内容会混在同一分组里。

### 8.2 备份文件列表

```
GET /api/json-files?page=1&page_size=20
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `items` | array | 元素含 `id`、`filename`、`created_at` |
| `total` / `page` / `page_size` | int | |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 500 | 查询失败 | `query JSON files failed` / `read JSON file failed` |

### 8.3 上传 JSON 文件

```
POST /api/json-files/upload
Content-Type: multipart/form-data
```

**表单字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `file` | file | 是 | 必须是 `.json` 文件，最大 50 MiB |

**响应 201**

| 字段 | 类型 | 说明 |
|---|---|---|
| `filename` | string | 生成的备份文件名 |
| `total` | int | 解析到的条目总数 |
| `inserted` | int | 实际插入数量 |
| `skipped_duplicates` | int | 因重复跳过的数量 |
| `group_name` | string | 分组名（= 原文件名去扩展名） |

```bash
curl -X POST http://127.0.0.1:8080/api/json-files/upload -F "file=@风景.json"
```

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | multipart 解析失败 | `invalid multipart form` |
| 400 | 缺 `file` 字段 | `file is required` |
| 400 | 非 `.json` 文件 | `only JSON files are supported` |
| 400 | 读文件失败 | `read file failed` |
| 400 | 内容格式错误 | `JSON must be an object with entries or an array` / `each entry requires a positive prompt` / `no entries found in JSON` |
| 500 | 备份或入库失败 | `save JSON backup failed` / `save JSON record failed` |

### 8.4 粘贴 JSON 文本导入

```
POST /api/json-files/import-text
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `content` | string | 是 | JSON 文本，最大 10 MiB |
| `filename` | string | 否 | 作为分组名；省略时使用 `手动导入-<时间戳>` |

**响应 201**：同 8.3。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | body 为空或读取失败 | `request body is required` |
| 400 | 外层不是合法 JSON | `invalid request body` |
| 400 | `content` 为空 | `content is required` |
| 400 | 内容格式错误 | 同 8.3 的三条消息 |
| 500 | 备份或入库失败 | `save JSON backup failed` / `save JSON record failed` |

### 8.5 查看备份内容

```
GET /api/json-files/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `filename` | string | |
| `content` | string | 文件全文 |
| `created_at` | string | |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid JSON file id` |
| 404 | 不存在 | `JSON file not found` |
| 500 | 读磁盘文件失败 | `read JSON file failed` |

### 8.6 下载备份文件

```
GET /api/json-files/{id}/download
```

**响应 200**：文件流，带 `Content-Disposition: attachment`。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid JSON file id` |
| 404 | 不存在 | `JSON file not found` |

### 8.7 删除备份记录

```
DELETE /api/json-files/{id}
```

删除备份记录与磁盘上的源文件，**不影响已入库的提示词**。

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `deleted` | bool | 恒为 `true` |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid JSON file id` |
| 404 | 不存在 | `JSON file not found` |
| 500 | 失败 | `delete JSON file failed` |

---

## 9. 图片接口

### 9.1 两个容易混淆的字段

| 字段 | 含义 |
|---|---|
| `filename` | **展示名 / 下载名**，通常是 ComfyUI 给出的原始文件名 |
| `storage_path` | **磁盘上的真实文件名**，形如 `<时间戳>_<id>.png`，与 `filename` 常常不同 |

文件类接口（`/file`、`/download`、删除）一律按 `storage_path` 定位真实文件。

> ⚠️ 列表接口 `GET /api/images` 里这个字段叫 **`path`**，详情接口 `GET /api/images/{id}` 里叫 **`storage_path`**——同一份数据，键名不同。

### 9.2 图片列表

```
GET /api/images?favorite=1&q=cat&group=风景&page=1&page_size=20
```

**Query 参数**

| 参数 | 类型 | 说明 |
|---|---|---|
| `favorite` | string | 值为 `1` 时只返回收藏图片 |
| `q` | string | 模糊搜索提示词标题、正向提示词或文件名 |
| `group` | string | 按提示词分组筛选 |
| `page` / `page_size` | int | 见 1.3 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `items` | array | 图片列表 |
| `total` / `page` / `page_size` | int | |

`items[]` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `generation_item_id` | int | 关联任务项 id |
| `filename` | string | 展示名 |
| `path` | string | 真实路径（即 `storage_path`） |
| `is_favorite` | bool | |
| `created_at` | string | |
| `prompt_id` | int | 关联提示词 id；**手动提交的图片为 0** |
| `prompt_title` | string | 无关联时为空串 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 500 | 查询失败 | `query images failed` / `read image failed` |

### 9.3 图片详情

```
GET /api/images/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `generation_item_id` | int | |
| `filename` | string | 展示名 |
| `storage_path` | string | 真实路径 |
| `positive_prompt` | string | 生成时使用的正向提示词 |
| `negative_prompt` | string | 工作流上的负向提示词 |
| `is_favorite` | bool | |
| `created_at` | string | |
| `prompt_id` | int | 无关联为 0 |
| `prompt_title` | string | 无关联为空串 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid image id` |
| 404 | 不存在 | `image not found` |

### 9.4 图片文件（内联）

```
GET /api/images/{id}/file
```

**响应 200**：图片二进制流，`Content-Type` 由文件扩展名推断（如 `image/png`）。不带 `Content-Disposition`，适合直接用于 `<img>` 标签。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid image id` |
| 404 | 记录不存在 | `image not found` |
| 404 | 文件路径解析失败 | `image file not found` |

### 9.5 图片下载

```
GET /api/images/{id}/download
```

**响应 200**：图片二进制流，带 `Content-Disposition: attachment; filename="<展示名>"`。

状态码同 9.4。

### 9.6 删除图片

```
DELETE /api/images/{id}
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `deleted` | bool | 恒为 `true` |

删除顺序为**先删磁盘文件、再删库记录**。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid image id` |
| 404 | 不存在 | `image not found` |
| 500 | 其它错误 | `delete image failed` |

### 9.7 批量删除图片

```
POST /api/images/batch-delete
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `ids` | array\<int64\> | 是 | 最多 500 个 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `applied` | int | 实际删除数 |
| `skipped` | int | 不存在的数量 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | body 非法或 `ids` 为空 | `ids is required` |
| 400 | 超过 500 个 | `too many ids, max 500 per request` |
| 500 | 出现非可跳过的错误 | `batch operation failed` |

### 9.8 批量收藏图片

```
POST /api/images/batch-favorite
```

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `ids` | array\<int64\> | 是 | 最多 500 个 |
| `favorite` | bool | 否 | `true` 收藏，`false` 取消；默认 `false` |

**响应 200**：`{"applied": N, "skipped": M}`

状态码同 9.7。

### 9.9 切换图片收藏

```
PATCH /api/images/{id}/favorite
```

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | |
| `is_favorite` | bool | 切换后的状态 |

> 若图片有关联提示词，则以提示词的收藏态为准并级联；手动提交的图片只影响自身。

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | `id` 非法 | `invalid image id` |
| 404 | 不存在 | `image not found` |
| 500 | 失败 | `toggle favorite failed` |

---

## 10. 数据导入导出

### 10.1 导出 ZIP

```
GET /api/export?favorite=1&group=风景&status=completed&search=cat
```

**Query 参数**

| 参数 | 类型 | 说明 |
|---|---|---|
| `favorite` | string | 值为 `1` 时只导出收藏 |
| `group` | string | 按分组筛选 |
| `status` | string | 按提示词状态筛选 |
| `search` | string | 模糊搜索标题、正向提示词、描述 |

> ⚠️ 这里的搜索参数名是 `search`，而列表接口用的是 `q`，注意区分。

**响应 200**：`application/zip` 流。

| 响应头 | 值 |
|---|---|
| `Content-Type` | `application/zip` |
| `Content-Disposition` | `attachment; filename="export-<YYYYMMDD_HHMMSS>.zip"` |

ZIP 结构：

```
export-20261001_183000.zip
├── prompts.json        # 提示词数组（含 negative_prompt / group_id / is_favorite）
├── images.json         # 图片数组（含 archive_name / prompt_id / positive_prompt）
└── images/             # 图片二进制文件
    ├── 1767000000_12.png
    └── ...
```

`prompts.json` 元素：

| 字段 | 类型 |
|---|---|
| `id` | int64 |
| `title` | string |
| `description` | string |
| `positive_prompt` | string |
| `negative_prompt` | string |
| `group_name` | string |
| `group_id` | string |
| `status` | string |
| `is_favorite` | bool |

`images.json` 元素：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int64 | |
| `filename` | string | 展示名 |
| `archive_name` | string | ZIP 内的实际文件名；与 `filename` 相同时省略 |
| `prompt_id` | int64 | |
| `positive_prompt` | string | |
| `is_favorite` | bool | |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 500 | 提示词查询失败 | `query prompts failed` |

### 10.2 导入 ZIP

```
POST /api/import
Content-Type: multipart/form-data
```

**表单字段**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `file` | file | 是 | 必须是 `.zip` 文件，最大 500 MiB |

**去重规则**

| 对象 | 去重键 |
|---|---|
| 提示词 | `title` + `positive_prompt` 完全相同 |
| 图片 | `filename` 已存在 |

**响应 200**

| 字段 | 类型 | 说明 |
|---|---|---|
| `imported_prompts` | int | 新插入的提示词数 |
| `skipped_prompts` | int | 跳过的提示词数 |
| `imported_images` | int | 新插入的图片数 |
| `skipped_images` | int | 跳过的图片数 |

| 状态码 | 触发条件 | 错误消息 |
|---|---|---|
| 400 | 缺 `file` 字段 | `missing file field` |
| 400 | 非 `.zip` 文件 | `only .zip files are supported` |
| 400 | ZIP 无法解析 | `invalid zip file` |
| 500 | 写临时文件 / 事务失败 | `create temp file failed` / `begin transaction failed` / `commit failed` |

> ZIP 内的文件名会被收敛为纯文件名，含路径分隔符或 `..` 的条目直接跳过（防止路径穿越）。

---

## 11. 附录：错误码与行为细节

### 11.1 状态码汇总

| 状态码 | 含义 | 典型场景 |
|---|---|---|
| 200 | 成功 | 查询、更新、删除（注意：**校验失败也是 200**） |
| 201 | 已创建 | 新建工作流 / 提示词 / JSON 导入 |
| 202 | 已接受 | 创建任务（异步执行） |
| 204 | 无内容 | `OPTIONS` 预检请求 |
| 400 | 参数错误 | 缺必填、`id` 非法、body 非法 JSON、含未知字段 |
| 404 | 不存在 | 资源 id 无效 |
| 409 | 状态冲突 | 工作流有在途任务、任务状态不允许取消/重试/删除 |
| 500 | 服务端错误 | 数据库或文件操作失败 |
| 502 | 上游不可达 | 测试 ComfyUI 连通性失败 |
| 503 | 依赖不可用 | 数据库 Ping 失败 |

> 「未开启 `DEBUG` 时 `/api/debug/*` 返回 404」是**路由不存在**，不是资源不存在，错误体是 Go 默认的 `404 page not found` 纯文本，而非 `{"error": ...}`。

### 11.2 值得注意的行为

**工作流**

- 落点自动识别在**导入时**完成，无需手工配置；手工选择的一侧优先，未选的一侧由自动识别补全。
- 工作流文件名带 id 后缀，两个同名工作流不会共用文件。
- 删除工作流会拦住在途任务，顺序为先删库、后删文件。
- 默认工作流全局唯一。
- 提交时**实时读取**当前的 ComfyUI 地址；`generation_tasks.comfyui_url` 只是创建时的审计快照。

**提示词与负向**

- 负向提示词配置在**工作流**上，不在提交接口里。同一个工作流的所有任务共用同一份负向。
- 部分工作流的负向由正向派生（`negative_shared: true`），此时负向不会生效，接口会明确提示。

**任务**

- 创建任务是异步的，返回 202 不代表已提交给 ComfyUI。
- 出图成功但保存失败时任务不会立刻判失败，而是保持 `submitted` 自动重试，连续 40 次失败才终止。
- 已成功的结果在重试时不会被重跑。

**收藏联动**

- 收藏状态在「提示词」与「图片」两个层级之间级联：改任一侧会同步另一侧。
- 手动提交的图片没有关联提示词，收藏只影响自身。

**JSON 导入**

- 分组名 = 文件名；去重键为「同组 + 同内容」。
- 同名文件重复导入是**追加**，改内容后请换文件名。

**已知限制**

- **子图（Subgraph）不支持**：含子图的工作流会在保存/提交时被拒绝。
- **无鉴权**：接口不做认证，请勿直接暴露到公网。
- **CORS 实际不生效**：白名单配置项未被读取，不会输出跨域头；跨域访问需自建反向代理。
- `POST /api/prompts/batch-delete` 与 `POST /api/prompts/batch-run` 不校验 500 条上限。

**调试（`DEBUG` 开启时）**

- `DEBUG` 关闭时：不建 `debug/` 目录、不写任何调试文件、`/api/debug/*` 路由不注册、出站请求的代码路径与开启前完全一致（多一层 nil 判断，无额外分配）。
- 调试文件单份上限 16 MiB，超出滚成 `.1`，只保留一代。
- `startup.json` 是**覆盖写**（只留最新一份），其余三个是追加写的 JSONL。
- 写盘失败一律吞掉，**绝不影响出图**；`/api/debug/files/{name}` 走白名单，任何含 `/` 或 `\` 的名字都会被拒。
