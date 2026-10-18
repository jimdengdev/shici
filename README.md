# 诗词雅集 · 公益中国诗词网站

基于 **Iris (Go)** 的公益中国诗词站点：后端直接生成 API 与 HTML 页面，内置后台管理系统，前台支持诗人检索与关键词检索。诗词文本均为公有领域，免费开放。

## 快速开始

```bash
cp .env.example .env && chmod 600 .env   # 填写后台密码与 MySQL 连接串
go build -o server ./cmd/server
./server                       # http://localhost:8080（自动读取同目录 .env）
```

`.env` 已被 `.gitignore` 忽略，凭据不会进仓库；容器与 systemd 注入的环境变量优先于 `.env`。

首次启动自动建库并灌入内置种子数据：**117 首名篇 + 51 位诗人小传**（先秦至清，诗 / 词 / 散曲 / 乐府）。

### 存储：MySQL（poetry_charity）/ SQLite（回退）

默认使用 SQLite（`data/shici.db`，`modernc.org/sqlite` 纯 Go 无 CGO），开箱即用。

设置 `SHICI_MYSQL_DSN`（或 `DATABASE_URL`）即切换到远端 MySQL 库，直接使用现有 `poems` 表数据。推荐把连接串写进 `.env`（不入库）：

```bash
# .env
SHICI_MYSQL_DSN=mysql://用户:密码@数据库主机:22213/poetry_charity
SHICI_MYSQL_DB=poetry_charity
ADMIN_USER=admin
ADMIN_PASS=你的后台密码
```

```bash
./server            # 自动读取 .env
```

连接串支持 `mysql://user:pass@host:port/db` 与原生 `user:pass@tcp(host:port)/db` 两种写法；省略库名时默认 `poetry_charity`（可用 `SHICI_MYSQL_DB` 覆盖）。临时覆盖可用环境变量，但注意命令会留在 shell 历史里。

数据表结构与业务字段的对应关系（`poems` 表）：

| 表字段 | 含义 | 站点用途 |
|---|---|---|
| `title` / `author` / `dynasty` | 标题 / 作者 / 朝代 | 展示与检索，作者即诗人 |
| `category` | 体裁（唐诗 / 宋词 / 元曲…） | 「体裁」筛选 |
| `content` | 正文 | 逐行排版（每行一句） |
| `translation` / `annotation` / `appreciation` | 译文 / 注释 / 赏析 | 详情页分块展示 |
| `tags` | JSON 数组标签 | 标签展示与 `?tag=` 筛选（读入时转为逗号分隔） |
| `featured` | 是否精选 | 首页「名篇撷英」优先 |
| `status` | published / draft / archived | 站点只展示 `published` |
| `view_count` / `favorite_count` / `comment_count` | 统计计数 | 预留（对应用户侧收藏、评论功能） |

- **诗人没有独立表**：`/poets` 由 `poems.author + dynasty` 聚合派生（按作品数排序，最多 300 位），诗人小传取自内置的 51 位名家小传；后台「诗人管理」的编辑即批量改写其作品的作者/朝代
- 首次连接自动建表（`IF NOT EXISTS`，不会影响已有数据）；库为空时灌入内置种子数据
- 已兼容远端表上的唯一键 `uk_poems_dedup(dynasty,title,author)`：重复作品保存会报错
- 环境变量：`PORT`、`SHICI_HOST`（监听地址，置于反向代理后设为 `127.0.0.1`）、`SHICI_DB`（SQLite 路径）、`SHICI_MYSQL_DSN` / `SHICI_MYSQL_DB`（二选一）、`ADMIN_USER` / `ADMIN_PASS` / `ADMIN_HASH`
- 配置来源：优先环境变量，未设置时自动加载同目录 `.env`（`SHICI_ENV_FILE` 可指定其他路径）
- 数据库权限：`poems` 表已存在时不再执行任何 DDL，**只读账号（仅 SELECT）即可运行前台**；后台维护需要 `INSERT/UPDATE`，若账号只读，启动日志会打印提示，后台保存返回数据库的权限错误
- **删除即改状态**：后台「删除」不做物理 `DELETE`，而是把 `status` 置为 `archived`；站点只展示 `published`，`archived` 记录保留在库中但不再出现在列表、详情、检索与统计里（需要时可把 `status` 改回 `published` 恢复）

## 前台功能

| 路由 | 说明 |
|---|---|
| `/` | 今日诗词（按日期轮换）+ 名篇撷英 + 收录统计 |
| `/poems` | 诗词检索：**关键词**（诗题/诗句/译文/注释/诗人，MySQL 走 ngram 全文索引）× **诗人** × 朝代 × 体裁，分页 |
| `/poems/{id}` | 详情：正文分行排版、译文、注释、赏析、标签、上一首/下一首 |
| `/poets` `/poets/{name}` | 诗人检索（姓名/朝代）、小传与作品（最多 200 首） |
| `/random` | 随机跳转一首 |
| `/about` | 公益说明与 API 文档 |

## 开放 API（JSON）

只开放三个接口，返回均为 UTF-8 JSON：

```
GET /api/random                      # 随机一首诗（整首）
GET /api/random/line                 # 随机一首诗中的一句
GET /api/poem?title=静夜思           # 按诗名取整首诗（同名可加 &author=李白 消歧）
```

- `/api/random`、`/api/poem` 返回诗对象：`id / title / author / dynasty / category / content / lines[] / translation / annotation / appreciation / tags[] / url`，空字段省略
- `/api/random/line` 返回：`{"line": "举头望明月，", "poem": {诗对象}}`，`poem` 为该句所属作品
- 出错返回 `{"error": "..."}`：缺 `title` 为 400，未找到为 404，触发限流为 429
- **限流**：三个接口合计 **20 QPS**（进程内令牌桶，突发容量 20，见 `internal/web/limit.go`）；超限返回 429 并带 `Retry-After: 1`、`X-RateLimit-Limit: 20`。页面路由不受限；多实例部署时需改为共享计数或在反向代理侧限流

```bash
curl 'http://localhost:8080/api/random'
curl 'http://localhost:8080/api/random/line'
curl 'http://localhost:8080/api/poem?title=%E9%9D%99%E5%A4%9C%E6%80%9D'
```

> 随机接口每次结果不同，不缓存；按诗名接口按 URI 缓存（见上文缓存说明）。

## 后台管理

- 入口 `/admin`，默认账号 `admin` / `shici@2024`（**上线前务必用 `ADMIN_USER` / `ADMIN_PASS` 环境变量修改**，或设 `ADMIN_HASH` 为 bcrypt 哈希）
- session 登录（Iris sessions，12 小时有效期）
- 仪表盘：收录统计、最近收录、按朝代分布
- 诗词管理 / 诗人管理：筛选、分页、新增、编辑、删除（删诗人会级联删其作品）

## 技术要点

- **Iris v12**：路由 / 视图引擎（`view.HTML`，内嵌模板 + 布局 `{{ yield . }}`）/ sessions
- **双存储后端**：SQLite（`modernc.org/sqlite` 纯 Go 无 CGO，默认）与 MySQL（`go-sql-driver/mysql`），方言差异（DDL / 连接池 / JSON 标签）收敛在 `internal/store`，业务层无感
- **关键词检索走 ngram 全文索引**：`poems` 表已有 `ft_poems_search(title,author,content,translation,annotation) WITH PARSER ngram`，连接时自动探测（自建库自动补建）；查询用 `MATCH … AGAINST(? IN BOOLEAN MODE)` 过滤、按 `NATURAL LANGUAGE MODE` 相关度排序，单字关键词或索引不可用时自动降级 `LIKE`
- **诗人派生**：无诗人表，`author` 聚合出诗人列表与作品数；诗人详情路由以姓名作为标识
- **元数据缓存**：朝代/体裁列表（10 分钟）与收录统计（1 分钟）短 TTL 缓存，写操作后立即失效
- **零外部资源**：模板、样式全部 `go:embed` 内嵌，单二进制部署；古典水墨风 CSS
- 今日诗词按日期确定性轮换（同一天稳定、跨天更换）；随机/每日取词用主键区间采样，避免 `ORDER BY RAND()` 全表排序

## 目录结构

```
cmd/server/main.go     入口（组装路由并启动）
internal/model         数据模型
internal/store         数据访问层（poems 表检索/分页/CRUD/统计，诗人聚合，双方言）
internal/web           前台：模板+静态资源内嵌，页面与 API
internal/admin         后台：登录鉴权与 CRUD
data/seed.go           公有种子数据（117 首诗词 + 51 位诗人）
```

## 启动脚本

服务器上直接用 `start.sh`（自动读取同目录 `.env`）：

```bash
chmod +x start.sh

./start.sh start             # 构建并后台启动
./start.sh start --no-build  # 直接启动已编译的 ./server
./start.sh status            # PID / 内存 / 实际监听端口
./start.sh logs              # 跟踪日志（Ctrl+C 退出）
./start.sh restart
./start.sh stop              # 先 SIGTERM，超时再 SIGKILL
./start.sh build             # 仅编译
```

- PID、端口、日志记录在 `.tmp/`（已列入 `.gitignore`）
- 环境变量优先于 `.env`，可临时覆盖：`PORT=8081 ./start.sh restart --no-build`
- 未安装 go 时可用 `GO=/usr/local/go/bin/go ./start.sh build` 指定

### systemd

仓库提供 `deploy/shici.service` 模板（默认路径 `/opt/shici`、用户 `shici`，按需修改）：

```bash
sudo cp deploy/shici.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now shici
sudo systemctl status shici
```

## 服务器部署（二进制 + Caddy 反向代理）

适合 Ubuntu/Debian。应用只监听回环地址，**Caddy 是唯一对外入口**，应用端口不对外暴露。

```bash
# 1. 本地交叉编译（静态二进制，无 CGO 依赖）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o server ./cmd/server

# 2. 上传到服务器 /opt/shici，写好 .env
#    SHICI_HOST=127.0.0.1
#    PORT=<应用内部端口>
#    chown shici:shici /opt/shici/.env && chmod 600 /opt/shici/.env

# 3. 安装为 systemd 服务（模板见 deploy/shici.service）
sudo cp deploy/shici.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now shici

# 4. 安装 Caddy 并配置反向代理
sudo apt-get install -y caddy
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudo cp deploy/caddy.env.example /etc/caddy/caddy.env   # 按实际修改
sudo mkdir -p /etc/systemd/system/caddy.service.d
printf '[Service]\nEnvironmentFile=-/etc/caddy/caddy.env\n' \
  | sudo tee /etc/systemd/system/caddy.service.d/env.conf
sudo systemctl daemon-reload && sudo systemctl restart caddy
```

要点：

- `deploy/Caddyfile` 里不写死地址：`SITE_ADDRESS` 与 `SHICI_UPSTREAM` 都来自 `/etc/caddy/caddy.env`
- 先用 `SITE_ADDRESS=:80` 通过 IP 访问；**配好域名后把 `SITE_ADDRESS` 改成域名**，Caddy 自动申请续期 HTTPS（HTTP 请求自动 308 跳转），无需改配置文件
- 套 Cloudflare 代理（橙云）时，SSL/TLS 模式请选 **Full (strict)**：源站已是有效证书，选 Flexible 会因为源站强制 HTTPS 造成重定向循环；仅 DNS（灰云）则无需额外设置
- 应用侧 `SHICI_HOST=127.0.0.1` + 端口只在回环监听，外网无法直连应用端口
- 验证：`curl -I http://<站点>`（HEAD 也返回 200）、`nc -z <站点> <应用端口>` 应失败
- 日志：应用 `journalctl -u shici -f`，访问日志 `/var/log/caddy/shici-access.log`

## MySQL 优化（1 核 1G / 20G）

34 万行数据在 1C1G 上跑 MySQL，瓶颈几乎都在「列表页排序」与「全表聚合」，按下面两步做即可稳定。

```bash
# 1. 补建复合索引（幂等，可重复执行；需要 poems 表的 ALTER 权限）
mysql -u<用户> -p poetry_charity < deploy/mysql-optimize.sql

# 2. 应用 MySQL 参数模板（1Panel：容器 → 参数 → 配置文件；系统包：/etc/mysql/mysql.conf.d/）
sudo cp deploy/mysql-1c1g.cnf /etc/mysql/mysql.conf.d/zz-1c1g.cnf
sudo systemctl restart mysql
```

> 云数据库（腾讯云 CynosDB / RDS 等）无法直接改 `my.cnf`：在控制台「参数设置 / 参数组」里按 `deploy/mysql-1c1g.cnf` 的键值逐项调整。应用账号只有 `SELECT/INSERT/UPDATE/DELETE` 时无法加索引（`ALTER command denied`），需用具备 ALTER 权限的账号执行上面的 SQL。

做了什么：

- **复合索引**：`(status, id)` / `(status, dynasty, id)` / `(status, category, id)` / `(status, featured, id)` / `(author, id)` / `(status, author, dynasty)`。列表页固定 `WHERE status='published' ORDER BY id`，只有单列 `idx_poems_status` 时会退化成「全表取 + filesort + 临时表」，这也是 CPU 打满的主因；补上后分页变成索引顺序扫描，`LIMIT/OFFSET` 不再排序
- **缓冲池**：`innodb_buffer_pool_size=256M`（1G 机器约 25%，留足给应用与系统），热数据命中内存后磁盘 IO 与 CPU 都明显下降
- **关闭 `performance_schema`**：1G 机器可省 100~300M 常驻内存；问题排查交给慢日志（`long_query_time=1`）
- **`innodb_flush_log_at_trx_commit=2`**：崩溃最多丢 1 秒事务，换写入吞吐；对数据安全性要求高时改回 `1`
- **应用侧**：检索总数（`COUNT(*)`）缓存 30s、诗人榜（`GROUP BY author`）缓存 5 分钟，后台写入时自动失效；MySQL 连接池空闲 5 分钟后归还

验证：

```sql
SHOW INDEX FROM poems;                       -- 六个复合索引齐全
EXPLAIN SELECT id FROM poems WHERE status='published' ORDER BY id LIMIT 20;
-- 期望 key = idx_poems_status_id，且 Extra 中没有 Using filesort / Using temporary
```

> 说明：全文检索依赖 `ft_poems_search`（ngram），单字关键词仍会降级 `LIKE '%词%'` 全表扫描，这是预期行为，短关键词请尽量少用。

### 缓存（内容极少变化，长 TTL + 事件失效）

两层进程内缓存（`internal/memo`，TTL 带抖动、单飞回源、过期先返回旧值再后台刷新）：

| 层 | 内容 | TTL |
|---|---|---|
| 数据层 | 朝代/体裁 24h · 统计 6h · 诗人榜 12h · 首页名篇 6h · 主键区间 6h · 检索总数 30min · 按诗名 30min | 见 `store.go` |
| 随机池 | 随机整首 / 随机一句：一次采样 64 个随机起点入池，逐个消费，余量不足后台补 | `store.go` 的 `pool[T]` |
| 页面层 | 渲染后的 HTML/JSON（按 URI，30min，32MiB LRU） | `internal/web/cache.go` |

开放 API 的缓存策略：

- `/api/random`、`/api/random/line`：结果每次不同，**不能按 URI 缓存**，改为「随机池」前置缓存——一次回源采样 64 个随机起点（每起点取一首，4 路并发）填入池，之后每个请求只从池里取一个，余量不足一半时后台异步补一批；数据库往返降为约 1/64，随机接口响应 `Cache-Control: no-store`，避免被代理/CDN 缓存成同一首
- `/api/poem?title=…`：数据层按「诗名+作者」缓存 30min（含「查无此诗」的负结果，避免重复回源），外层再叠加按 URI 的页面缓存，命中即零回源
- 随机池与数据缓存均已登记到 `memo`：后台写入或 `SIGUSR1` 时一并清空

- 命中情况看响应头 `X-Cache: HIT/MISS`
- 后台增删改会立即清空全部缓存；批量导入数据后手动清：

```bash
systemctl kill -s SIGUSR1 shici
```

## 部署提示

- 单二进制 + SQLite 文件即可运行，适合小内存 VPS / 容器
- 建议用 systemd / Docker 常驻，反向代理套 HTTPS
- 站点为公益用途：无广告、无追踪，欢迎传播与二次开发

### 配置与密钥管理

所有凭据（MySQL 连接串、后台密码）只写在服务器上的 `.env` 里：

```bash
cp .env.example .env && chmod 600 .env && vi .env
```

- `.env` 已列入 `.gitignore`，不会被提交；`.dockerignore` 也已排除，不会进入镜像构建上下文
- `.env.example` 只保留占位符，可安全提交
- 程序自动读取同目录 `.env`；`systemd` / `docker compose` 注入的环境变量优先级更高
- 仓库中**不要**出现真实主机、账号或密码；一旦误提交，请立即轮换数据库口令与后台密码

## 部署到 1Panel（Docker 编排）

仓库已内置 `Dockerfile` + `compose.yml` + `.env.example`：

```bash
# 1. 在服务器上拉取代码
git clone https://github.com/jimdengdev/shici.git && cd shici

# 2. 准备环境变量（后台密码 + MySQL 连接）
cp .env.example .env && chmod 600 .env && vi .env
#    DSN 直接填远端库地址，例如：
#    mysql://用户:密码@数据库主机:22213/poetry_charity
#    （.env 只在服务器上，不入库）

# 3a. 直接启动（或用 1Panel → 容器 → 编排 → 创建编排指向本目录）
docker compose up -d --build

# 3b. 也可以用 1Panel 网页：容器 → 编排 → 创建编排 → 路径选本目录
```

要点：

- MySQL 库不存在会**自动建库**（utf8mb4）并灌入种子数据；也可以先用 `go run ./cmd/mkdb` 手动建库
- 连接已有 `poetry_charity` 库时，`poems` 表按 `IF NOT EXISTS` 建表、已有 34 万首数据原样读取
- `compose.yml` 已配置 `host.docker.internal:host-gateway`，容器内可直达宿主机 MySQL
- 不设置 `SHICI_MYSQL_DSN` 时自动回退容器内 SQLite，`./data` 目录已挂载持久化
- 反向代理：1Panel → 网站 → 创建网站，代理到 `http://127.0.0.1:8080` 即可套 HTTPS
