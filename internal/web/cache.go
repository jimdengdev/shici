package web

import (
	"bytes"
	"net/http"
	"time"

	"github.com/kataras/iris/v12"
	"github.com/kataras/iris/v12/context"

	"shici/internal/memo"
)

// 页面级缓存（L2）：把渲染后的 HTML/JSON 按请求 URI 缓存，命中即省掉
// 全部数据库往返。内容型站点数据极少变化，策略与数据层一致：
//
//	长 TTL + 后台写入失效（invalidateMeta → memo.FlushAll）+ SIGUSR1 手动清空。
//
// /admin 路由不经过本中间件（admin.Register 独立注册），/random 保持每次随机。
const (
	pageTTL          = 30 * time.Minute
	pageMaxEntries   = 1024
	pageMaxBytes     = int64(32 << 20) // 32MiB
	pageSingleMaxLen = 4 << 20         // 单页超过 4MiB 不缓存
)

type cachedPage struct {
	body  []byte
	ctype string
}

// PageCache 页面缓存中间件。注册在具体路由之前：
//
//	app.Get("/poems", web.PageCache(), listPoems(st))
func PageCache() iris.Handler {
	cache := memo.New[string, cachedPage]("page", pageTTL, pageMaxEntries, pageMaxBytes,
		func(p cachedPage) int64 { return int64(len(p.body) + 128) })

	return func(ctx iris.Context) {
		// 只缓存无副作用的 GET；后台/API 之外的路径由注册处控制
		if ctx.Method() != http.MethodGet {
			ctx.Next()
			return
		}
		key := ctx.Request().RequestURI

		if page, ok := cache.Peek(key); ok {
			w := ctx.ResponseWriter()
			h := w.Header()
			if page.ctype != "" {
				h.Set("Content-Type", page.ctype)
			}
			h.Set("X-Cache", "HIT")
			h.Set("Cache-Control", "public, max-age=60")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(page.body)
			ctx.StopExecution()
			return
		}

		// 未命中：透传渲染结果，同时捕获 200 响应写入缓存
		orig := ctx.ResponseWriter()
		tee := &teeWriter{ResponseWriter: orig}
		ctx.ResetResponseWriter(tee)
		orig.Header().Set("X-Cache", "MISS")
		orig.Header().Set("Cache-Control", "public, max-age=60")
		ctx.Next()
		ctx.ResetResponseWriter(orig)

		if tee.code == http.StatusOK && tee.buf.Len() > 0 && tee.buf.Len() < pageSingleMaxLen {
			cache.Set(key, cachedPage{
				body:  append([]byte(nil), tee.buf.Bytes()...),
				ctype: orig.Header().Get("Content-Type"),
			})
		}
	}
}

// teeWriter 包装原始 ResponseWriter：正常写回客户端的同时捕获 200 响应体。
type teeWriter struct {
	context.ResponseWriter
	buf  bytes.Buffer
	code int
}

func (w *teeWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *teeWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	if w.code == http.StatusOK {
		w.buf.Write(p)
	}
	return n, err
}
