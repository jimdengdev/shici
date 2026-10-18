// shici —— 公益中国诗词网站
//
// 前台：/（今日诗词）、/poems（关键词+诗人检索）、/poets、/poems/{id}、/random、/about
// API ：/api/poems、/api/poets、/api/search?q=…
// 后台：/admin（账号由 ADMIN_USER / ADMIN_PASS（或 ADMIN_HASH）配置）
//
// 配置优先从环境变量读取；未设置时自动加载同目录 .env（.env 已被 .gitignore 忽略）。
package main

import (
	"bufio"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

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

// loadDotEnv 读取 .env（KEY=VALUE，支持 # 注释与引号），仅填充尚未设置的环境变量。
// 环境变量优先，便于容器/ systemd 注入覆盖。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // 没有 .env 属正常情况
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// 去掉成对引号
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func main() {
	loadDotEnv(envOr("SHICI_ENV_FILE", ".env"))

	// 存储后端：设置 SHICI_MYSQL_DSN / DATABASE_URL 时使用 MySQL，否则使用零依赖的 SQLite。
	// 支持 mysql://user:pass@host:port/db 与原生 DSN 两种写法；未写库名时用 SHICI_MYSQL_DB。
	raw := os.Getenv("SHICI_MYSQL_DSN")
	if raw == "" {
		raw = os.Getenv("DATABASE_URL")
	}
	var st *store.Store
	var err error
	if raw != "" {
		dsn, dsnErr := store.NormalizeDSN(raw, envOr("SHICI_MYSQL_DB", store.DefaultMySQLDatabase))
		if dsnErr != nil {
			log.Fatalf("MySQL 连接串无效: %v", dsnErr)
		}
		st, err = store.OpenMySQL(dsn)
	} else {
		st, err = store.Open(envOr("SHICI_DB", "data/shici.db"))
	}
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	if st.ReadOnly() {
		log.Printf("注意：当前数据库账号只有读权限（SELECT），后台的新增/修改/删除将失败")
	}

	// SIGUSR1：手动清空全部缓存。批量导入数据后执行：
	//   systemctl kill -s SIGUSR1 shici
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
			st.InvalidateCache()
		}
	}()

	// 启动预热：提前填充首页/列表/诗人榜缓存，冷启动的访问者不再承担回源耗时
	go st.Warmup()

	app := iris.New()
	// 把 HEAD 当作 GET 路由处理（Iris 只为 GET 注册路由，否则健康检查/监控的
	// HEAD 请求会得到 404）。响应体由 net/http 在 HEAD 语义下丢弃。
	app.WrapRouter(func(w http.ResponseWriter, r *http.Request, router http.HandlerFunc) {
		if r.Method == http.MethodHead {
			r.Method = http.MethodGet
		}
		router(w, r)
	})
	web.Register(app, st)
	admin.Register(app, st)

	// 监听地址：SHICI_HOST 为空时监听全部网卡；部署在反向代理之后时设为回环地址，
	// 只允许本机代理访问，应用端口不对外暴露。
	port := envOr("PORT", "8080")
	addr := ":" + port
	if host := strings.TrimSpace(os.Getenv("SHICI_HOST")); host != "" {
		addr = net.JoinHostPort(host, port)
	}
	log.Printf("诗词雅集启动: %s （后台 /admin）", addr)
	if err := app.Listen(addr, iris.WithoutServerError(iris.ErrServerClosed)); err != nil {
		log.Fatal(err)
	}
}
