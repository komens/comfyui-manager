# 架构重构方案

## 当前架构问题

1. **JSON 文件是核心**：当前 `prompt_entries` 通过 `json_file_id` 强依赖 `json_files` 表，删除 JSON 文件会导致数据不一致
2. **负面提示词冗余**：每条 `prompt_entry` 和 `generation_item` 都存了 `negative_prompt`，但大多数工作流的负面提示词相同
3. **单条提交不入库**：`createDirectTask` 直接创建任务，不经过 prompt 表，无法统一管理
4. **工作流配置不直观**：用户需要手写 JSON mapping，无法可视化调整 width/height/steps 等参数

---

## 新架构设计

### 核心思路

**提示词是主角，JSON 只是导入工具。**

### 数据模型变更

#### 1. `prompts` 表（原 `prompt_entries`，重建）

```sql
CREATE TABLE prompts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  positive_prompt TEXT NOT NULL,
  group_name TEXT NOT NULL DEFAULT '',
  group_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',  -- pending / done / failed
  completed_at DATETIME,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

- 移除 `json_file_id`、`entry_key`、`negative_prompt` 字段
- 新增 `group_name`（来源文件名）、`group_id`（来源文件名的 slug）
- 单条提交也会插入此表

#### 2. `workflows` 表（新增字段）

```sql
ALTER TABLE workflows ADD COLUMN negative_prompt TEXT NOT NULL DEFAULT '';
```

- 负面提示词绑定到工作流，每个工作流一份通用负面提示词

#### 3. `json_files` 表（保留，简化）

```sql
-- 保留原表结构，仅作为文件备份管理
-- 移除 total_count / completed_count / failed_count 统计字段
CREATE TABLE json_files (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  filename TEXT NOT NULL,
  storage_path TEXT NOT NULL,
  created_at DATETIME NOT NULL
);
```

- 上传时：解析 JSON → 插入 `prompts` 表（带 group_name）→ 保存文件备份
- 列表页：仅显示文件列表，支持上传/查看/下载/删除
- 删除：仅删文件记录，不动 prompts 表

#### 4. `generation_tasks` 表（保持不变）

#### 5. `generation_items` 表（微调）

```sql
-- entry_id 改为 prompt_id，指向 prompts 表
-- negative_prompt 移除，运行时从 workflow 读取
CREATE TABLE generation_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  prompt_id INTEGER,
  positive_prompt TEXT NOT NULL,
  comfy_prompt_id TEXT,
  status TEXT NOT NULL DEFAULT 'queued',
  error_message TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(task_id) REFERENCES generation_tasks(id),
  FOREIGN KEY(prompt_id) REFERENCES prompts(id)
);
```

#### 6. `images` 表（不变）

---

### API 变更

#### 提示词管理 API（新增）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/prompts` | 列表，支持 ?group=xxx&status=xxx 筛选 |
| POST | `/api/prompts` | 新增单条提示词 |
| GET | `/api/prompts/:id` | 详情（含关联图片列表） |
| PUT | `/api/prompts/:id` | 编辑提示词 |
| DELETE | `/api/prompts/:id` | 删除提示词 |
| POST | `/api/prompts/:id/run` | 重跑提示词（指定 workflow_id） |
| GET | `/api/prompts/groups` | 获取所有分组列表 |

#### JSON 文件 API（简化）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/json-files` | 文件列表 |
| POST | `/api/json-files/upload` | 上传（解析入库 + 备份文件） |
| GET | `/api/json-files/:id` | 查看文件内容 |
| GET | `/api/json-files/:id/download` | 下载文件 |
| DELETE | `/api/json-files/:id` | 删除文件（仅删文件记录） |

#### 工作流 API（新增字段）

| 方法 | 路径 | 变更 |
|------|------|------|
| PUT | `/api/workflows/:id` | 支持更新 `negative_prompt` 和 `params_schema` |

---

### 前端页面变更

#### 1. 新增「提示词库」页面（侧边栏一级入口）

- 左侧：分组列表（按 group_name 分组，可筛选）
- 右侧：提示词列表表格
  - 列：标题、正面提示词（截断显示）、状态、关联图片数、操作
  - 操作：查看、编辑、重跑、删除
- 顶部：搜索框 + 添加按钮

#### 2. 提示词详情页 `/prompts/:id`

- 提示词信息（标题、描述、正面提示词）
- 生成历史：所有关联的 generation_items + 对应图片
- 重跑按钮：弹窗选择工作流

#### 3. JSON 文件页面（简化）

- 移除条目列表功能
- 仅保留：上传、文件列表（带下载/删除/查看）

#### 4. 工作流页面（增强）

- 新增「负面提示词」文本框
- 新增「参数可视化编辑」区域
  - 自动解析 workflow_json 中的 KSampler 节点
  - 提供 width、height、steps、cfg 的表单控件
  - 修改后自动更新 workflow_json 和 mapping

#### 5. 直接提交页面（改用 prompt 表）

- 提交后自动插入 prompts 表（标题可为空）
- 页面简化为：选工作流 → 填提示词 → 选参数 → 提交

---

### 工作流参数可视化编辑方案

从导入的 workflow_json 中自动提取 KSampler 类节点的输入字段：

```json
{
  "3": {
    "class_type": "KSampler",
    "inputs": {
      "seed": 0,
      "steps": 20,
      "cfg": 7.0,
      "sampler_name": "euler",
      "scheduler": "normal",
      "denoise": 1.0
    }
  }
}
```

前端自动识别 `class_type` 为 `KSampler` / `KSamplerAdvanced` 的节点，提取以下字段供用户编辑：
- `steps` - 采样步数
- `cfg` - CFG 引导系数
- `seed` - 种子
- `width` / `height` - 从 EmptyLatentImage 节点提取

编辑后同步更新 `workflow_json` 和 `mapping_json`。

---

### 迁移策略

1. 创建新的 `prompts` 表
2. 从 `prompt_entries` 迁移数据到 `prompts`
3. `workflows` 表新增 `negative_prompt` 列
4. 更新 `generation_items` 表结构
5. 保留 `json_files` 表，简化字段
6. 逐步废弃 `prompt_entries` 表
