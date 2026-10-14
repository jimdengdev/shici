// Package admin 后台管理：session 登录鉴权 + 诗词/诗人 CRUD。
// 账号通过环境变量 ADMIN_USER / ADMIN_PASS 配置（默认 admin / shici@2024）。
package admin

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/kataras/iris/v12"
	"github.com/kataras/iris/v12/sessions"
	"golang.org/x/crypto/bcrypt"

	"shici/internal/model"
	"shici/internal/store"
)

// AdminPageSize 后台列表每页条数。
const AdminPageSize = 20

var sess = sessions.New(sessions.Config{
	Cookie:  "shici_admin",
	Expires: 12 * time.Hour,
})

// Register 注册后台路由（/admin/**）。
func Register(app *iris.Application, st *store.Store) {
	// 登录无需鉴权
	app.Get("/admin/login", loginForm)
	app.Post("/admin/login", loginSubmit(st))

	// 其余后台路由需登录
	p := app.Party("/admin", requireAuth)
	p.Get("/", dashboard(st))
	p.Get("/logout", logout)

	poems := p.Party("/poems")
	poems.Get("/", poemList(st))
	poems.Get("/new", poemFormNew(st))
	poems.Post("/new", poemCreate(st))
	poems.Get("/{id:int64}/edit", poemFormEdit(st))
	poems.Post("/{id:int64}/edit", poemUpdate(st))
	poems.Post("/{id:int64}/delete", poemDelete(st))

	poets := p.Party("/poets")
	poets.Get("/", poetList(st))
	poets.Get("/new", poetFormNew)
	poets.Post("/new", poetCreate(st))
	poets.Get("/{id:int64}/edit", poetFormEdit(st))
	poets.Post("/{id:int64}/edit", poetUpdate(st))
	poets.Post("/{id:int64}/delete", poetDelete(st))
}

// ── 鉴权 ───────────────────────────────────────────────

func credentials() (user, pass string) {
	user = os.Getenv("ADMIN_USER")
	pass = os.Getenv("ADMIN_PASS")
	if user == "" {
		user = "admin"
	}
	if pass == "" {
		pass = "shici@2024"
	}
	return
}

func requireAuth(ctx iris.Context) {
	if sess.Start(ctx).GetString("user") == "" {
		ctx.Redirect("/admin/login")
		return
	}
	ctx.Next()
}

func loginForm(ctx iris.Context) {
	ctx.ViewLayout(iris.NoLayout)
	ctx.View("admin/login.html", iris.Map{
		"Title": "登录", "Error": ctx.URLParam("e") == "1",
	})
}

func loginSubmit(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		user, pass := credentials()
		if ctx.FormValue("username") != user {
			ctx.Redirect("/admin/login?e=1")
			return
		}
		// 默认密码为明文比对；若设置了 ADMIN_HASH（bcrypt），则用哈希校验
		hash := os.Getenv("ADMIN_HASH")
		if hash != "" {
			if bcrypt.CompareHashAndPassword([]byte(hash), []byte(ctx.FormValue("password"))) != nil {
				ctx.Redirect("/admin/login?e=1")
				return
			}
		} else if ctx.FormValue("password") != pass {
			ctx.Redirect("/admin/login?e=1")
			return
		}
		s := sess.Start(ctx)
		s.Set("user", user)
		ctx.Redirect("/admin")
	}
}

func logout(ctx iris.Context) {
	s := sess.Start(ctx)
	s.Delete("user")
	ctx.Redirect("/admin/login")
}

// ── 仪表盘 ─────────────────────────────────────────────

func dashboard(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		stats, err := st.Stats()
		if err != nil {
			stats = store.Stats{}
		}
		ctx.View("admin/dashboard.html", iris.Map{
			"Title": "仪表盘", "Stats": stats,
		})
	}
}

// ── 诗词管理 ───────────────────────────────────────────

func poemList(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		page := clampPage(ctx.URLParamIntDefault("page", 1))
		f := store.Filter{
			Keyword: ctx.URLParam("keyword"), Poet: ctx.URLParam("poet"),
			Dynasty: ctx.URLParam("dynasty"), Genre: ctx.URLParam("genre"),
			Page: page, PageSize: AdminPageSize,
		}
		list, total, _ := st.ListPoems(f)
		ctx.View("admin/poems.html", iris.Map{
			"Title": "诗词管理", "Poems": list, "Total": total,
			"Filter": f, "Page": page,
			"Pages":   (total + AdminPageSize - 1) / AdminPageSize,
			"Genres":  st.Genres(),
			"PageURLs": adminPageURLs(ctx, "/admin/poems", page,
				(total+AdminPageSize-1)/AdminPageSize),
		})
	}
}

func poemFormData(st *store.Store, m model.Poem, action string, errMsg string) iris.Map {
	return iris.Map{
		"Title":  "编辑诗词",
		"Poem":   m,
		"Action": action,
		"Poets":  st.PoetOptions(),
		"Error":  errMsg,
	}
}

func poemFormNew(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		ctx.View("admin/poem_form.html", poemFormData(st, model.Poem{PoetID: firstPoetID(st)}, "/admin/poems/new", ""))
	}
}

func firstPoetID(st *store.Store) int64 {
	if poets := st.PoetOptions(); len(poets) > 0 {
		return poets[0].ID
	}
	return 0
}

func poemFromForm(ctx iris.Context) (m model.Poem) {
	m.ID = store.AtoiID(ctx.FormValue("id"))
	m.Title = ctx.FormValue("title")
	m.PoetID = store.AtoiID(ctx.FormValue("poet_id"))
	m.Genre = ctx.FormValue("genre")
	m.Content = ctx.FormValue("content")
	m.Tags = ctx.FormValue("tags")
	m.Note = ctx.FormValue("note")
	return
}

func poemValid(m model.Poem) bool {
	return m.Title != "" && m.PoetID > 0 && m.Content != ""
}

func poemCreate(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		m := poemFromForm(ctx)
		if !poemValid(m) {
			ctx.View("admin/poem_form.html", poemFormData(st, m, "/admin/poems/new", "请填写标题、诗人与正文"))
			return
		}
		if _, err := st.CreatePoem(m); err != nil {
			ctx.View("admin/poem_form.html", poemFormData(st, m, "/admin/poems/new", "保存失败："+err.Error()))
			return
		}
		ctx.Redirect("/admin/poems")
	}
}

func poemFormEdit(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		id := ctx.Params().GetInt64Default("id", 0)
		m, err := st.GetPoem(id)
		if err != nil {
			ctx.Redirect("/admin/poems")
			return
		}
		ctx.View("admin/poem_form.html",
			poemFormData(st, m, fmt.Sprintf("/admin/poems/%d/edit", id), ""))
	}
}

func poemUpdate(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		id := ctx.Params().GetInt64Default("id", 0)
		m := poemFromForm(ctx)
		m.ID = id
		if !poemValid(m) {
			ctx.View("admin/poem_form.html",
				poemFormData(st, m, fmt.Sprintf("/admin/poems/%d/edit", id), "请填写标题、诗人与正文"))
			return
		}
		if err := st.UpdatePoem(m); err != nil {
			ctx.View("admin/poem_form.html",
				poemFormData(st, m, fmt.Sprintf("/admin/poems/%d/edit", id), "保存失败："+err.Error()))
			return
		}
		ctx.Redirect("/admin/poems")
	}
}

func poemDelete(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		st.DeletePoem(ctx.Params().GetInt64Default("id", 0))
		ctx.Redirect("/admin/poems")
	}
}

// ── 诗人管理 ───────────────────────────────────────────

func poetList(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		poets, _ := st.ListPoets(ctx.URLParam("keyword"), "")
		ctx.View("admin/poets.html", iris.Map{
			"Title": "诗人管理", "Poets": poets, "Total": len(poets),
			"Keyword": ctx.URLParam("keyword"),
		})
	}
}

func poetFormData(p model.Poet, action string, errMsg string) iris.Map {
	return iris.Map{
		"Title": "编辑诗人", "Poet": p, "Action": action, "Error": errMsg,
	}
}

func poetFormNew(ctx iris.Context) {
	ctx.View("admin/poet_form.html", poetFormData(model.Poet{}, "/admin/poets/new", ""))
}

func poetFromForm(ctx iris.Context) (p model.Poet) {
	p.ID = store.AtoiID(ctx.FormValue("id"))
	p.Name = ctx.FormValue("name")
	p.Dynasty = ctx.FormValue("dynasty")
	p.Intro = ctx.FormValue("intro")
	return
}

func poetValid(p model.Poet) bool { return p.Name != "" }

func poetCreate(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		p := poetFromForm(ctx)
		if !poetValid(p) {
			ctx.View("admin/poet_form.html", poetFormData(p, "/admin/poets/new", "请填写诗人姓名"))
			return
		}
		if _, err := st.CreatePoet(p.Name, p.Dynasty, p.Intro); err != nil {
			ctx.View("admin/poet_form.html", poetFormData(p, "/admin/poets/new", "保存失败："+err.Error()))
			return
		}
		ctx.Redirect("/admin/poets")
	}
}

func poetFormEdit(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		id := ctx.Params().GetInt64Default("id", 0)
		p, err := st.GetPoet(id)
		if err != nil {
			ctx.Redirect("/admin/poets")
			return
		}
		ctx.View("admin/poet_form.html",
			poetFormData(p, fmt.Sprintf("/admin/poets/%d/edit", id), ""))
	}
}

func poetUpdate(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		id := ctx.Params().GetInt64Default("id", 0)
		p := poetFromForm(ctx)
		p.ID = id
		if !poetValid(p) {
			ctx.View("admin/poet_form.html",
				poetFormData(p, fmt.Sprintf("/admin/poets/%d/edit", id), "请填写诗人姓名"))
			return
		}
		if err := st.UpdatePoet(p.ID, p.Name, p.Dynasty, p.Intro); err != nil {
			ctx.View("admin/poet_form.html",
				poetFormData(p, fmt.Sprintf("/admin/poets/%d/edit", id), "保存失败："+err.Error()))
			return
		}
		ctx.Redirect("/admin/poets")
	}
}

func poetDelete(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		st.DeletePoet(ctx.Params().GetInt64Default("id", 0))
		ctx.Redirect("/admin/poets")
	}
}

// ── 辅助 ───────────────────────────────────────────────

func clampPage(p int) int {
	if p < 1 {
		return 1
	}
	return p
}

// adminPageURLs 后台分页（保留筛选参数）。
func adminPageURLs(ctx iris.Context, base string, page, pages int) iris.Map {
	q := url.Values{}
	for _, k := range []string{"keyword", "poet", "dynasty", "genre"} {
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
	return iris.Map{"Prev": prev, "Next": next, "Pages": pages, "Page": page}
}
