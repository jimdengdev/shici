package web

import (
	"fmt"
	"net/url"
	"strconv"

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

	// 页面
	app.Get("/", home(st))
	app.Get("/poems", listPoems(st))
	app.Get("/poems/{id:int64}", poemDetail(st))
	app.Get("/poets", listPoets(st))
	app.Get("/poets/{id:int64}", poetDetail(st))
	app.Get("/random", random(st))
	app.Get("/about", about)

	// JSON API
	app.Get("/api/poems", apiListPoems(st))
	app.Get("/api/poems/{id:int64}", apiPoem(st))
	app.Get("/api/poets", apiListPoets(st))
	app.Get("/api/poets/{id:int64}", apiPoet(st))
	app.Get("/api/search", apiListPoems(st)) // /api/search?q=... 别名
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
		featured, _, _ := st.ListPoems(store.Filter{Page: 1, PageSize: 8})
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
		poet, _ := st.GetPoet(m.PoetID)
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
		id := ctx.Params().GetInt64Default("id", 0)
		p, err := st.GetPoet(id)
		if err != nil {
			notFound(ctx, st)
			return
		}
		poems, _ := st.PoemsByPoet(p.ID)
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

func apiListPoems(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		f := filterFrom(ctx)
		if ctx.URLParam("q") != "" && f.Keyword == "" {
			f.Keyword = ctx.URLParam("q") // /api/search?q=
		}
		list, total, err := st.ListPoems(f)
		if err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.JSON(iris.Map{"error": err.Error()})
			return
		}
		ctx.JSON(iris.Map{
			"items": list, "total": total,
			"page": f.Page, "page_size": f.PageSize,
			"keyword": f.Keyword, "poet": f.Poet,
			"dynasty": f.Dynasty, "genre": f.Genre,
		})
	}
}

func apiPoem(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		m, err := st.GetPoem(ctx.Params().GetInt64Default("id", 0))
		if err != nil {
			ctx.StatusCode(iris.StatusNotFound)
			ctx.JSON(iris.Map{"error": "诗词不存在"})
			return
		}
		ctx.JSON(iris.Map{"item": m})
	}
}

func apiListPoets(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		poets, err := st.ListPoets(ctx.URLParam("keyword"), ctx.URLParam("dynasty"))
		if err != nil {
			ctx.StatusCode(iris.StatusInternalServerError)
			ctx.JSON(iris.Map{"error": err.Error()})
			return
		}
		ctx.JSON(iris.Map{"items": poets, "total": len(poets)})
	}
}

func apiPoet(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		id := ctx.Params().GetInt64Default("id", 0)
		p, err := st.GetPoet(id)
		if err != nil {
			ctx.StatusCode(iris.StatusNotFound)
			ctx.JSON(iris.Map{"error": "诗人不存在"})
			return
		}
		poems, _ := st.PoemsByPoet(id)
		ctx.JSON(iris.Map{"item": p, "poems": poems})
	}
}

// ── 辅助 ───────────────────────────────────────────────

func filterFrom(ctx iris.Context) store.Filter {
	page := ctx.URLParamIntDefault("page", 1)
	if page < 1 {
		page = 1
	}
	return store.Filter{
		Keyword:  ctx.URLParam("keyword"),
		Poet:    ctx.URLParam("poet"),
		Dynasty: ctx.URLParam("dynasty"),
		Genre:   ctx.URLParam("genre"),
		Tag:     ctx.URLParam("tag"),
		Page:    page,
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
