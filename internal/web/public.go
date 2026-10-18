package web

import (
	"fmt"
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
	app.HandleDir("/static", Static())

	// 页面（挂页面缓存中间件：命中直接回 HTML，后台写入/SIGUSR1 会清空）
	pc := PageCache()
	app.Get("/", pc, home(st))
	app.Get("/poems", pc, listPoems(st))
	app.Get("/poems/{id:int64}", pc, poemDetail(st))
	app.Get("/poets", pc, listPoets(st))
	app.Get("/poets/{name:string}", pc, poetDetail(st))
	app.Get("/about", pc, about)
	app.Get("/random", random(st)) // 每次随机跳转，不缓存

	// JSON API：只开放三个——随机整首、随机一句、按诗名取全篇。
	// 三个接口共用一个 20 QPS 的限流器（挂最外层，超限时连缓存都不查）；
	// 随机接口每次结果不同，不挂页面缓存；按诗名可按 URI 缓存。
	rl := RateLimit()
	app.Get("/api/random", rl, apiRandomPoem(st))
	app.Get("/api/random/line", rl, apiRandomLine(st))
	app.Get("/api/poem", rl, pc, apiPoemByTitle(st)) // /api/poem?title=静夜思[&author=李白]
}

// render 公共渲染：默认布局 + 公共导航数据。
func render(ctx iris.Context, st *store.Store, name string, data iris.Map) {
	if data == nil {
		data = iris.Map{}
	}
	data["Dynasties"] = st.Dynasties()
	ctx.View(name, data)
}

// ── 页面处理器 ─────────────────────────────────────────

func home(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		daily, err := st.DailyPoem()
		if err != nil {
			daily = model.Poem{}
		}
		featured := st.FeaturedPoems(8)
		stats, _ := st.Stats()
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
		list, total, _ := st.ListPoems(f)
		render(ctx, st, "poems.html", iris.Map{
			"Title":    "诗词检索",
			"Keyword":  f.Keyword,
			"Poet":     f.Poet,
			"Poems":    list,
			"Total":    total,
			"Page":     f.Page,
			"Pages":    (total + f.PageSize - 1) / f.PageSize,
			"Genres":   st.Genres(),
			"Filter":   f,
			"PageURLs": pageURLs(ctx, "/poems", f.Page, (total+f.PageSize-1)/f.PageSize),
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
			"Title": m.Title,
			"Poem":  m,
			"Poet":  poet,
			"Prev":  prev,
			"Next":  next,
		})
	}
}

func listPoets(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		keyword := ctx.URLParam("keyword")
		dynasty := ctx.URLParam("dynasty")
		poets, _ := st.ListPoets(keyword, dynasty)
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
		render(ctx, st, "poets.html", iris.Map{
			"Title":    title,
			"Keyword":  keyword,
			"Groups":   groups,
			"Order":    order,
			"Poets":    poets,
			"Total":    len(poets),
			"PageURLs": nil,
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
		poems, _ := st.PoemsByPoet(p.Name)
		render(ctx, st, "poet.html", iris.Map{
			"Title": p.Name,
			"Poet":  p,
			"Poems": poems,
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

func about(ctx iris.Context) {
	ctx.View("about.html", iris.Map{"Title": "关于本站"})
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
		m, err := st.GetPoemByTitle(title, ctx.URLParam("author"))
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
	return store.Filter{
		Keyword:  ctx.URLParam("keyword"),
		Poet:     ctx.URLParam("poet"),
		Dynasty:  ctx.URLParam("dynasty"),
		Genre:    genre,
		Tag:      ctx.URLParam("tag"),
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

func notFound(ctx iris.Context, st *store.Store) {
	ctx.StatusCode(iris.StatusNotFound)
	render(ctx, st, "404.html", iris.Map{"Title": "未找到"})
}
