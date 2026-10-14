// shici —— 公益中国诗词网站
//
// 前台：/（今日诗词）、/poems（关键词+诗人检索）、/poets、/poems/{id}、/random、/about
// API ：/api/poems、/api/poets、/api/search?q=…
// 后台：/admin（默认账号 admin / shici@2024，可用 ADMIN_USER/ADMIN_PASS 覆盖）
package main

import (
	"log"
	"os"

	"github.com/kataras/iris/v12"

	"shici/internal/admin"
	"shici/internal/store"
	"shici/internal/web"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	// 存储后端：设置 SHICI_MYSQL_DSN 时使用 MySQL，否则使用零依赖的 SQLite
	var st *store.Store
	var err error
	if dsn := os.Getenv("SHICI_MYSQL_DSN"); dsn != "" {
		st, err = store.OpenMySQL(dsn)
	} else {
		st, err = store.Open(envOr("SHICI_DB", "data/shici.db"))
	}
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	app := iris.New()
	web.Register(app, st)
	admin.Register(app, st)

	port := envOr("PORT", "8080")
	log.Printf("诗词雅集启动: http://localhost:%s （后台 /admin）", port)
	if err := app.Listen(":"+port, iris.WithoutServerError(iris.ErrServerClosed)); err != nil {
		log.Fatal(err)
	}
}
