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
	dbPath := envOr("SHICI_DB", "data/shici.db")
	st, err := store.Open(dbPath)
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
