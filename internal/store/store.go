// Package store 基于 SQLite（modernc.org/sqlite，纯 Go 无 CGO）的数据访问层。
// 首次打开自动建表并灌入种子数据。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql" // MySQL 驱动（纯 Go）
	_ "modernc.org/sqlite"           // 纯 Go SQLite 驱动

	"shici/data"
	"shici/internal/model"
)

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS poets (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  name    TEXT NOT NULL UNIQUE,
  dynasty TEXT NOT NULL DEFAULT '',
  intro   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS poems (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  title     TEXT NOT NULL,
  poet_id   INTEGER NOT NULL REFERENCES poets(id) ON DELETE CASCADE,
  poet_name TEXT NOT NULL DEFAULT '',
  dynasty   TEXT NOT NULL DEFAULT '',
  genre     TEXT NOT NULL DEFAULT '',
  content   TEXT NOT NULL,
  tags      TEXT NOT NULL DEFAULT '',
  note      TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_poems_title ON poems(title);
CREATE INDEX IF NOT EXISTS idx_poems_poet_name ON poems(poet_name);
CREATE INDEX IF NOT EXISTS idx_poems_dynasty ON poems(dynasty);
`

const mysqlSchema = `
CREATE TABLE IF NOT EXISTS poets (
  id      INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  name    VARCHAR(64) NOT NULL UNIQUE,
  dynasty VARCHAR(32) NOT NULL DEFAULT '',
  intro   TEXT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE TABLE IF NOT EXISTS poems (
  id        BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  title     VARCHAR(255) NOT NULL,
  poet_id   INT UNSIGNED NOT NULL,
  poet_name VARCHAR(64) NOT NULL DEFAULT '',
  dynasty   VARCHAR(32) NOT NULL DEFAULT '',
  genre     VARCHAR(32) NOT NULL DEFAULT '',
  content   MEDIUMTEXT NOT NULL,
  tags      VARCHAR(255) NOT NULL DEFAULT '',
  note      TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT fk_poems_poet FOREIGN KEY (poet_id) REFERENCES poets(id) ON DELETE CASCADE,
  KEY idx_poems_title (title),
  KEY idx_poems_poet_name (poet_name),
  KEY idx_poems_dynasty (dynasty)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
`

// dialect 方言差异：DDL、随机函数、连接池策略。
type dialect struct {
	driver  string
	ddl     string
	randFn  string // SQLite: RANDOM() / MySQL: RAND()
	maxOpen int    // 连接池上限（SQLite 串行化防锁冲突）
}

var (
	dialectSQLite = dialect{driver: "sqlite", ddl: sqliteSchema, randFn: "RANDOM()", maxOpen: 1}
	dialectMySQL  = dialect{driver: "mysql", ddl: mysqlSchema, randFn: "RAND()", maxOpen: 10}
)

// Filter 诗词检索过滤条件。
type Filter struct {
	Keyword  string // 关键词：匹配标题 / 正文 / 诗人 / 标签
	Poet     string // 诗人（模糊匹配）
	Dynasty  string
	Genre    string
	Tag      string
	Page     int
	PageSize int
}

// Store 数据存储。
type Store struct {
	db *sql.DB
	d  dialect
}

// Open 打开（或创建）SQLite 数据库；库为空时灌入种子数据。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// 启用外键级联删除与忙等待（modernc/sqlite 连接级 PRAGMA）
	dsn := fmt.Sprintf("%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	return open(dialectSQLite, dsn)
}

// OpenMySQL 打开 MySQL 数据库（需 utf8mb4 + parseTime）；库不存在时自动创建（utf8mb4）。
// DSN 示例：user:pass@tcp(127.0.0.1:3306)/shici?charset=utf8mb4&parseTime=true&loc=Local
func OpenMySQL(dsn string) (*Store, error) {
	s, err := open(dialectMySQL, dsn)
	if err == nil {
		return s, nil
	}
	// 库不存在（MySQL 错误码 1049）→ 自动建库后重试
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == 1049 {
		if createErr := createMySQLDatabase(dsn); createErr == nil {
			return open(dialectMySQL, dsn)
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
	if d.maxOpen > 1 {
		db.SetConnMaxLifetime(time.Hour) // 连接池防陈旧连接（MySQL）
	}
	// MySQL 驱动默认不支持单次 Exec 多语句，按语句逐条建表（双方言通用）
	for _, stmt := range strings.Split(d.ddl, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("建表失败: %w", err)
		}
	}
	s := &Store{db: db, d: d}
	if err := s.seedIfEmpty(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) seedIfEmpty() error {
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

	poetID := map[string]int64{}
	dynasty := map[string]string{}
	for _, p := range data.Poets {
		res, err := tx.Exec(
			`INSERT INTO poets(name, dynasty, intro) VALUES(?,?,?)`,
			p.Name, p.Dynasty, p.Intro)
		if err != nil {
			return fmt.Errorf("写入诗人 %s: %w", p.Name, err)
		}
		id, _ := res.LastInsertId()
		poetID[p.Name] = id
		dynasty[p.Name] = p.Dynasty
	}
	for _, m := range data.Poems {
		pid, ok := poetID[m.Poet]
		if !ok {
			return fmt.Errorf("种子诗词 %q 引用了未知诗人 %q", m.Title, m.Poet)
		}
		if _, err := tx.Exec(
			`INSERT INTO poems(title, poet_id, poet_name, dynasty, genre, content, tags, note)
			 VALUES(?,?,?,?,?,?,?,?)`,
			m.Title, pid, m.Poet, dynasty[m.Poet], m.Genre, m.Content, m.Tags, m.Note,
		); err != nil {
			return fmt.Errorf("写入诗词 %q: %w", m.Title, err)
		}
	}
	return tx.Commit()
}

const poemCols = `p.id, p.title, p.poet_id, p.poet_name, p.dynasty, p.genre, p.content, p.tags, p.note`

func scanPoem(row interface{ Scan(...any) error }) (model.Poem, error) {
	var m model.Poem
	err := row.Scan(&m.ID, &m.Title, &m.PoetID, &m.PoetName, &m.Dynasty, &m.Genre, &m.Content, &m.Tags, &m.Note)
	return m, err
}

func like(v string) string { return "%" + v + "%" }

func (f *Filter) clauses() (string, []any) {
	var conds []string
	var args []any
	if q := strings.TrimSpace(f.Keyword); q != "" {
		conds = append(conds, `(p.title LIKE ? OR p.content LIKE ? OR p.poet_name LIKE ? OR p.tags LIKE ?)`)
		args = append(args, like(q), like(q), like(q), like(q))
	}
	if v := strings.TrimSpace(f.Poet); v != "" {
		conds = append(conds, `p.poet_name LIKE ?`)
		args = append(args, like(v))
	}
	if v := strings.TrimSpace(f.Dynasty); v != "" {
		conds = append(conds, `p.dynasty = ?`)
		args = append(args, v)
	}
	if v := strings.TrimSpace(f.Genre); v != "" {
		conds = append(conds, `p.genre = ?`)
		args = append(args, v)
	}
	if v := strings.TrimSpace(f.Tag); v != "" {
		conds = append(conds, `p.tags LIKE ?`)
		args = append(args, like(v))
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListPoems 按条件检索（含总数）。分页参数被钳制在安全范围。
func (s *Store) ListPoems(f Filter) ([]model.Poem, int, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
	where, args := f.clauses()

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM poems p`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT ` + poemCols + ` FROM poems p` + where +
		` ORDER BY p.id LIMIT ? OFFSET ?`
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var list []model.Poem
	for rows.Next() {
		m, err := scanPoem(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, m)
	}
	return list, total, rows.Err()
}

// GetPoem 按 ID 取一首。
func (s *Store) GetPoem(id int64) (model.Poem, error) {
	row := s.db.QueryRow(`SELECT `+poemCols+` FROM poems p WHERE p.id = ?`, id)
	m, err := scanPoem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Poem{}, ErrNotFound
	}
	return m, err
}

// PrevNext 相邻两首（按 id 顺序）。
func (s *Store) PrevNext(id int64) (prev, next *model.Poem) {
	var p model.Poem
	row := s.db.QueryRow(`SELECT `+poemCols+` FROM poems p WHERE p.id = (SELECT MAX(id) FROM poems WHERE id < ?)`, id)
	if m, err := scanPoem(row); err == nil {
		p = m
		prev = &p
	}
	var n model.Poem
	row = s.db.QueryRow(`SELECT `+poemCols+` FROM poems p WHERE p.id = (SELECT MIN(id) FROM poems WHERE id > ?)`, id)
	if m, err := scanPoem(row); err == nil {
		n = m
		next = &n
	}
	return
}

// RandomPoem 随机一首（SQLite: RANDOM() / MySQL: RAND()）。
func (s *Store) RandomPoem() (model.Poem, error) {
	row := s.db.QueryRow(`SELECT ` + poemCols + ` FROM poems p ORDER BY ` + s.d.randFn + ` LIMIT 1`)
	m, err := scanPoem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Poem{}, ErrNotFound
	}
	return m, err
}

// DailyPoem 今日诗词：按日期确定性挑选（一天内稳定，跨天轮换）。
func (s *Store) DailyPoem() (model.Poem, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM poems`).Scan(&total); err != nil || total == 0 {
		return model.Poem{}, ErrNotFound
	}
	day := time.Now().Unix() / 86400
	idx := (day*2654435761 + 12345) % int64(total)
	if idx < 0 {
		idx += int64(total)
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM poems ORDER BY id LIMIT 1 OFFSET ?`, idx).Scan(&id); err != nil {
		return model.Poem{}, ErrNotFound
	}
	return s.GetPoem(id)
}

// ErrNotFound 统一的「不存在」错误。
var ErrNotFound = errors.New("记录不存在")

// ── 诗人 ──────────────────────────────────────────────

// ListPoets 诗人列表（含作品数），支持关键词（姓名/朝代）与朝代过滤。
func (s *Store) ListPoets(keyword, dynasty string) ([]model.Poet, error) {
	var conds []string
	var args []any
	if q := strings.TrimSpace(keyword); q != "" {
		conds = append(conds, `(t.name LIKE ? OR t.intro LIKE ?)`)
		args = append(args, like(q), like(q))
	}
	if v := strings.TrimSpace(dynasty); v != "" {
		conds = append(conds, `t.dynasty = ?`)
		args = append(args, v)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	q := `SELECT t.id, t.name, t.dynasty, t.intro,
	         (SELECT COUNT(*) FROM poems p WHERE p.poet_id = t.id) AS cnt
	      FROM poets t` + where + ` ORDER BY t.id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []model.Poet
	for rows.Next() {
		var p model.Poet
		var cnt int
		if err := rows.Scan(&p.ID, &p.Name, &p.Dynasty, &p.Intro, &cnt); err != nil {
			return nil, err
		}
		p.PoemCount = cnt
		list = append(list, p)
	}
	return list, rows.Err()
}

// GetPoet 按 ID 取诗人。
func (s *Store) GetPoet(id int64) (model.Poet, error) {
	var p model.Poet
	row := s.db.QueryRow(`SELECT id, name, dynasty, intro FROM poets WHERE id = ?`, id)
	if err := row.Scan(&p.ID, &p.Name, &p.Dynasty, &p.Intro); errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	} else if err != nil {
		return p, err
	}
	var cnt int
	s.db.QueryRow(`SELECT COUNT(*) FROM poems WHERE poet_id = ?`, id).Scan(&cnt)
	p.PoemCount = cnt
	return p, nil
}

// PoemsByPoet 某诗人的全部作品。
func (s *Store) PoemsByPoet(poetID int64) ([]model.Poem, error) {
	rows, err := s.db.Query(`SELECT `+poemCols+` FROM poems p WHERE p.poet_id = ? ORDER BY p.id`, poetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// ── 后台 CRUD ─────────────────────────────────────────

func (s *Store) CreatePoet(name, dynasty, intro string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO poets(name, dynasty, intro) VALUES(?,?,?)`,
		strings.TrimSpace(name), strings.TrimSpace(dynasty), strings.TrimSpace(intro))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdatePoet(id int64, name, dynasty, intro string) error {
	if _, err := s.db.Exec(`UPDATE poets SET name=?, dynasty=?, intro=? WHERE id=?`,
		strings.TrimSpace(name), strings.TrimSpace(dynasty), strings.TrimSpace(intro), id); err != nil {
		return err
	}
	// 同步反规范化字段
	_, err := s.db.Exec(`UPDATE poems SET poet_name=?, dynasty=? WHERE poet_id=?`,
		strings.TrimSpace(name), strings.TrimSpace(dynasty), id)
	return err
}

func (s *Store) DeletePoet(id int64) error {
	res, err := s.db.Exec(`DELETE FROM poets WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreatePoem(m model.Poem) (int64, error) {
	// 由诗人 ID 补齐诗人姓名与朝代
	var name, dynasty string
	err := s.db.QueryRow(`SELECT name, dynasty FROM poets WHERE id=?`, m.PoetID).Scan(&name, &dynasty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(
		`INSERT INTO poems(title, poet_id, poet_name, dynasty, genre, content, tags, note)
		 VALUES(?,?,?,?,?,?,?,?)`,
		m.Title, m.PoetID, name, dynasty, m.Genre, m.Content, m.Tags, m.Note)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdatePoem(m model.Poem) error {
	var name, dynasty string
	err := s.db.QueryRow(`SELECT name, dynasty FROM poets WHERE id=?`, m.PoetID).Scan(&name, &dynasty)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE poems SET title=?, poet_id=?, poet_name=?, dynasty=?, genre=?, content=?, tags=?, note=? WHERE id=?`,
		m.Title, m.PoetID, name, dynasty, m.Genre, m.Content, m.Tags, m.Note, m.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeletePoem(id int64) error {
	res, err := s.db.Exec(`DELETE FROM poems WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── 元数据 / 统计 ──────────────────────────────────────

func (s *Store) distinct(col string) []string {
	rows, err := s.db.Query(`SELECT DISTINCT ` + col + ` FROM poems WHERE ` + col + ` <> '' ORDER BY ` + col)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if rows.Scan(&v) == nil {
			out = append(out, v)
		}
	}
	return out
}

// Dynasties 诗词涉及的朝代列表。
func (s *Store) Dynasties() []string { return s.distinct("dynasty") }

// Genres 诗词涉及的体裁列表。
func (s *Store) Genres() []string { return s.distinct("genre") }

// PoetOptions 下拉选项：所有诗人（含作品数）。
func (s *Store) PoetOptions() []model.Poet { poets, _ := s.ListPoets("", ""); return poets }

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

// Stats 返回统计信息。
func (s *Store) Stats() (Stats, error) {
	var st Stats
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM poems`).Scan(&st.Poems); err != nil {
		return st, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM poets`).Scan(&st.Poets); err != nil {
		return st, err
	}
	rows, err := s.db.Query(`SELECT dynasty, COUNT(*) FROM poems WHERE dynasty <> '' GROUP BY dynasty ORDER BY COUNT(*) DESC`)
	if err == nil {
		for rows.Next() {
			var c Count
			if rows.Scan(&c.Label, &c.N) == nil {
				st.ByDynasty = append(st.ByDynasty, c)
			}
		}
		rows.Close()
	}
	st.Recent, _, _ = s.ListPoems(Filter{Page: 1, PageSize: 6})
	return st, nil
}

// AtoiID 安全解析 ID。
func AtoiID(s string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return id
}
