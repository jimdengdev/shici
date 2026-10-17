// mkdb — 在 MySQL 服务器上创建诗词库（utf8mb4，默认 poetry_charity）。
// 用法（服务端 DSN 不带库名）：
//
//	MYSQL_SERVER_DSN='root:pass@tcp(127.0.0.1:3306)/' MYSQL_DATABASE=poetry_charity go run ./cmd/mkdb
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/go-sql-driver/mysql"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	serverDSN := envOr("MYSQL_SERVER_DSN", "root:pass@tcp(127.0.0.1:3306)/")
	dbName := envOr("MYSQL_DATABASE", "poetry_charity")

	db, err := sql.Open("mysql", serverDSN)
	if err != nil {
		log.Fatalf("连接 MySQL 失败: %v", err)
	}
	defer db.Close()

	var version string
	if err := db.QueryRow("SELECT VERSION()").Scan(&version); err != nil {
		log.Fatalf("连接 MySQL 失败: %v", err)
	}

	if _, err := db.Exec(
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", dbName)); err != nil {
		log.Fatalf("创建数据库 %s 失败: %v", dbName, err)
	}
	log.Printf("MySQL %s：数据库 %s 已就绪（utf8mb4）", version, dbName)
}
