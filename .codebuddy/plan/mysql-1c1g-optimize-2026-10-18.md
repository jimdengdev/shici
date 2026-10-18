# 诗词站 MySQL 优化方案（1 核 1G / 20G）

- 日期：2026-10-18
- 范围：线上 MySQL（1Panel 安装，库 `poetry_charity`，表 `poems`）+ Go 应用数据访问层
- 目标机型：1 vCPU / 1G RAM / 20G 磁盘（单机部署，应用与 Caddy 同机）

## 一、现状与诊断

监控基线（截图，2026-10-18）：

| 指标 | 观测值 | 判断 |
| --- | --- | --- |
| CPU | 当前 0.83%，历史尖峰触顶 100% | 周期性尖峰，非持续负载 |
| 内存 | 35.9%（≈360M/1G） | 尚有余量，但 `performance_schema` + 默认缓冲池吃掉了不该占的部分 |
| 存储 | 1.1G / 20G（6%） | 磁盘不是瓶颈，可放心加索引 |
| 请求 | 2 次/秒 | 访问量很小，说明尖峰由「单条重查询」而非并发造成 |

数据量：`poems` 约 34 万行；含 ngram 全文索引 `ft_poems_search`（title/author/content/translation/annotation 五列）。

代码侧的慢查询根因（`internal/store/store.go`）：

1. **列表页排序退化为全表 filesort**：`WHERE p.status='published' ORDER BY p.id LIMIT ? OFFSET ?`，只有单列 `idx_poems_status`，MySQL 取回 34 万行再排序 + 临时表 → CPU 打满的主因。
2. **每次翻页都做 COUNT(*)**：`SELECT COUNT(*) FROM poems p WHERE ...` 与列表查询重复扫描。
3. **诗人榜全表聚合无缓存**：`GROUP BY p.author, p.dynasty ORDER BY cnt DESC LIMIT 300`，每次访问 `/poets` 重跑。
4. **每个列表行取全量 TEXT**：`poemCols` 含 content/translation/annotation/appreciation，列表页只用到 `Excerpt`（由 content 派生）。
5. **不可索引条件**：`CAST(p.tags AS CHAR) LIKE '%x%'`、`p.author LIKE '%x%'`、单字关键词降级 `LIKE`，均为全表扫描。
6. **连接池**：`maxOpen=10`，空闲连接不回收，小内存机器上常驻占用不必要。

## 二、目标（验收指标）

| 指标 | 现状 | 目标 |
| --- | --- | --- |
| 列表页 `/poems` 查询耗时 P95 | 数百 ms（filesort） | < 50ms |
| `EXPLAIN` 列表查询 | `Using filesort` / `Using temporary` | 命中 `idx_poems_status_id`，无 filesort |
| MySQL 常驻内存 | ≈360M | < 480M（buffer pool 256M 计） |
| CPU 尖峰 | 触顶 100% | 持续 < 30%，无尖峰 |
| `/poets` 首次/重复访问 | 每次全表聚合 | 首次 < 500ms，重复命中缓存 < 5ms |

## 三、方案分层

### P0 索引层（收益最大，先做）

新增 6 个复合索引，覆盖站点全部高频访问路径：

| 索引 | 服务查询 |
| --- | --- |
| `idx_poems_status_id (status, id)` | 列表分页、上/下一首、最近收录 |
| `idx_poems_status_dynasty_id (status, dynasty, id)` | 按朝代筛选 |
| `idx_poems_status_category_id (status, category, id)` | 按体裁筛选 |
| `idx_poems_status_featured_id (status, featured, id)` | 首页「名篇撷英」`ORDER BY featured DESC, id` |
| `idx_poems_author_id (author, id)` | 诗人详情页作品列表 |
| `idx_poems_status_author_dynasty (status, author, dynasty)` | 诗人榜 `GROUP BY`（覆盖索引，免临时表） |

落地方式（两处已就绪，二选一或都保留）：

- `deploy/mysql-optimize.sql`：幂等脚本（`information_schema.statistics` 判存 + `PREPARE/EXECUTE`），可重复执行，末尾 `ANALYZE TABLE`
- 应用启动自动补建：`ensureIndexes()`，历史库首次启动即补齐；只读账号无 ALTER 权限时静默跳过

后续可选瘦身：确认业务无依赖后删除被覆盖的单列索引 `idx_poems_status` / `idx_poems_dynasty` / `idx_poems_category` / `idx_poems_author`，减少写放大与索引体积（脚本中已注释给出）。

### P1 参数层（`deploy/mysql-1c1g.cnf`）

| 参数 | 建议值 | 理由 |
| --- | --- | --- |
| `innodb_buffer_pool_size` | 256M | 1G 机器约 25%，热数据命中内存 |
| `innodb_redo_log_capacity` | 128M | 8.0.30+ 写法；以下用 `innodb_log_file_size=64M`，二者不可同时设置 |
| `innodb_flush_log_at_trx_commit` | 2 | 每秒刷盘，崩溃最多丢 1s 事务，换写入吞吐（介意改回 1） |
| `performance_schema` | OFF | 省 100~300M 常驻内存，排查靠慢日志 |
| `max_connections` | 30 | 连接池上限 10，够用；连接越少越省内存 |
| `tmp_table_size` / `max_heap_table_size` | 32M | 诗人榜 GROUP BY 尽量走内存临时表 |
| `sort_buffer_size` / `join_buffer_size` | 1M | 按连接分配，小内存禁止调大 |
| `long_query_time` | 1 | 开启慢日志，定位未走索引的查询 |
| `ngram_token_size` | 2 | 与既有全文索引一致，改动需重建索引 |

### P2 应用层（已实现）

- 检索总数缓存 30s（`countTTL`，上限 128 条，超限整体重建，后台写入 `invalidateMeta()` 清空）
- 诗人榜缓存 5 分钟（`poetTTL`，按 keyword+dynasty 分键）
- 连接池 `SetConnMaxIdleTime(5m)`，空闲连接归还
- 已有：朝代/体裁 `DISTINCT` 缓存 10 分钟、后台统计缓存 1 分钟

### P3 可选深入（按需，当前流量下非必须）

1. **列表页列裁剪**：新增 `poemListCols`（`LEFT(content, 200) AS content`，translation/annotation/appreciation 置空），减少约 70% 传输量。需配套独立 scan 函数，改动面中等
2. **深分页**：`LIMIT ? OFFSET ?` 在 OFFSET > 1 万时仍偏慢；可限制最大页数或改「游标分页」（`WHERE id > ?` 续页）
3. **tags 检索**：`CAST(tags AS CHAR) LIKE '%x%'` 无法索引；MySQL 8.0.17+ 可加多值索引 `CAST(tags AS CHAR(24) ARRAY)` + `JSON_CONTAINS` 改写
4. **作者模糊匹配**：`author LIKE '%李白%'` 全表扫；若产品可接受，改为前缀匹配 `LIKE '李白%'` 即可走 `idx_poems_author_id`
5. **ngram 索引体积**：五列全文索引占磁盘不小，若只按诗题/诗句检索，可评估缩到 `title, content` 两列（需重建索引，约数分钟）

### P4 运维层

- 慢日志每周巡检：`pt-query-digest` 或 `SELECT * FROM sys.statements_with_full_table_scans`
- 监控阈值：CPU > 70% 持续 5 分钟、内存 > 80%、慢查询 > 5 条/分钟 触发告警
- 备份：每日 `mysqldump --single-transaction` 全库，保留 7 天（20G 盘足够）
- 上线前先在从库/快照验证 `ALTER TABLE`（加索引为 Online DDL，34 万行约几十秒到数分钟）

## 四、执行步骤与顺序

| 步骤 | 操作 | 预计耗时 | 风险 |
| --- | --- | --- | --- |
| 1 | 备份：`mysqldump --single-transaction poetry_charity > backup.sql` | 1~2 min | 无 |
| 2 | 执行 `deploy/mysql-optimize.sql`（含 ANALYZE） | 1~5 min | 低（Online DDL，期间可读） |
| 3 | 套用 `deploy/mysql-1c1g.cnf` 并重启 MySQL | 1 min 停机 | 中（参数错误会导致启动失败，需可回滚配置） |
| 4 | 重启应用（触发 `ensureIndexes` 兜底 + 缓存生效） | 10 s | 低 |
| 5 | 验收：`EXPLAIN` + 页面耗时 + 监控观察 30 分钟 | 30 min | 无 |

回滚：

- 参数：恢复原 `my.cnf` 后重启；`innodb_flush_log_at_trx_commit=2` 可在线改回 1
- 索引：`ALTER TABLE poems DROP INDEX idx_poems_xxx;`（按需单独删除）
- 代码：回退 `store.go` 提交即可，索引与缓存互不依赖

## 五、验收清单

- [ ] `SHOW INDEX FROM poems;` 六个复合索引齐全
- [ ] `EXPLAIN SELECT id FROM poems WHERE status='published' ORDER BY id LIMIT 20;` → `key=idx_poems_status_id`，无 `Using filesort` / `Using temporary`
- [ ] `EXPLAIN SELECT author,dynasty,COUNT(*) FROM poems WHERE status='published' GROUP BY author,dynasty LIMIT 300;` → 覆盖索引 `idx_poems_status_author_dynasty`
- [ ] 连续刷新 `/poems?page=1..50`、`/poets` 各 10 次，MySQL CPU 无明显尖峰
- [ ] 慢日志 24 小时内无超过 1s 的语句
- [ ] 内存长期稳定 < 480M，未触发 OOM killer

## 六、影响面与说明

- 索引和参数对业务完全透明，无接口/字段变更
- 缓存导致的可见延迟上限：检索总数 30s、诗人榜 5 分钟（后台增删改会立即失效）
- 单字关键词、标签检索仍走全表 `LIKE`，属预期行为；如访问量增长再按 P3 处理
