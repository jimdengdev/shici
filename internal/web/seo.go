package web

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/kataras/iris/v12"

	"shici/internal/store"
)

const (
	siteName        = "诗词雅集"
	siteDescription = "公益中国诗词站点：免费检索、阅读中国古典诗词名篇，收录诗词、诗人小传与译文注释赏析。"
)

// siteBase 站点对外根地址：优先 SHICI_SITE_URL（推荐，避免代理头缺失），
// 其次按请求的 X-Forwarded-Proto / TLS / Host 推断。
func siteBase(ctx iris.Context) string {
	if v := strings.TrimSpace(os.Getenv("SHICI_SITE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	scheme := "http"
	if ctx.Request().TLS != nil {
		scheme = "https"
	}
	if p := ctx.GetHeader("X-Forwarded-Proto"); p != "" {
		if i := strings.IndexByte(p, ','); i >= 0 {
			p = p[:i]
		}
		if p = strings.TrimSpace(p); p != "" {
			scheme = p
		}
	}
	return scheme + "://" + ctx.Host()
}

// canonicalURL 当前页面的规范地址（去掉查询串，由调用方决定是否覆盖）。
func canonicalURL(ctx iris.Context) string {
	return siteBase(ctx) + ctx.Request().URL.Path
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func writeXML(ctx iris.Context, body string) {
	ctx.Header("Content-Type", "application/xml; charset=utf-8")
	ctx.Header("Cache-Control", "public, max-age=3600")
	ctx.WriteString(body)
}

// robots 告诉爬虫后台/API 不必抓取，并声明 sitemap。
func robots(ctx iris.Context) {
	ctx.Header("Content-Type", "text/plain; charset=utf-8")
	ctx.Header("Cache-Control", "public, max-age=86400")
	fmt.Fprintf(ctx, "User-agent: *\nDisallow: /admin\nDisallow: /api/\nAllow: /\n\nSitemap: %s/sitemap.xml\n", siteBase(ctx))
}

// sitemapIndex 分片索引：页面、诗人、诗词按 45k 一片。
func sitemapIndex(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		base := siteBase(ctx)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
		for _, name := range []string{"sitemap-pages.xml", "sitemap-poets.xml"} {
			fmt.Fprintf(&b, "  <sitemap><loc>%s/%s</loc></sitemap>\n", xmlEscape(base), name)
		}
		for i := 0; i < st.SitemapPoemShards(); i++ {
			fmt.Fprintf(&b, "  <sitemap><loc>%s/sitemap-poems/%d.xml</loc></sitemap>\n", xmlEscape(base), i)
		}
		b.WriteString("</sitemapindex>\n")
		writeXML(ctx, b.String())
	}
}

// sitemapPages 固定页面。
func sitemapPages(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		base := siteBase(ctx)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
		for _, path := range []string{"/", "/poems", "/poets", "/about"} {
			fmt.Fprintf(&b, "  <url><loc>%s</loc></url>\n", xmlEscape(base+path))
		}
		b.WriteString("</urlset>\n")
		writeXML(ctx, b.String())
	}
}

// sitemapPoems 单个分片内的诗词详情页。
func sitemapPoems(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		// 路由为 /sitemap-poems/{n}.xml（Iris 参数需独占路径段，故这里自行去掉后缀）
		name := strings.TrimSuffix(ctx.Params().Get("file"), ".xml")
		shard, err := strconv.Atoi(name)
		if err != nil {
			ctx.NotFound()
			return
		}
		ids, err := st.SitemapPoemIDs(shard)
		if err != nil {
			ctx.StatusCode(http.StatusInternalServerError)
			ctx.WriteString("sitemap 生成失败")
			return
		}
		if len(ids) == 0 {
			ctx.NotFound()
			return
		}
		base := siteBase(ctx)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
		for _, id := range ids {
			fmt.Fprintf(&b, "  <url><loc>%s/poems/%d</loc></url>\n", xmlEscape(base), id)
		}
		b.WriteString("</urlset>\n")
		writeXML(ctx, b.String())
	}
}

// sitemapPoets 诗人详情页。
func sitemapPoets(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		poets, err := st.SitemapPoetNames()
		if err != nil {
			ctx.StatusCode(http.StatusInternalServerError)
			ctx.WriteString("sitemap 生成失败")
			return
		}
		base := siteBase(ctx)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
		for _, p := range poets {
			fmt.Fprintf(&b, "  <url><loc>%s</loc></url>\n", xmlEscape(base+p.URL()))
		}
		b.WriteString("</urlset>\n")
		writeXML(ctx, b.String())
	}
}
