-- 诗词雅集 —— MySQL 索引优化（幂等，可重复执行）
--
-- 目标：让列表分页、诗人榜等高频查询走索引，避免全表 filesort / 临时表。
-- 权限：需要 poems 表的 ALTER 权限（只读账号执行会报 ALTER command denied）。
-- 说明：InnoDB 二级索引会隐式附加主键，idx_poems_status / idx_poems_author
--       已分别等价于 (status, id) / (author, id)，因此不再创建同前缀复合索引。
--       34 万行加索引为 Online DDL，通常几十秒到数分钟，期间可读。

SET @db := DATABASE();

-- idx_poems_status_dynasty_id (status, dynasty, id)
SET @sql := IF(
  (SELECT COUNT(*) FROM information_schema.statistics
     WHERE table_schema = @db AND table_name = 'poems'
       AND index_name = 'idx_poems_status_dynasty_id') = 0,
  'ALTER TABLE poems ADD INDEX idx_poems_status_dynasty_id (status, dynasty, id)',
  'SELECT ''idx_poems_status_dynasty_id exists''');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- idx_poems_status_category_id (status, category, id)
SET @sql := IF(
  (SELECT COUNT(*) FROM information_schema.statistics
     WHERE table_schema = @db AND table_name = 'poems'
       AND index_name = 'idx_poems_status_category_id') = 0,
  'ALTER TABLE poems ADD INDEX idx_poems_status_category_id (status, category, id)',
  'SELECT ''idx_poems_status_category_id exists''');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- idx_poems_status_featured_id (status, featured, id)
SET @sql := IF(
  (SELECT COUNT(*) FROM information_schema.statistics
     WHERE table_schema = @db AND table_name = 'poems'
       AND index_name = 'idx_poems_status_featured_id') = 0,
  'ALTER TABLE poems ADD INDEX idx_poems_status_featured_id (status, featured, id)',
  'SELECT ''idx_poems_status_featured_id exists''');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- idx_poems_status_author_dynasty (status, author, dynasty)
SET @sql := IF(
  (SELECT COUNT(*) FROM information_schema.statistics
     WHERE table_schema = @db AND table_name = 'poems'
       AND index_name = 'idx_poems_status_author_dynasty') = 0,
  'ALTER TABLE poems ADD INDEX idx_poems_status_author_dynasty (status, author, dynasty)',
  'SELECT ''idx_poems_status_author_dynasty exists''');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

ANALYZE TABLE poems;

-- 历史库若曾创建过以下冗余索引，确认无业务依赖后可删除（可选）：
-- ALTER TABLE poems DROP INDEX idx_poems_status_id, DROP INDEX idx_poems_author_id;
