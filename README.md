# 诗词雅集 · 公益中国诗词网站

基于 **Iris (Go)** 的公益中国诗词站点：后端直接生成 API 与 HTML 页面，内置后台管理系统，前台支持诗人检索与关键词检索。诗词文本均为公有领域，免费开放。

## 快速开始

```bash
go build -o server ./cmd/server
./server                       # http://localhost:8080（默认 SQLite，零依赖）
```

首次启动自动建库并灌入内置种子数据：**117 首名篇 + 51 位诗人小传**（先秦至清，诗 / 词 / 散曲 / 乐府）。

### 存储：SQLite（默认）/ MySQL

默认使用 SQLite（`data/shici.db`，`modernc.org/sqlite` 纯 Go 无 CGO），开箱即用。

设置 `SHICI_MYSQL_DSN` 即切换到 MySQL（需 MySQL 5.7+ / 8.0+，InnoDB，utf8mb4）：

```bash
# 建库：CREATE DATABASE shici CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
SHICI_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/shici?charset=utf8mb4&parseTime=true&loc=Local' \
PORT=8080 ADMIN_USER=admin ADMIN_PASS=你的密码 ./server
```

- 首次连接自动建表、自动灌入种子数据（两种后端行为一致）
- 删诗人会级联删除其名下作品（两种后端均启用外键）
- 环境变量：`PORT`、`SHICI_DB`（SQLite 路径）、`SHICI_MYSQL_DSN`（二选一）、`ADMIN_USER` / `ADMIN_PASS` / `ADMIN_HASH`

## 前台功能

| 路由 | 说明 |
|---|---|
| `/` | 今日诗词（按日期轮换）+ 名篇撷英 + 收录统计 |
| `/poems` | 诗词检索：**关键词**（诗题/诗句/标签）× **诗人** × 朝代 × 体裁，分页 |
| `/poems/{id}` | 详情：正文分行排版、题解、标签、上一首/下一首 |
| `/poets` `/poets/{id}` | 诗人检索（姓名/小传/朝代）、生平与全部作品 |
| `/random` | 随机跳转一首 |
| `/about` | 公益说明与 API 文档 |

## 开放 API（JSON）

```
GET /api/poems?keyword=明月&poet=李白&dynasty=唐&genre=词&page=1
GET /api/poems/{id}
GET /api/poets?keyword=苏轼
GET /api/poets/{id}          # 含诗人全部作品
GET /api/search?q=黄河       # 检索别名
```

## 后台管理

- 入口 `/admin`，默认账号 `admin` / `shici@2024`（**上线前务必用 `ADMIN_USER` / `ADMIN_PASS` 环境变量修改**，或设 `ADMIN_HASH` 为 bcrypt 哈希）
- session 登录（Iris sessions，12 小时有效期）
- 仪表盘：收录统计、最近收录、按朝代分布
- 诗词管理 / 诗人管理：筛选、分页、新增、编辑、删除（删诗人会级联删其作品）

## 技术要点

- **Iris v12**：路由 / 视图引擎（`view.HTML`，内嵌模板 + 布局 `{{ yield . }}`）/ sessions
- **双存储后端**：SQLite（`modernc.org/sqlite` 纯 Go 无 CGO，默认）与 MySQL（`go-sql-driver/mysql`），方言差异（DDL / `RANDOM()` vs `RAND()` / 连接池）收敛在 `internal/store`，业务层无感
- **零外部资源**：模板、样式全部 `go:embed` 内嵌，单二进制部署；古典水墨风 CSS
- 今日诗词按日期确定性轮换（同一天稳定、跨天更换）

## 目录结构

```
cmd/server/main.go     入口（组装路由并启动）
internal/model         数据模型
internal/store         SQLite 数据访问层（检索/分页/CRUD/统计）
internal/web           前台：模板+静态资源内嵌，页面与 API
internal/admin         后台：登录鉴权与 CRUD
data/seed.go           公有种子数据（117 首诗词 + 51 位诗人）
```

## 部署提示

- 单二进制 + SQLite 文件即可运行，适合小内存 VPS / 容器
- 建议用 systemd / Docker 常驻，反向代理套 HTTPS
- 站点为公益用途：无广告、无追踪，欢迎传播与二次开发
