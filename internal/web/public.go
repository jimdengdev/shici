package web

import (
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"log"
	"net/url"
	"strconv"
	"strings"

	"github.com/kataras/iris/v12"
	"github.com/kataras/iris/v12/view"

	"shici/internal/model"
	"shici/internal/store"
)

// PageSize 前台每页条数。
const PageSize = 12

// Register 注册视图引擎、静态资源与全部前台路由。
func Register(app *iris.Application, st *store.Store) {
	tmpl := view.HTML(Templates(), ".html").
		Layout("layouts/layout.html")
	app.RegisterView(tmpl)

	// 基础安全响应头（Caddy 也有，直连应用端口时同样生效）
	app.Use(func(ctx iris.Context) {
		ctx.Header("X-Content-Type-Options", "nosniff")
		ctx.Header("X-Frame-Options", "SAMEORIGIN")
		ctx.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		ctx.Next()
	})

	// 静态资源：内容哈希 ETag + 一年 immutable 缓存（URL 带 ?v= 版本）
	app.Get("/static/{file:path}", StaticHandler())

	// 健康检查（HEAD 由 main 统一转成 GET）
	app.Get("/healthz", func(ctx iris.Context) {
		ctx.Header("Cache-Control", "no-store")
		ctx.WriteString("ok")
	})

	// SEO：robots 与分片 sitemap
	app.Get("/robots.txt", robots)
	app.Get("/sitemap.xml", sitemapIndex(st))
	app.Get("/sitemap-pages.xml", sitemapPages(st))
	app.Get("/sitemap-poems/{file:string}", sitemapPoems(st))
	app.Get("/sitemap-poets.xml", sitemapPoets(st))

	// 页面（挂页面缓存中间件：命中直接回 HTML，后台写入/SIGUSR1 会清空）
	pc := PageCache()
	app.Get("/", pc, home(st))
	app.Get("/poems", pc, SearchLimit(), listPoems(st))
	app.Get("/poems/{id:int64}", pc, poemDetail(st))
	app.Get("/poets", pc, listPoets(st))
	app.Get("/poets/{name:string}", pc, poetDetail(st))
	app.Get("/about", pc, about(st))
	app.Get("/random", random(st)) // 每次随机跳转，不缓存

	// JSON API：只开放三个——随机整首、随机一句、按诗名取全篇。
	// 三个接口共用一个 20 QPS 的限流器（挂最外层，超限时连缓存都不查）；
	// 随机接口每次结果不同，不挂页面缓存；按诗名可按 URI 缓存。
	rl := RateLimit()
	app.Get("/api/random", rl, apiRandomPoem(st))
	app.Get("/api/random/line", rl, apiRandomLine(st))
	app.Get("/api/poem", rl, pc, apiPoemByTitle(st)) // /api/poem?title=静夜思[&author=李白]

	// 未知路由：API 返回 JSON，页面返回站内 404
	app.OnErrorCode(iris.StatusNotFound, func(ctx iris.Context) {
		if strings.HasPrefix(ctx.Path(), "/api/") {
			jsonErr(ctx, iris.StatusNotFound, "接口不存在")
			return
		}
		notFound(ctx, st)
	})
}

// render 公共渲染：默认布局 + 公共导航数据。
func render(ctx iris.Context, st *store.Store, name string, data iris.Map) {
	if data == nil {
		data = iris.Map{}
	}
	data["Dynasties"] = st.Dynasties()
	data["AssetVersion"] = AssetVersion()
	data["SiteName"] = siteName
	if _, ok := data["Description"]; !ok {
		data["Description"] = siteDescription
	}
	if _, ok := data["Canonical"]; !ok {
		data["Canonical"] = canonicalURL(ctx)
	}
	if _, ok := data["OGType"]; !ok {
		data["OGType"] = "website"
	}
	ctx.View(name, data)
}

// serverError 记录错误并渲染 500 页面（避免数据库故障时静默显示空列表）。
func serverError(ctx iris.Context, st *store.Store, err error) {
	log.Printf("[web] %s %s: %v", ctx.Method(), ctx.Path(), err)
	ctx.StatusCode(iris.StatusInternalServerError)
	render(ctx, st, "500.html", iris.Map{"Title": "服务异常", "NoIndex": true})
}

// poemJSONLD 诗词详情页的结构化数据（schema.org CreativeWork）。
func poemJSONLD(base string, m model.Poem) htmltemplate.JS {
	ld := map[string]any{
		"@context":    "https://schema.org",
		"@type":       "CreativeWork",
		"name":        m.Title,
		"author":      map[string]any{"@type": "Person", "name": m.Author},
		"inLanguage":  "zh-CN",
		"text":        m.Content,
		"genre":       m.Category,
		"url":         fmt.Sprintf("%s/poems/%d", base, m.ID),
		"description": m.Brief(120),
	}
	b, err := json.Marshal(ld) // encoding/json 会转义 < > &，可安全嵌入 <script>
	if err != nil {
		return ""
	}
	// 必须用 template.JS：html/template 会把 <script> 内的普通字符串按 JS 字符串转义。
	return htmltemplate.JS(b)
}

// ── 页面处理器 ─────────────────────────────────────────

func home(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		daily, err := st.DailyPoem()
		if err != nil {
			daily = model.Poem{}
		}
		featured := st.FeaturedPoems(8)
		stats, err := st.Stats()
		if err != nil {
			log.Printf("[web] 首页统计: %v", err)
		}
		render(ctx, st, "index.html", iris.Map{
			"Title":    "首页",
			"Daily":    daily,
			"Featured": featured,
			"Stats":    stats,
		})
	}
}

func listPoems(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		f := filterFrom(ctx)
		list, total, err := st.ListPoems(&f)
		if err != nil {
			serverError(ctx, st, err)
			return
		}
		pages := store.PageCount(total, f.PageSize)
		desc := fmt.Sprintf("诗词检索：共 %d 首古典诗词，支持关键词、诗人、朝代与体裁筛选。", total)
		if f.Keyword != "" {
			desc = fmt.Sprintf("“%s”相关诗词共 %d 首，支持按诗人、朝代与体裁继续筛选。", f.Keyword, total)
		}
		render(ctx, st, "poems.html", iris.Map{
			"Title":       "诗词检索",
			"Description": desc,
			// 筛选/分页页不参与收录，规范地址统一指向 /poems
			"Canonical": siteBase(ctx) + "/poems",
			"NoIndex":   f.Keyword != "" || f.Poet != "" || f.Dynasty != "" || f.Genre != "" || f.Tag != "" || f.Page > 1,
			"Keyword":   f.Keyword,
			"Poet":      f.Poet,
			"Poems":     list,
			"Total":     total,
			"Page":      f.Page,
			"Pages":     pages,
			"Genres":    st.Genres(),
			"Filter":    f,
			"PageURLs":  pageURLs(ctx, "/poems", f.Page, pages),
		})
	}
}

func poemDetail(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		id := ctx.Params().GetInt64Default("id", 0)
		m, err := st.GetPoem(id)
		if err != nil {
			notFound(ctx, st)
			return
		}
		poet, _ := st.GetPoet(m.Author)
		prev, next := st.PrevNext(m.ID)
		render(ctx, st, "poem.html", iris.Map{
			"Title":       m.Title + " · " + m.Author,
			"Description": fmt.Sprintf("《%s》%s（%s·%s）：%s", m.Title, m.Author, m.Dynasty, m.Category, m.Brief(80)),
			"OGType":      "article",
			"JSONLD":      poemJSONLD(siteBase(ctx), m),
			"Poem":        m,
			"Poet":        poet,
			"Prev":        prev,
			"Next":        next,
		})
	}
}

func listPoets(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		keyword := ctx.URLParam("keyword")
		dynasty := ctx.URLParam("dynasty")
		poets, err := st.ListPoets(keyword, dynasty)
		if err != nil {
			serverError(ctx, st, err)
			return
		}
		// 按朝代分组
		groups := map[string][]model.Poet{}
		var order []string
		for _, p := range poets {
			d := p.Dynasty
			if d == "" {
				d = "其他"
			}
			if _, ok := groups[d]; !ok {
				order = append(order, d)
			}
			groups[d] = append(groups[d], p)
		}
		title := "诗人"
		if keyword != "" {
			title = "诗人检索：“" + keyword + "”"
		}
		desc := fmt.Sprintf("诗人检索：共 %d 位诗人，按作品数排序，附朝代与小传。", len(poets))
		if keyword != "" {
			desc = fmt.Sprintf("“%s”相关诗人共 %d 位。", keyword, len(poets))
		}
		render(ctx, st, "poets.html", iris.Map{
			"Title":       title,
			"Description": desc,
			"Canonical":   siteBase(ctx) + "/poets",
			"NoIndex":     keyword != "" || dynasty != "",
			"Keyword":     keyword,
			"Dynasty":     dynasty,
			"Groups":      groups,
			"Order":       order,
			"Poets":       poets,
			"Total":       len(poets),
			"PageURLs":    nil,
		})
	}
}

func poetDetail(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		name, _ := url.PathUnescape(ctx.Params().Get("name"))
		p, err := st.GetPoet(name)
		if err != nil {
			notFound(ctx, st)
			return
		}
		poems, err := st.PoemsByPoet(p.Name)
		if err != nil {
			serverError(ctx, st, err)
			return
		}
		desc := p.Intro
		if desc == "" {
			desc = fmt.Sprintf("%s（%s）的诗词作品，共收录 %d 首。", p.Name, p.Dynasty, p.PoemCount)
		}
		render(ctx, st, "poet.html", iris.Map{
			"Title":       p.Name,
			"Description": desc,
			"Canonical":   siteBase(ctx) + p.URL(),
			"Poet":        p,
			"Poems":       poems,
		})
	}
}

func random(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		m, err := st.RandomPoem()
		if err != nil {
			notFound(ctx, st)
			return
		}
		ctx.Redirect(fmt.Sprintf("/poems/%d", m.ID))
	}
}

func about(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		render(ctx, st, "about.html", iris.Map{
			"Title":       "关于本站",
			"Description": "诗词雅集是公益中国诗词站点：全部文本属公有领域，免费开放检索与阅读。",
		})
	}
}

// ── JSON API ───────────────────────────────────────────

// poemView 对外的诗词视图：正文同时给出整段与逐行两种形式，空字段不输出。
type poemView struct {
	ID           int64    `json:"id"`
	Title        string   `json:"title"`
	Author       string   `json:"author"`
	Dynasty      string   `json:"dynasty,omitempty"`
	Category     string   `json:"category,omitempty"`
	Content      string   `json:"content"`
	Lines        []string `json:"lines"`
	Translation  string   `json:"translation,omitempty"`
	Annotation   string   `json:"annotation,omitempty"`
	Appreciation string   `json:"appreciation,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	URL          string   `json:"url"`
}

func newPoemView(m model.Poem) poemView {
	return poemView{
		ID:           m.ID,
		Title:        m.Title,
		Author:       m.Author,
		Dynasty:      m.Dynasty,
		Category:     m.Category,
		Content:      m.Content,
		Lines:        m.Lines(),
		Translation:  m.Translation,
		Annotation:   m.Annotation,
		Appreciation: m.Appreciation,
		Tags:         m.TagList(),
		URL:          fmt.Sprintf("/poems/%d", m.ID),
	}
}

func jsonErr(ctx iris.Context, code int, msg string) {
	ctx.StatusCode(code)
	ctx.JSON(iris.Map{"error": msg})
}

// apiRandomPoem 随机整首诗（走随机池，命中即零回源）。
func apiRandomPoem(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		// 结果每次不同，禁止任何中间层缓存，否则会「随机成同一首」
		ctx.Header("Cache-Control", "no-store")
		m, err := st.RandomPoem()
		if err != nil {
			jsonErr(ctx, iris.StatusNotFound, "暂无诗词")
			return
		}
		ctx.JSON(newPoemView(m))
	}
}

// apiRandomLine 随机一首诗中的一句（走随机句池）。
func apiRandomLine(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		ctx.Header("Cache-Control", "no-store")
		res, err := st.RandomLine()
		if err != nil {
			jsonErr(ctx, iris.StatusNotFound, "暂无诗词")
			return
		}
		ctx.JSON(iris.Map{"line": res.Line, "poem": newPoemView(res.Poem)})
	}
}

// apiPoemByTitle 按诗名取整首诗（同名可加 author 消歧）。
func apiPoemByTitle(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		title := strings.TrimSpace(ctx.URLParam("title"))
		if title == "" {
			jsonErr(ctx, iris.StatusBadRequest, "缺少参数 title")
			return
		}
		if len([]rune(title)) > 100 {
			jsonErr(ctx, iris.StatusBadRequest, "title 过长")
			return
		}
		m, err := st.GetPoemByTitle(title, clampRunes(ctx.URLParam("author"), 60))
		if err != nil {
			jsonErr(ctx, iris.StatusNotFound, "诗词不存在")
			return
		}
		ctx.JSON(newPoemView(m))
	}
}

// ── 辅助 ───────────────────────────────────────────────

func filterFrom(ctx iris.Context) store.Filter {
	page := ctx.URLParamIntDefault("page", 1)
	if page < 1 {
		page = 1
	}
	genre := ctx.URLParam("genre") // 兼容 category 写法
	if genre == "" {
		genre = ctx.URLParam("category")
	}
	// 输入长度钳制：避免超长关键词构造昂贵的全文/LIKE 查询与大量缓存键。
	return store.Filter{
		Keyword:  clampRunes(ctx.URLParam("keyword"), 64),
		Poet:     clampRunes(ctx.URLParam("poet"), 60),
		Dynasty:  clampRunes(ctx.URLParam("dynasty"), 24),
		Genre:    clampRunes(genre, 24),
		Tag:      clampRunes(ctx.URLParam("tag"), 32),
		Page:     page,
		PageSize: PageSize,
	}
}

// pageURLs 生成分页导航（保留当前查询参数）。
func pageURLs(ctx iris.Context, base string, page, pages int) iris.Map {
	if pages <= 0 {
		pages = 1
	}
	q := url.Values{}
	for _, k := range []string{"keyword", "poet", "dynasty", "genre", "tag"} {
		if v := ctx.URLParam(k); v != "" {
			q.Set(k, v)
		}
	}
	build := func(p int) string {
		clone := url.Values{}
		for k, vs := range q {
			clone[k] = vs
		}
		if p > 1 {
			clone.Set("page", strconv.Itoa(p))
		}
		if len(clone) == 0 {
			return base
		}
		return base + "?" + clone.Encode()
	}
	var prev, next string
	if page > 1 {
		prev = build(page - 1)
	}
	if page < pages {
		next = build(page + 1)
	}
	return iris.Map{
		"Prev": prev, "Next": next,
		"First": build(1), "Last": build(pages),
		"Pages": pages, "Page": page,
	}
}

// clampRunes 去空白并按字符数截断，防止超长查询参数。
func clampRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func notFound(ctx iris.Context, st *store.Store) {
	ctx.StatusCode(iris.StatusNotFound)
	render(ctx, st, "404.html", iris.Map{"Title": "未找到", "NoIndex": true})
}
