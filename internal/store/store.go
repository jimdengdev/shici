// Package store 数据访问层，直接适配远端 MySQL 库（poetry_charity）的 poems 表结构：
//
//	poems(id, title, author, dynasty, category, content,
//	      translation, annotation, appreciation, tags(JSON), featured,
//	      view_count, favorite_count, comment_count, status,
//	      created_by, created_at, updated_at)
//
// 库中没有诗人表，诗人由 poems.author/dynasty 聚合派生；诗人小传取自内置
// data.Poets（未收录的诗人显示为空）。SQLite 作为零依赖的本地开发后端，
// 使用同一套字段语义（JSON 标签以文本存储）。
//
// 关键词检索优先使用 poems 表的 ngram 全文索引 ft_poems_search
// （title, author, content, translation, annotation），连接时自动探测，
// 自建库自动补建；索引不可用或关键词短于 ngram 分词长度时降级 LIKE。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql" // MySQL 驱动（纯 Go）
	_ "modernc.org/sqlite"           // 纯 Go SQLite 驱动

	"shici/data"
	"shici/internal/memo"
	"shici/internal/model"
)

// DefaultMySQLDatabase 连接串未指定库名时使用的默认库。
const DefaultMySQLDatabase = "poetry_charity"

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS poems (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  title          TEXT NOT NULL,
  author         TEXT NOT NULL,
  dynasty        TEXT NOT NULL DEFAULT '',
  category       TEXT NOT NULL DEFAULT '古诗',
  content        TEXT NOT NULL DEFAULT '',
  translation    TEXT NOT NULL DEFAULT '',
  annotation     TEXT NOT NULL DEFAULT '',
  appreciation   TEXT NOT NULL DEFAULT '',
  tags           TEXT DEFAULT NULL,
  featured       INTEGER NOT NULL DEFAULT 0,
  view_count     INTEGER NOT NULL DEFAULT 0,
  favorite_count INTEGER NOT NULL DEFAULT 0,
  comment_count  INTEGER NOT NULL DEFAULT 0,
  status         TEXT NOT NULL DEFAULT 'published',
  created_by     INTEGER DEFAULT NULL,
  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_poems_dedup ON poems(dynasty, title, author);
CREATE INDEX IF NOT EXISTS idx_poems_dynasty ON poems(dynasty);
CREATE INDEX IF NOT EXISTS idx_poems_category ON poems(category);
CREATE INDEX IF NOT EXISTS idx_poems_status ON poems(status);
CREATE INDEX IF NOT EXISTS idx_poems_featured ON poems(featured, created_at);
CREATE INDEX IF NOT EXISTS idx_poems_author ON poems(author);
`

const mysqlSchema = `
CREATE TABLE IF NOT EXISTS poems (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  title          VARCHAR(100) NOT NULL COMMENT '标题',
  author         VARCHAR(60) NOT NULL COMMENT '作者',
  dynasty        VARCHAR(24) NOT NULL COMMENT '朝代',
  category       VARCHAR(24) NOT NULL DEFAULT '古诗' COMMENT '体裁分类',
  content        TEXT NOT NULL COMMENT '正文',
  translation    TEXT NOT NULL COMMENT '译文',
  annotation     TEXT NOT NULL COMMENT '注释',
  appreciation   TEXT NOT NULL COMMENT '赏析',
  tags           JSON DEFAULT NULL COMMENT '标签数组',
  featured       TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否精选',
  view_count     BIGINT NOT NULL DEFAULT 0 COMMENT '阅读量',
  favorite_count INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '收藏数',
  comment_count  INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '评论数',
  status         ENUM('draft','published','archived') NOT NULL DEFAULT 'published' COMMENT '状态',
  created_by     BIGINT UNSIGNED DEFAULT NULL COMMENT '创建者',
  created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_poems_dedup (dynasty, title, author),
  KEY idx_poems_dynasty (dynasty),
  KEY idx_poems_category (category),
  KEY idx_poems_status (status),
  KEY idx_poems_featured (featured, created_at),
  KEY idx_poems_author (author),
  KEY idx_poems_status_id (status, id),
  KEY idx_poems_status_dynasty_id (status, dynasty, id),
  KEY idx_poems_status_category_id (status, category, id),
  KEY idx_poems_status_featured_id (status, featured, id),
  KEY idx_poems_author_id (author, id),
  KEY idx_poems_status_author_dynasty (status, author, dynasty)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='古诗词表'
`

// mysqlIndexes 站点查询依赖的复合索引：新库由 DDL 直接建好，
// 已存在的库（多为历史库）启动时按需补建，避免列表页 ORDER BY 触发全表 filesort。
var mysqlIndexes = []struct{ name, spec string }{
	{"idx_poems_status_id", "(status, id)"},
	{"idx_poems_status_dynasty_id", "(status, dynasty, id)"},
	{"idx_poems_status_category_id", "(status, category, id)"},
	{"idx_poems_status_featured_id", "(status, featured, id)"},
	{"idx_poems_author_id", "(author, id)"},
	{"idx_poems_status_author_dynasty", "(status, author, dynasty)"},
}

// dialect 方言差异：驱动、DDL、连接池与旧表兼容策略。
type dialect struct {
	driver        string
	ddl           string
	maxOpen       int
	tableExists   string // 判断 poems 表是否存在的查询（返回 1 行即存在）
	dropLegacyDDL bool   // 旧版自有 schema（无 author 列）是否可直接丢弃重建
	fulltextIndex string // 全文索引名（空 = 该方言不支持全文检索，走 LIKE）
}

var (
	dialectSQLite = dialect{
		driver: "sqlite",
		ddl:    sqliteSchema,
		// SQLite 串行化写入，避免 database is locked
		maxOpen:     1,
		tableExists: `SELECT 1 FROM sqlite_master WHERE type='table' AND name='poems'`,
		// 本地库由种子数据重建，旧结构可直接丢弃
		dropLegacyDDL: true,
	}
	dialectMySQL = dialect{
		driver:        "mysql",
		ddl:           mysqlSchema,
		maxOpen:       10,
		tableExists:   `SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'poems'`,
		dropLegacyDDL: false,
		// 与远端 poems 表一致：ngram 全文索引（中文分词）
		fulltextIndex: "ft_poems_search",
	}
)

// publishedCond 站点展示的有效记录：published 为在架，archived 视为已删除（软删除）。
const publishedCond = `p.status = 'published'`

// 全文索引列（与 ft_poems_search 的列集合严格一致，MySQL 要求）。
const fulltextCols = `title, author, content, translation, annotation`

// Filter 诗词检索过滤条件。
type Filter struct {
	Keyword  string // 关键词：走 ngram 全文索引（MySQL）/ LIKE（SQLite）
	Poet     string // 作者（模糊匹配）
	Dynasty  string
	Genre    string // 体裁（对应 category 列）
	Tag      string
	Page     int
	PageSize int
	// AllStatus 为 true 时包含草稿（后台列表用），仍排除 archived。
	AllStatus bool
}

// Store 数据存储。
type Store struct {
	db       *sql.DB
	d        dialect
	fulltext bool // 是否可用 ngram 全文索引

	// 进程内缓存（internal/memo）：内容型站点数据极少变化，
	// 采用「长 TTL + 写入失效（invalidateMeta）+ SIGUSR1 清空」。
	// 1C1G 远端 MySQL 下 COUNT(*) / GROUP BY / filesort 都是全表级开销，逐个说明：
	countCache *memo.Cache[string, int]          // 检索总数 COUNT(*)
	poetCache  *memo.Cache[string, []model.Poet] // 诗人榜 GROUP BY author
	featCache  *memo.Cache[int, []model.Poem]    // 首页名篇 ORDER BY featured
	metaCache  *memo.Cache[string, []string]     // 朝代/体裁 DISTINCT
	statsCache *memo.Cache[string, Stats]        // 后台统计
	rangeCache *memo.Cache[string, [2]int64]     // 已发布主键区间 MIN/MAX
	titleCache *memo.Cache[string, model.Poem]   // 按诗名取全篇（开放 API）

	// 随机池（前置缓存）：随机结果不能按 URI 缓存，改为「批量回源 + 逐个消费」，
	// 一次回源取一批候选，之后的请求直接从池里取，见 pool 说明。
	randPoemPool *pool[model.Poem]
	randLinePool *pool[RandomLineResult]
}

// 缓存 TTL：按数据变动频率设定；后台写入会立即失效，SIGUSR1 可全量清空。
const (
	metaTTL     = 24 * time.Hour   // 朝代/体裁列表
	statsTTL    = 6 * time.Hour    // 收录统计
	countTTL    = 30 * time.Minute // 检索结果总数（只影响分页展示）
	poetTTL     = 12 * time.Hour   // 诗人榜（GROUP BY author 全表聚合）
	featuredTTL = 6 * time.Hour    // 首页名篇（无索引时是全表 filesort）
	rangeTTL    = 6 * time.Hour    // 已发布主键区间（随机/每日取词）
	titleTTL    = 30 * time.Minute // 按诗名取全篇（开放 API，命中即零回源）
)

// 随机池参数：池越大回源越少，但占用内存与填充耗时越多。
const randomPoolSize = 64 // 一次回源（采样 64 个随机起点）可支撑的请求数

// ── 连接 ───────────────────────────────────────────────

// NormalizeDSN 把 mysql:// 形式的连接串转成 go-sql-driver 的 DSN，
// 也接受原生 DSN；未指定库名时补 defaultDB。
func NormalizeDSN(raw, defaultDB string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("MySQL 连接串为空")
	}
	if strings.HasPrefix(raw, "mysql://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("解析 MySQL 连接串失败: %w", err)
		}
		cfg := mysql.NewConfig()
		cfg.Net = "tcp"
		cfg.Addr = u.Host
		if u.User != nil {
			cfg.User = u.User.Username()
			if p, ok := u.User.Password(); ok {
				cfg.Passwd = p
			}
		}
		cfg.DBName = strings.TrimPrefix(u.Path, "/")
		cfg.ParseTime = true
		cfg.Loc = time.Local
		cfg.Params = map[string]string{"charset": "utf8mb4"}
		for k, vs := range u.Query() {
			switch strings.ToLower(k) {
			case "parsetime", "loc":
				continue
			case "charset":
				cfg.Params["charset"] = vs[0]
			default:
				cfg.Params[k] = vs[0]
			}
		}
		if cfg.DBName == "" {
			cfg.DBName = defaultDB
		}
		return cfg.FormatDSN(), nil
	}
	// 原生 DSN
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		return "", fmt.Errorf("解析 MySQL DSN 失败: %w", err)
	}
	if cfg.DBName == "" {
		cfg.DBName = defaultDB
	}
	cfg.ParseTime = true
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	if cfg.Params["charset"] == "" {
		cfg.Params["charset"] = "utf8mb4"
	}
	return cfg.FormatDSN(), nil
}

// Open 打开（或创建）SQLite 数据库；库为空时灌入种子数据。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// 启用外键与忙等待（modernc/sqlite 连接级 PRAGMA）
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)", path)
	return open(dialectSQLite, dsn)
}

// OpenMySQL 打开 MySQL 数据库（utf8mb4）。支持 mysql://user:pass@host:port/db?… 形式。
func OpenMySQL(dsn string) (*Store, error) {
	normalized, err := NormalizeDSN(dsn, DefaultMySQLDatabase)
	if err != nil {
		return nil, err
	}
	s, err := open(dialectMySQL, normalized)
	if err == nil {
		return s, nil
	}
	// 库不存在（MySQL 错误码 1049）→ 自动建库后重试
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == 1049 {
		if createErr := createMySQLDatabase(normalized); createErr == nil {
			return open(dialectMySQL, normalized)
		}
	}
	return nil, err
}

// createMySQLDatabase 从 DSN 解析库名，连到服务端（不带库）创建 utf8mb4 数据库。
func createMySQLDatabase(dsn string) error {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return err
	}
	name := cfg.DBName
	if name == "" {
		return errors.New("DSN 中未指定数据库名")
	}
	cfg.DBName = ""
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", name))
	return err
}

func open(d dialect, dsn string) (*Store, error) {
	db, err := sql.Open(d.driver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(d.maxOpen)
	db.SetMaxIdleConns(d.maxOpen)
	if d.maxOpen > 1 {
		db.SetConnMaxLifetime(time.Hour)       // 连接池防陈旧连接（MySQL）
		db.SetConnMaxIdleTime(5 * time.Minute) // 空闲连接及时归还，1C1G 小库不常驻占用
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	// 旧版本自有 schema（poems 无 author 列）处理；返回表是否已存在且结构匹配
	exists, err := ensureCompatible(db, d)
	if err != nil {
		db.Close()
		return nil, err
	}
	if !exists {
		// 表不存在才建表：只读账号（无 CREATE 权限）连接已有库时不触发 DDL。
		// MySQL 驱动默认不支持单次 Exec 多语句，按语句逐条执行（双方言通用）
		for _, stmt := range strings.Split(d.ddl, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				db.Close()
				return nil, fmt.Errorf("建表失败: %w", err)
			}
		}
	}
	s := &Store{db: db, d: d}
	s.countCache = memo.New[string, int]("count", countTTL, 256, 0, nil)
	s.poetCache = memo.New[string, []model.Poet]("poets", poetTTL, 32, 0, nil)
	s.featCache = memo.New[int, []model.Poem]("featured", featuredTTL, 16, 0, nil)
	s.metaCache = memo.New[string, []string]("meta", metaTTL, 8, 0, nil)
	s.statsCache = memo.New[string, Stats]("stats", statsTTL, 1, 0, nil)
	s.rangeCache = memo.New[string, [2]int64]("idrange", rangeTTL, 1, 0, nil)
	s.titleCache = memo.New[string, model.Poem]("title", titleTTL, 512, 0, nil)
	s.randPoemPool = newPool[model.Poem]("rand-poem", func() []model.Poem {
		return s.samplePoems(randomPoolSize)
	})
	s.randLinePool = newPool[RandomLineResult]("rand-line", func() []RandomLineResult {
		return s.sampleLines(randomPoolSize)
	})
	s.fulltext = s.detectFulltext()
	if !s.fulltext && d.fulltextIndex != "" {
		// 自建库补齐 ngram 全文索引；不支持时静默降级为 LIKE
		if _, err := db.Exec(`ALTER TABLE poems ADD FULLTEXT KEY ` + d.fulltextIndex +
			` (` + fulltextCols + `) WITH PARSER ngram`); err == nil {
			s.fulltext = s.detectFulltext()
		}
	}
	if d.driver == "mysql" {
		// 历史库缺失的复合索引按需补建（只读账号/无 ALTER 权限时静默跳过）
		ensureIndexes(db)
	}
	if err := s.seedIfEmpty(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ensureIndexes 为已存在的 poems 表补建列表页/诗人榜所需的复合索引。
// 逐条检查 information_schema.statistics，已存在即跳过；失败只记日志不阻断启动
// （只读账号没有 ALTER 权限时站点仍可运行，只是分页会走全表排序）。
func ensureIndexes(db *sql.DB) {
	for _, idx := range mysqlIndexes {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.statistics
			WHERE table_schema = DATABASE() AND table_name = 'poems' AND index_name = ?`,
			idx.name).Scan(&n)
		if err != nil || n > 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE poems ADD INDEX ` + idx.name + ` ` + idx.spec); err != nil {
			log.Printf("[db] 补建索引 %s 失败（账号可能无 ALTER 权限）: %v", idx.name, err)
			continue
		}
		log.Printf("[db] 已补建索引 %s %s", idx.name, idx.spec)
	}
}

// detectFulltext 探测 ngram 全文索引是否可用（索引存在且列集合匹配）。
func (s *Store) detectFulltext() bool {
	if s.d.fulltextIndex == "" {
		return false
	}
	rows, err := s.db.Query(`SELECT id FROM poems WHERE MATCH(` + fulltextCols +
		`) AGAINST('诗词' IN BOOLEAN MODE) LIMIT 1`)
	if err != nil {
		return false
	}
	rows.Close()
	return true
}

// ensureCompatible 确认 poems 表是目标结构，返回表是否已存在且可用。
// 表已存在时无需再执行建表 DDL（只读账号没有 CREATE 权限）。
func ensureCompatible(db *sql.DB, d dialect) (bool, error) {
	var one int
	err := db.QueryRow(d.tableExists).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // 表还不存在，需要建表
	}
	if err != nil {
		return false, fmt.Errorf("检查 poems 表失败: %w", err)
	}
	var author string
	if err := db.QueryRow(`SELECT author FROM poems LIMIT 1`).Scan(&author); err == nil ||
		errors.Is(err, sql.ErrNoRows) {
		return true, nil // 结构匹配（空表也算）
	}
	if !d.dropLegacyDDL {
		return false, errors.New("poems 表结构不兼容：缺少 author 列，请确认连接的是诗词库")
	}
	if _, err := db.Exec(`DROP TABLE poems`); err != nil {
		return false, fmt.Errorf("重建旧版 poems 表失败: %w", err)
	}
	return false, nil // 已丢弃旧表，需重新建表
}

// ReadOnly 判断当前数据库账号是否缺少写入权限（MySQL 只读账号）。
// 用于启动提示：只读账号下后台的新增/修改/删除会被数据库拒绝。
func (s *Store) ReadOnly() bool {
	if s.d.driver != "mysql" {
		return false
	}
	// GRANTEE 形如 'poetry_user'@'%'
	const grantee = `CONCAT("'", SUBSTRING_INDEX(CURRENT_USER(),'@',1), "'@'", SUBSTRING_INDEX(CURRENT_USER(),'@',-1), "'")`
	q := `SELECT COUNT(*) FROM (
	        SELECT PRIVILEGE_TYPE FROM information_schema.user_privileges WHERE GRANTEE=` + grantee + `
	        UNION ALL
	        SELECT PRIVILEGE_TYPE FROM information_schema.schema_privileges
	          WHERE TABLE_SCHEMA=DATABASE() AND GRANTEE=` + grantee + `
	        UNION ALL
	        SELECT PRIVILEGE_TYPE FROM information_schema.table_privileges
	          WHERE TABLE_SCHEMA=DATABASE() AND GRANTEE=` + grantee + `
	      ) t
	      WHERE PRIVILEGE_TYPE IN ('INSERT','UPDATE','DELETE','ALL PRIVILEGES')`
	var n int
	if err := s.db.QueryRow(q).Scan(&n); err != nil {
		return false // 查询失败时不阻断启动
	}
	return n == 0
}

func (s *Store) Close() error { return s.db.Close() }

// ── 种子数据（仅空库时写入） ────────────────────────────

var poetIntros = func() map[string]string {
	m := make(map[string]string, len(data.Poets))
	for _, p := range data.Poets {
		m[p.Name] = p.Intro
	}
	return m
}()

var poetDynasty = func() map[string]string {
	m := make(map[string]string, len(data.Poets))
	for _, p := range data.Poets {
		m[p.Name] = p.Dynasty
	}
	return m
}()

func (s *Store) seedIfEmpty() error {
	if s.ReadOnly() {
		return nil // 只读账号无法写入种子数据
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM poems`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, m := range data.Poems {
		params := poemParams(model.Poem{
			Title:        m.Title,
			Author:       m.Poet,
			Dynasty:      poetDynasty[m.Poet],
			Category:     m.Genre,
			Content:      m.Content,
			Appreciation: m.Note,
			Tags:         m.Tags,
			Status:       "published",
		})
		if _, err := tx.Exec(insertPoemSQL, params...); err != nil {
			return fmt.Errorf("写入诗词 %q: %w", m.Title, err)
		}
	}
	return tx.Commit()
}

// ── 诗词查询 ───────────────────────────────────────────

const poemCols = `p.id, p.title, p.author, p.dynasty, p.category, p.content,
	p.translation, p.annotation, p.appreciation, p.tags, p.featured,
	p.view_count, p.favorite_count, p.comment_count, p.status`

func scanPoem(row interface{ Scan(...any) error }) (model.Poem, error) {
	var m model.Poem
	var tags sql.NullString
	var featured int
	err := row.Scan(&m.ID, &m.Title, &m.Author, &m.Dynasty, &m.Category, &m.Content,
		&m.Translation, &m.Annotation, &m.Appreciation, &tags, &featured,
		&m.ViewCount, &m.FavoriteCount, &m.CommentCount, &m.Status)
	if err != nil {
		return m, err
	}
	m.Tags = tagsToText(tags)
	m.Featured = featured != 0
	return m, nil
}

// tagsToText 把库中的 JSON 数组转成逗号分隔标签（兼容纯文本标签）。
func tagsToText(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	s := strings.TrimSpace(v.String)
	if !strings.HasPrefix(s, "[") {
		return s
	}
	var list []string
	if err := json.Unmarshal([]byte(s), &list); err != nil {
		return strings.Trim(s, `[]"`)
	}
	return strings.Join(list, ",")
}

// tagsToJSON 把逗号分隔标签转成 JSON 数组（空则 NULL）。
func tagsToJSON(tags string) any {
	var list []string
	for _, t := range strings.Split(tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			list = append(list, t)
		}
	}
	if len(list) == 0 {
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return string(b)
}

func like(v string) string { return "%" + v + "%" }

// booleanQuery 清理 BOOLEAN MODE 保留字符（+ - > < ( ) ~ * " @），避免语法错误。
func booleanQuery(q string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '+', '-', '>', '<', '(', ')', '~', '*', '"', '@', ':':
			return ' '
		}
		return r
	}, q)
	return strings.Join(strings.Fields(cleaned), " ")
}

// searchable ngram 最小分词长度为 2，全部词都短于 2 字时无法命中索引。
func searchable(q string) bool {
	for _, tk := range strings.Fields(q) {
		if len([]rune(tk)) >= 2 {
			return true
		}
	}
	return false
}

// clauses 构造过滤条件。全文检索时返回相关度排序表达式（args 对应 where，
// orderArgs 对应 ORDER BY 的占位符）。
func (s *Store) clauses(f *Filter) (where string, args []any, order string, orderArgs []any) {
	var conds []string
	var rankArgs []any

	if q := strings.TrimSpace(f.Keyword); q != "" {
		bq := booleanQuery(q)
		if s.fulltext && bq != "" && searchable(bq) {
			// ngram 全文索引：BOOLEAN MODE 精确/分词匹配 + 自然语言相关度排序
			conds = append(conds, `MATCH(`+fulltextCols+`) AGAINST(? IN BOOLEAN MODE)`)
			args = append(args, bq)
			order = `MATCH(` + fulltextCols + `) AGAINST(? IN NATURAL LANGUAGE MODE) DESC, p.id`
			rankArgs = append(rankArgs, bq)
		} else {
			// 单字关键词 / 无全文索引（SQLite、未建索引的库）：回退 LIKE
			conds = append(conds, `(p.title LIKE ? OR p.author LIKE ? OR p.content LIKE ? OR CAST(p.tags AS CHAR) LIKE ?)`)
			args = append(args, like(q), like(q), like(q), like(q))
		}
	}
	if v := strings.TrimSpace(f.Poet); v != "" {
		conds = append(conds, `p.author LIKE ?`)
		args = append(args, like(v))
	}
	if v := strings.TrimSpace(f.Dynasty); v != "" {
		conds = append(conds, `p.dynasty = ?`)
		args = append(args, v)
	}
	if v := strings.TrimSpace(f.Genre); v != "" {
		conds = append(conds, `p.category = ?`)
		args = append(args, v)
	}
	if v := strings.TrimSpace(f.Tag); v != "" {
		conds = append(conds, `CAST(p.tags AS CHAR) LIKE ?`)
		args = append(args, like(v))
	}
	statusCond := publishedCond
	if f.AllStatus {
		statusCond = `p.status <> 'archived'`
	}
	conds = append(conds, statusCond)
	if order == "" {
		order = "p.id"
	}
	return " WHERE " + strings.Join(conds, " AND "), args, order, rankArgs
}

// ListPoems 按条件检索（含总数）。分页参数被钳制在安全范围：
// 页码超过总页数时回落到最后一页（f.Page 会被就地修正，便于调用方展示）。
func (s *Store) ListPoems(f *Filter) ([]model.Poem, int, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
	where, args, order, orderArgs := s.clauses(f)

	total, err := s.countCache.Get(countKey(where, args), func() (int, error) {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM poems p`+where, args...).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	})
	if err != nil {
		return nil, 0, err
	}

	if pages := PageCount(total, f.PageSize); f.Page > pages {
		f.Page = pages // 深分页/越界页回落到最后一页，避免无意义的超大 OFFSET
	}

	q := `SELECT ` + poemCols + ` FROM poems p` + where + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), orderArgs...)
	queryArgs = append(queryArgs, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := s.db.Query(q, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	list, err := collectPoems(rows)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func collectPoems(rows *sql.Rows) ([]model.Poem, error) {
	var list []model.Poem
	for rows.Next() {
		m, err := scanPoem(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

// FeaturedPoems 首页「名篇撷英」：精选优先，不足按主键顺序补足。
// 复合索引未建好时该查询是全表 filesort，首页每次访问都会命中，故长 TTL 缓存。
func (s *Store) FeaturedPoems(limit int) []model.Poem {
	if limit < 1 || limit > 50 {
		limit = 8
	}
	list, err := s.featCache.Get(limit, func() ([]model.Poem, error) {
		rows, err := s.db.Query(`SELECT `+poemCols+` FROM poems p WHERE `+publishedCond+
			` ORDER BY p.featured DESC, p.id LIMIT ?`, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return collectPoems(rows)
	})
	if err != nil {
		return nil
	}
	return list
}

// recentPoems 最近收录（按主键倒序）。
func (s *Store) recentPoems(limit int) []model.Poem {
	rows, err := s.db.Query(`SELECT `+poemCols+` FROM poems p WHERE `+publishedCond+
		` ORDER BY p.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	list, _ := collectPoems(rows)
	return list
}

// GetPoem 按 ID 取一首「已发布」作品（前台唯一入口，草稿与归档均不可见）。
func (s *Store) GetPoem(id int64) (model.Poem, error) {
	return s.getPoem(id, publishedCond)
}

// GetPoemForAdmin 后台编辑：草稿与已发布可读，软删除（archived）不可读。
func (s *Store) GetPoemForAdmin(id int64) (model.Poem, error) {
	return s.getPoem(id, `p.status <> 'archived'`)
}

func (s *Store) getPoem(id int64, cond string) (model.Poem, error) {
	q := `SELECT ` + poemCols + ` FROM poems p WHERE p.id = ? AND ` + cond
	row := s.db.QueryRow(q, id)
	m, err := scanPoem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Poem{}, ErrNotFound
	}
	return m, err
}

// PageCount 由总数与每页条数计算页数（至少 1 页，避免「第 1 / 0 页」）。
func PageCount(total, pageSize int) int {
	if pageSize < 1 || total <= 0 {
		return 1
	}
	return (total + pageSize - 1) / pageSize
}

// PrevNext 相邻两首（按 id 顺序）。
func (s *Store) PrevNext(id int64) (prev, next *model.Poem) {
	row := s.db.QueryRow(`SELECT `+poemCols+` FROM poems p WHERE p.id < ? AND `+publishedCond+
		` ORDER BY p.id DESC LIMIT 1`, id)
	if m, err := scanPoem(row); err == nil {
		prev = &m
	}
	row = s.db.QueryRow(`SELECT `+poemCols+` FROM poems p WHERE p.id > ? AND `+publishedCond+
		` ORDER BY p.id LIMIT 1`, id)
	if m, err := scanPoem(row); err == nil {
		next = &m
	}
	return
}

// idRange 已发布作品的主键区间，用于随机/每日取词（避免 ORDER BY RAND() 全表排序）。
// 区间在数据量不变时是稳定的，缓存 6 小时；后台写入会立即失效。
func (s *Store) idRange() (int64, int64, error) {
	r, err := s.rangeCache.Get("range", func() ([2]int64, error) {
		var lo, hi sql.NullInt64
		if err := s.db.QueryRow(`SELECT MIN(id), MAX(id) FROM poems p WHERE `+publishedCond).
			Scan(&lo, &hi); err != nil {
			return [2]int64{}, err
		}
		if !lo.Valid || !hi.Valid {
			return [2]int64{}, ErrNotFound
		}
		return [2]int64{lo.Int64, hi.Int64}, nil
	})
	return r[0], r[1], err
}

// poemAtOrAfter 取 id >= target 的第一首（越过末尾则回退到最早一首）。
func (s *Store) poemAtOrAfter(target int64) (model.Poem, error) {
	row := s.db.QueryRow(`SELECT `+poemCols+` FROM poems p WHERE p.id >= ? AND `+publishedCond+
		` ORDER BY p.id LIMIT 1`, target)
	m, err := scanPoem(row)
	if errors.Is(err, sql.ErrNoRows) {
		row = s.db.QueryRow(`SELECT ` + poemCols + ` FROM poems p WHERE ` + publishedCond +
			` ORDER BY p.id LIMIT 1`)
		m, err = scanPoem(row)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return model.Poem{}, ErrNotFound
	}
	return m, err
}

// RandomPoem 随机一首：从随机池取（池见下），命中即零数据库往返。
func (s *Store) RandomPoem() (model.Poem, error) { return s.randPoemPool.take() }

// GetPoemByTitle 按诗名取一首：先精确匹配 title（同名时取 id 最小的一首），
// 未命中再退化为包含匹配，便于「静夜思」这类带前后缀的写法也能查到。
// author 非空时用于消歧（同一诗题可能有多位作者的作品）。
// 结果按「诗名+作者」缓存（含「查无此诗」的负结果，避免重复回源）。
func (s *Store) GetPoemByTitle(title, author string) (model.Poem, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return model.Poem{}, ErrNotFound
	}
	author = strings.TrimSpace(author)
	m, err := s.titleCache.Get(title+"\x00"+author, func() (model.Poem, error) {
		m, err := s.queryPoemByTitle(title, author)
		if errors.Is(err, ErrNotFound) {
			return model.Poem{}, nil // 零值即「查无此诗」，同样入缓存
		}
		return m, err
	})
	if err != nil {
		return model.Poem{}, err
	}
	if m.ID == 0 {
		return model.Poem{}, ErrNotFound
	}
	return m, nil
}

// queryPoemByTitle 实际查询（缓存未命中时调用）。
func (s *Store) queryPoemByTitle(title, author string) (model.Poem, error) {
	cond := []string{publishedCond}
	base := []any{}
	if author != "" {
		cond = append(cond, `p.author = ?`)
		base = append(base, author)
	}
	query := func(match string, val any) (model.Poem, error) {
		c := append(append([]string{}, cond...), match)
		a := append(append([]any{}, base...), val)
		row := s.db.QueryRow(`SELECT `+poemCols+` FROM poems p WHERE `+
			strings.Join(c, " AND ")+` ORDER BY p.id LIMIT 1`, a...)
		m, err := scanPoem(row)
		if errors.Is(err, sql.ErrNoRows) {
			return model.Poem{}, ErrNotFound
		}
		return m, err
	}
	m, err := query(`p.title = ?`, title)
	if errors.Is(err, ErrNotFound) {
		return query(`p.title LIKE ?`, like(title)) // 包含匹配兜底
	}
	return m, err
}

// RandomLineResult 随机一句的结果：句子 + 所属作品。
type RandomLineResult struct {
	Poem model.Poem
	Line string
}

// RandomLine 随机一句：从随机句池取（池见下），命中即零数据库往返。
func (s *Store) RandomLine() (RandomLineResult, error) { return s.randLinePool.take() }

// ── 随机池（前置缓存） ────────────────────────────────
//
// 随机结果不能按 URI 缓存（否则每次都返回同一首），这里用「批量回源 + 逐个消费」：
// 一次回源在主键区间上取多个随机起点、各取一小段，打散后作为候选池；
// 之后每个请求只从池里取一个，余量不足时后台异步补一批。
// 效果：连续请求不重复、分布仍覆盖全库，而数据库往返降为每池一次（约 1/64）。
// 池已登记到 memo，后台写入 / SIGUSR1 清空缓存时一并失效。
type pool[T any] struct {
	name    string
	fill    func() []T
	mu      sync.Mutex
	items   []T
	next    int
	filling bool // 是否已有 goroutine 在补池
}

func newPool[T any](name string, fill func() []T) *pool[T] {
	p := &pool[T]{name: name, fill: fill}
	memo.Register(p)
	return p
}

func (p *pool[T]) Name() string { return p.name }

// Flush 清空池（下次请求会同步回源重新填充）。
func (p *pool[T]) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items, p.next = nil, 0
}

// take 取一个候选：池空时同步填充；余量不足一半时后台异步补一批。
func (p *pool[T]) take() (T, error) {
	var zero T

	p.mu.Lock()
	if p.next < len(p.items) {
		v := p.items[p.next]
		p.next++
		async := !p.filling && len(p.items)-p.next < len(p.items)/2
		if async {
			p.filling = true
		}
		p.mu.Unlock()
		if async {
			go p.refill()
		}
		return v, nil
	}
	p.mu.Unlock()

	items := p.fill()
	if len(items) == 0 {
		return zero, ErrNotFound
	}
	p.mu.Lock()
	p.items, p.next = items, 1
	p.mu.Unlock()
	return items[0], nil
}

// refill 后台补一批：成功后整池替换（丢弃未消费的余量，保证新鲜度）。
func (p *pool[T]) refill() {
	items := p.fill()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filling = false
	if len(items) > 0 {
		p.items, p.next = items, 0
	}
}

// samplePoems 批量采样：每个随机起点只取 1 首——若一次取连续多首，
// 池内 id 会相邻（常是同一作品的连章），随机性明显变差。
// 4 路并发只为缩短填充耗时（填充走后台，不阻塞请求）。
func (s *Store) samplePoems(limit int) []model.Poem {
	lo, hi, err := s.idRange()
	if err != nil {
		return nil
	}
	const workers = 4
	perWorker := (limit + workers - 1) / workers

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		out = make([]model.Poem, 0, limit)
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				m, err := s.poemAtOrAfter(lo + rand.Int63n(hi-lo+1))
				if err != nil {
					return
				}
				mu.Lock()
				out = append(out, m)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// sampleLines 批量采样句子：每首诗随机挑一句（同一首不在池里重复出现），再打散。
func (s *Store) sampleLines(limit int) []RandomLineResult {
	poems := s.samplePoems(limit)
	out := make([]RandomLineResult, 0, limit)
	for _, m := range poems {
		lines := m.Lines()
		if len(lines) == 0 {
			continue
		}
		out = append(out, RandomLineResult{Poem: m, Line: lines[rand.Intn(len(lines))]})
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// DailyPoem 今日诗词：按日期确定性挑选（一天内稳定，跨天轮换）。
func (s *Store) DailyPoem() (model.Poem, error) {
	lo, hi, err := s.idRange()
	if err != nil {
		return model.Poem{}, err
	}
	span := hi - lo + 1
	day := time.Now().Unix() / 86400
	idx := (day*2654435761 + 12345) % span
	if idx < 0 {
		idx += span
	}
	return s.poemAtOrAfter(lo + idx)
}

// ErrNotFound 统一的「不存在」错误。
var ErrNotFound = errors.New("记录不存在")

// ── 诗人（由 poems 聚合派生） ───────────────────────────

// poetLimit 诗人列表默认返回条数（全库逾万位作者，按作品数取前若干）。
const poetLimit = 300

// ListPoets 诗人列表（含作品数），支持姓名关键词与朝代过滤。
func (s *Store) ListPoets(keyword, dynasty string) ([]model.Poet, error) {
	// 全库数十万行做 GROUP BY 聚合，结果按关键词/朝代缓存，避免每次访问都重扫
	key := strings.TrimSpace(keyword) + "\x00" + strings.TrimSpace(dynasty)
	return s.poetCache.Get(key, func() ([]model.Poet, error) {
		var conds []string
		var args []any
		if q := strings.TrimSpace(keyword); q != "" {
			conds = append(conds, `p.author LIKE ?`)
			args = append(args, like(q))
		}
		if v := strings.TrimSpace(dynasty); v != "" {
			conds = append(conds, `p.dynasty = ?`)
			args = append(args, v)
		}
		conds = append(conds, publishedCond)

		q := `SELECT p.author, p.dynasty, COUNT(*) AS cnt
		      FROM poems p WHERE ` + strings.Join(conds, " AND ") + `
		      GROUP BY p.author, p.dynasty
		      ORDER BY cnt DESC, p.author
		      LIMIT ?`
		args = append(args, poetLimit)

		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var list []model.Poet
		for rows.Next() {
			var p model.Poet
			var cnt int
			if err := rows.Scan(&p.Name, &p.Dynasty, &cnt); err != nil {
				return nil, err
			}
			p.PoemCount = cnt
			p.Intro = poetIntros[p.Name]
			list = append(list, p)
		}
		return list, rows.Err()
	})
}

// GetPoet 按姓名取诗人（同名跨朝代时取作品最多者）。
func (s *Store) GetPoet(name string) (model.Poet, error) {
	var p model.Poet
	var cnt int
	err := s.db.QueryRow(`SELECT p.author, p.dynasty, COUNT(*) AS cnt
		FROM poems p WHERE p.author = ? AND `+publishedCond+`
		GROUP BY p.author, p.dynasty ORDER BY cnt DESC LIMIT 1`, name).
		Scan(&p.Name, &p.Dynasty, &cnt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.PoemCount = cnt
	p.Intro = poetIntros[p.Name]
	return p, nil
}

// PoetPoemLimit 诗人详情页最多展示的作品数。
const PoetPoemLimit = 200

// PoemsByPoet 某诗人的作品（按主键顺序，最多 PoetPoemLimit 首）。
func (s *Store) PoemsByPoet(name string) ([]model.Poem, error) {
	rows, err := s.db.Query(`SELECT `+poemCols+` FROM poems p WHERE p.author = ? AND `+
		publishedCond+` ORDER BY p.id LIMIT ?`, name, PoetPoemLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPoems(rows)
}

// PoetOptions 下拉选项：作品最多的诗人。
func (s *Store) PoetOptions() []model.Poet { poets, _ := s.ListPoets("", ""); return poets }

// RenamePoet 重命名诗人（批量更新其作品上的 author/dynasty）。
func (s *Store) RenamePoet(oldName, newName, newDynasty string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return errors.New("诗人姓名不能为空")
	}
	res, err := s.db.Exec(`UPDATE poems SET author=?, dynasty=? WHERE author=?`,
		newName, strings.TrimSpace(newDynasty), oldName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.invalidateMeta()
	return nil
}

// DeletePoet 删除诗人：把其全部作品置为 archived（状态位软删除，不做物理 DELETE）。
func (s *Store) DeletePoet(name string) error {
	res, err := s.db.Exec(`UPDATE poems SET status='archived' WHERE author=? AND status<>'archived'`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.invalidateMeta()
	return nil
}

// ── 后台 CRUD ─────────────────────────────────────────

const insertPoemSQL = `INSERT INTO poems
	(title, author, dynasty, category, content, translation, annotation, appreciation, tags, featured, status)
	VALUES(?,?,?,?,?,?,?,?,?,?,?)`

func poemParams(m model.Poem) []any {
	status := strings.TrimSpace(m.Status)
	if status == "" {
		status = "published"
	}
	featured := 0
	if m.Featured {
		featured = 1
	}
	return []any{
		strings.TrimSpace(m.Title), strings.TrimSpace(m.Author), strings.TrimSpace(m.Dynasty),
		defaultIfEmpty(strings.TrimSpace(m.Category), "古诗"), m.Content,
		m.Translation, m.Annotation, m.Appreciation, tagsToJSON(m.Tags), featured, status,
	}
}

func defaultIfEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// CreatePoem 新增作品。
func (s *Store) CreatePoem(m model.Poem) (int64, error) {
	res, err := s.db.Exec(insertPoemSQL, poemParams(m)...)
	if err != nil {
		return 0, err
	}
	s.invalidateMeta()
	return res.LastInsertId()
}

// UpdatePoem 修改作品。
func (s *Store) UpdatePoem(m model.Poem) error {
	status := strings.TrimSpace(m.Status)
	if status == "" {
		status = "published"
	}
	featured := 0
	if m.Featured {
		featured = 1
	}
	res, err := s.db.Exec(`UPDATE poems SET
		title=?, author=?, dynasty=?, category=?, content=?,
		translation=?, annotation=?, appreciation=?, tags=?, featured=?, status=?
		WHERE id=?`,
		strings.TrimSpace(m.Title), strings.TrimSpace(m.Author), strings.TrimSpace(m.Dynasty),
		defaultIfEmpty(strings.TrimSpace(m.Category), "古诗"), m.Content,
		m.Translation, m.Annotation, m.Appreciation, tagsToJSON(m.Tags), featured, status, m.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.invalidateMeta()
	return nil
}

// DeletePoem 删除作品：置 status='archived'（状态位软删除，数据保留，不做物理 DELETE）。
func (s *Store) DeletePoem(id int64) error {
	res, err := s.db.Exec(`UPDATE poems SET status='archived' WHERE id=? AND status<>'archived'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.invalidateMeta()
	return nil
}

// ── 元数据 / 统计 ──────────────────────────────────────

func (s *Store) distinct(col string) []string {
	v, err := s.metaCache.Get(col, func() ([]string, error) {
		rows, err := s.db.Query(`SELECT DISTINCT ` + col + ` FROM poems p WHERE ` + publishedCond +
			` AND ` + col + ` <> '' ORDER BY ` + col)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if rows.Scan(&v) == nil {
				out = append(out, v)
			}
		}
		return out, rows.Err()
	})
	if err != nil {
		return nil // 与旧行为一致：查询失败不阻断页面
	}
	return v
}

// countKey 检索条件 → 缓存键（args 均为字符串，直接拼接即可）。
func countKey(where string, args []any) string {
	var b strings.Builder
	b.WriteString(where)
	for _, a := range args {
		b.WriteByte(0)
		fmt.Fprint(&b, a)
	}
	return b.String()
}

// InvalidateCache 清空全部进程内缓存（含页面缓存）。
// 触发方式：后台写入（自动）、批量导入后 `systemctl kill -s SIGUSR1 shici`（手动）。
func (s *Store) InvalidateCache() { memo.FlushAll() }

// Warmup 启动预热：把首页/列表/诗人榜等高频数据提前填入缓存，
// 避免冷启动的第一个访问者承担全部回源耗时。
func (s *Store) Warmup() {
	s.FeaturedPoems(8)
	_, _ = s.Stats()
	s.Dynasties()
	s.Genres()
	_, _ = s.ListPoets("", "")
	f := Filter{Page: 1, PageSize: 12}
	_, _, _ = s.ListPoems(&f)
	_, _ = s.RandomPoem() // 预热随机池：填充后前 64 次随机请求零回源
	_, _ = s.RandomLine()
	log.Printf("[cache] 启动预热完成")
}

// invalidateMeta 写入数据后清空全部缓存（数据 + 页面）。
func (s *Store) invalidateMeta() { memo.FlushAll() }

// Dynasties 诗词涉及的朝代列表。
func (s *Store) Dynasties() []string { return s.distinct("p.dynasty") }

// Genres 诗词涉及的体裁列表（category 列）。
func (s *Store) Genres() []string { return s.distinct("p.category") }

// Stats 后台统计。
type Stats struct {
	Poems, Poets int
	ByDynasty    []Count
	Recent       []model.Poem
}

// Count 计数条目。
type Count struct {
	Label string
	N     int
}

// Stats 返回统计信息（statsTTL 内复用，避免每次全表 COUNT）。
func (s *Store) Stats() (Stats, error) {
	return s.statsCache.Get("stats", func() (Stats, error) {
		var st Stats
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM poems p WHERE ` + publishedCond).
			Scan(&st.Poems); err != nil {
			return st, err
		}
		if err := s.db.QueryRow(`SELECT COUNT(DISTINCT author) FROM poems p WHERE ` + publishedCond).
			Scan(&st.Poets); err != nil {
			return st, err
		}
		rows, err := s.db.Query(`SELECT p.dynasty, COUNT(*) FROM poems p WHERE ` + publishedCond +
			` AND p.dynasty <> '' GROUP BY p.dynasty ORDER BY COUNT(*) DESC`)
		if err == nil {
			for rows.Next() {
				var c Count
				if rows.Scan(&c.Label, &c.N) == nil {
					st.ByDynasty = append(st.ByDynasty, c)
				}
			}
			rows.Close()
		}
		st.Recent = s.recentPoems(6)
		return st, nil
	})
}

// AtoiID 安全解析 ID。
func AtoiID(s string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return id
}
