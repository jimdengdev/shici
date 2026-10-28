package web

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
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
		key := pageCacheKey(ctx.Request())

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

// cacheJunkParams 与内容无关的跟踪参数：不影响渲染结果，不应产生独立缓存条目。
var cacheJunkParams = map[string]bool{
	"_": true, "t": true, "fbclid": true, "gclid": true, "spm": true, "ref": true,
}

// pageCacheKey 规范化缓存键：排序查询参数、丢弃空值与跟踪参数，
// 避免同一页面因参数顺序或 utm_* 产生大量重复条目（缓存污染）。
func pageCacheKey(r *http.Request) string {
	raw := r.RequestURI
	if raw == "" {
		raw = r.URL.RequestURI() // 测试/客户端构造的请求没有 RequestURI
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, vs := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || cacheJunkParams[lk] ||
			len(vs) == 0 || (len(vs) == 1 && vs[0] == "") {
			delete(q, k)
		}
	}
	if len(q) == 0 {
		return u.Path
	}
	return u.Path + "?" + q.Encode()
}

// teeWriter 包装原始 ResponseWriter：正常写回客户端的同时捕获 200 响应体。
// 缓存头也在首次写响应时设置：只有 200 才声明可缓存，4xx/5xx 一律 no-store，
// 避免 404 等错误页被浏览器或中间层缓存。
type teeWriter struct {
	context.ResponseWriter
	buf         bytes.Buffer
	code        int
	wroteHeader bool
}

func (w *teeWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.code = code
	h := w.ResponseWriter.Header()
	if code == http.StatusOK {
		h.Set("X-Cache", "MISS")
		h.Set("Cache-Control", "public, max-age=60")
	} else {
		h.Del("X-Cache")
		h.Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *teeWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if w.code == http.StatusOK {
		w.buf.Write(p)
	}
	return n, err
}
