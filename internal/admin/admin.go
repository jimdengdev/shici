// Package admin 后台管理：session 登录鉴权 + 诗词/诗人 CRUD。
// 账号通过环境变量 ADMIN_USER / ADMIN_PASS 配置（默认 admin / shici@2024）。
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kataras/iris/v12"
	"github.com/kataras/iris/v12/sessions"
	"golang.org/x/crypto/bcrypt"

	"shici/internal/model"
	"shici/internal/store"
	"shici/internal/web"
)

// AdminPageSize 后台列表每页条数。
const AdminPageSize = 20

// sess 在 Register 中初始化：需等 main 读取完 .env 后再读环境变量。
var sess *sessions.Sessions

// Register 注册后台路由（/admin/**）。
func Register(app *iris.Application, st *store.Store) {
	sess = sessions.New(sessions.Config{
		Cookie:  "shici_admin",
		Expires: 12 * time.Hour,
		// 反向代理 / 直接用 IP 访问时不要把 Cookie 的 Domain 固定为请求子域，
		// 否则浏览器会因 Domain 与当前主机不一致而丢弃会话 Cookie。
		DisableSubdomainPersistence: true,
		// 部署在 HTTPS（Caddy）之后时设置 SHICI_COOKIE_SECURE=1，让 Cookie 带 Secure。
		CookieSecureTLS: os.Getenv("SHICI_COOKIE_SECURE") == "1",
	})

	// 登录无需鉴权，但仍校验 CSRF
	app.Get("/admin/login", loginForm)
	app.Post("/admin/login", loginSubmit())

	// 其余后台路由需登录 + POST 需 CSRF 令牌
	p := app.Party("/admin", requireAuth, requireCSRF)
	p.Get("/", dashboard(st))
	p.Get("/logout", logout)

	poems := p.Party("/poems")
	poems.Get("/", poemList(st))
	poems.Get("/new", poemFormNew(st))
	poems.Post("/new", poemCreate(st))
	poems.Get("/{id:int64}/edit", poemFormEdit(st))
	poems.Post("/{id:int64}/edit", poemUpdate(st))
	poems.Post("/{id:int64}/delete", poemDelete(st))

	// 诗人由 poems.author 聚合派生，以姓名作为标识；不支持新增（随作品产生）
	poets := p.Party("/poets")
	poets.Get("/", poetList(st))
	poets.Get("/{name:string}/edit", poetFormEdit(st))
	poets.Post("/{name:string}/edit", poetUpdate(st))
	poets.Post("/{name:string}/delete", poetDelete(st))
}

// ── 鉴权 ───────────────────────────────────────────────

// genPassOnce 在未配置密码时生成一次性临时密码（不把默认密码写死在代码里）。
var (
	genPassOnce sync.Once
	genPass     string
)

func credentials() (user, pass string) {
	user = os.Getenv("ADMIN_USER")
	pass = os.Getenv("ADMIN_PASS")
	if user == "" {
		user = "admin"
	}
	if pass == "" && os.Getenv("ADMIN_HASH") == "" {
		genPassOnce.Do(func() {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				log.Fatalf("生成后台临时密码失败: %v", err)
			}
			genPass = hex.EncodeToString(b)
			log.Printf("[admin] 未配置 ADMIN_PASS / ADMIN_HASH，已生成临时密码：%s（仅本次进程有效，重启后重置）", genPass)
		})
		pass = genPass
	}
	return
}

// csrfToken 返回当前会话的 CSRF 令牌（没有则生成）。
func csrfToken(ctx iris.Context) string {
	s := sess.Start(ctx)
	if t := s.GetString("csrf"); t != "" {
		return t
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	t := hex.EncodeToString(b)
	s.Set("csrf", t)
	return t
}

// checkCSRF 常量时间比对表单里的 CSRF 令牌与会话中的令牌。
func checkCSRF(ctx iris.Context) bool {
	want := sess.Start(ctx).GetString("csrf")
	got := ctx.FormValue("csrf")
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// requireCSRF 对后台写操作校验 CSRF 令牌（GET 只读操作放行）。
func requireCSRF(ctx iris.Context) {
	switch ctx.Method() {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		if !checkCSRF(ctx) {
			ctx.StatusCode(iris.StatusForbidden)
			ctx.WriteString("CSRF 校验失败，请刷新页面后重试")
			ctx.StopExecution()
			return
		}
	}
	ctx.Next()
}

// view 统一渲染后台页面：自动注入 CSRF 令牌供表单使用。
func view(ctx iris.Context, name string, data iris.Map) {
	if data == nil {
		data = iris.Map{}
	}
	data["CSRF"] = csrfToken(ctx)
	data["AssetVersion"] = web.AssetVersion()
	ctx.View(name, data)
}

// ── 登录限流 ───────────────────────────────────────────

const (
	loginMaxFails = 10
	loginWindow   = 10 * time.Minute
)

type failEntry struct {
	n     int
	reset time.Time
}

var loginFailures = struct {
	sync.Mutex
	m map[string]*failEntry
}{m: make(map[string]*failEntry)}

func clientIP(ctx iris.Context) string {
	host, _, err := net.SplitHostPort(ctx.RemoteAddr())
	if err != nil {
		return ctx.RemoteAddr()
	}
	return host
}

func loginBlocked(ip string) bool {
	loginFailures.Lock()
	defer loginFailures.Unlock()
	e, ok := loginFailures.m[ip]
	if !ok {
		return false
	}
	if time.Now().After(e.reset) {
		delete(loginFailures.m, ip)
		return false
	}
	return e.n >= loginMaxFails
}

func loginFail(ip string) {
	loginFailures.Lock()
	defer loginFailures.Unlock()
	e, ok := loginFailures.m[ip]
	if !ok || time.Now().After(e.reset) {
		e = &failEntry{reset: time.Now().Add(loginWindow)}
		loginFailures.m[ip] = e
	}
	e.n++
	// 简单的过期清理，避免 IP 表无限增长
	if len(loginFailures.m) > 5000 {
		now := time.Now()
		for k, v := range loginFailures.m {
			if now.After(v.reset) {
				delete(loginFailures.m, k)
			}
		}
	}
}

func loginSucceeded(ip string) {
	loginFailures.Lock()
	delete(loginFailures.m, ip)
	loginFailures.Unlock()
}

func requireAuth(ctx iris.Context) {
	if sess.Start(ctx).GetString("user") == "" {
		ctx.Redirect("/admin/login")
		return
	}
	ctx.Next()
}

func loginPage(ctx iris.Context, status int, msg string) {
	ctx.StatusCode(status)
	ctx.ViewLayout(iris.NoLayout)
	view(ctx, "admin/login.html", iris.Map{"Title": "登录", "Error": msg})
}

func loginForm(ctx iris.Context) {
	msg := ""
	switch ctx.URLParam("e") {
	case "1":
		msg = "账号或密码不正确"
	case "2":
		msg = "尝试过于频繁，请稍后再试"
	}
	loginPage(ctx, iris.StatusOK, msg)
}

func loginSubmit() iris.Handler {
	return func(ctx iris.Context) {
		ip := clientIP(ctx)
		if loginBlocked(ip) {
			loginPage(ctx, iris.StatusTooManyRequests, "尝试过于频繁，请稍后再试")
			return
		}
		if !checkCSRF(ctx) {
			loginPage(ctx, iris.StatusForbidden, "页面已过期，请刷新后重试")
			return
		}

		user, pass := credentials()
		// 用户名与密码都用常量时间比较，避免通过响应时间探测
		userOK := subtle.ConstantTimeCompare([]byte(ctx.FormValue("username")), []byte(user)) == 1
		var passOK bool
		if hash := os.Getenv("ADMIN_HASH"); hash != "" {
			passOK = bcrypt.CompareHashAndPassword([]byte(hash), []byte(ctx.FormValue("password"))) == nil
		} else {
			passOK = subtle.ConstantTimeCompare([]byte(ctx.FormValue("password")), []byte(pass)) == 1
		}
		if !userOK || !passOK {
			loginFail(ip)
			loginPage(ctx, iris.StatusOK, "账号或密码不正确")
			return
		}

		loginSucceeded(ip)
		s := sess.Start(ctx)
		s.Delete("csrf") // 登录成功后轮换令牌，防会话固定
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
		view(ctx, "admin/dashboard.html", iris.Map{
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
			Page: page, PageSize: AdminPageSize, AllStatus: true,
		}
		list, total, _ := st.ListPoems(&f)
		pages := store.PageCount(total, AdminPageSize)
		view(ctx, "admin/poems.html", iris.Map{
			"Title": "诗词管理", "Poems": list, "Total": total,
			"Filter": f, "Page": f.Page,
			"Pages":    pages,
			"Genres":   st.Genres(),
			"PageURLs": adminPageURLs(ctx, "/admin/poems", f.Page, pages),
		})
	}
}

func poemFormData(st *store.Store, m model.Poem, action string, errMsg string) iris.Map {
	return iris.Map{
		"Title":     "编辑诗词",
		"Poem":      m,
		"Action":    action,
		"Poets":     st.PoetOptions(),
		"Dynasties": st.Dynasties(),
		"Genres":    st.Genres(),
		"Error":     errMsg,
	}
}

func poemFormNew(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		view(ctx, "admin/poem_form.html",
			poemFormData(st, model.Poem{Category: "古诗", Status: "published"}, "/admin/poems/new", ""))
	}
}

func poemFromForm(ctx iris.Context) (m model.Poem) {
	m.ID = store.AtoiID(ctx.FormValue("id"))
	m.Title = ctx.FormValue("title")
	m.Author = ctx.FormValue("author")
	m.Dynasty = ctx.FormValue("dynasty")
	m.Category = ctx.FormValue("category")
	m.Content = ctx.FormValue("content")
	m.Translation = ctx.FormValue("translation")
	m.Annotation = ctx.FormValue("annotation")
	m.Appreciation = ctx.FormValue("appreciation")
	m.Tags = ctx.FormValue("tags")
	m.Featured = ctx.FormValue("featured") == "1"
	m.Status = ctx.FormValue("status")
	return
}

func poemValid(m model.Poem) bool {
	return m.Title != "" && m.Author != "" && m.Content != ""
}

func poemCreate(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		m := poemFromForm(ctx)
		if !poemValid(m) {
			view(ctx, "admin/poem_form.html", poemFormData(st, m, "/admin/poems/new", "请填写标题、诗人与正文"))
			return
		}
		if _, err := st.CreatePoem(m); err != nil {
			view(ctx, "admin/poem_form.html", poemFormData(st, m, "/admin/poems/new", "保存失败："+err.Error()))
			return
		}
		ctx.Redirect("/admin/poems")
	}
}

func poemFormEdit(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		id := ctx.Params().GetInt64Default("id", 0)
		m, err := st.GetPoemForAdmin(id)
		if err != nil {
			ctx.Redirect("/admin/poems")
			return
		}
		view(ctx, "admin/poem_form.html",
			poemFormData(st, m, fmt.Sprintf("/admin/poems/%d/edit", id), ""))
	}
}

func poemUpdate(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		id := ctx.Params().GetInt64Default("id", 0)
		m := poemFromForm(ctx)
		m.ID = id
		if !poemValid(m) {
			view(ctx, "admin/poem_form.html",
				poemFormData(st, m, fmt.Sprintf("/admin/poems/%d/edit", id), "请填写标题、诗人与正文"))
			return
		}
		if err := st.UpdatePoem(m); err != nil {
			view(ctx, "admin/poem_form.html",
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
// 诗人由 poems.author/dynasty 聚合派生：不支持新增，编辑即批量改写其作品上的
// 作者/朝代，删除即删除其全部作品。

func poetList(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		poets, _ := st.ListPoets(ctx.URLParam("keyword"), "")
		view(ctx, "admin/poets.html", iris.Map{
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

// poetName 取 URL 中的诗人姓名（已做路径反转义）。
func poetName(ctx iris.Context) string {
	name, _ := url.PathUnescape(ctx.Params().Get("name"))
	return name
}

func poetFormEdit(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		p, err := st.GetPoet(poetName(ctx))
		if err != nil {
			ctx.Redirect("/admin/poets")
			return
		}
		view(ctx, "admin/poet_form.html", poetFormData(p, p.EditURL(), ""))
	}
}

func poetUpdate(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		oldName := poetName(ctx)
		name := strings.TrimSpace(ctx.FormValue("name"))
		dynasty := strings.TrimSpace(ctx.FormValue("dynasty"))
		if name == "" {
			cur, _ := st.GetPoet(oldName)
			view(ctx, "admin/poet_form.html", poetFormData(cur, cur.EditURL(), "请填写诗人姓名"))
			return
		}
		if err := st.RenamePoet(oldName, name, dynasty); err != nil {
			cur, _ := st.GetPoet(oldName)
			view(ctx, "admin/poet_form.html", poetFormData(cur, cur.EditURL(), "保存失败："+err.Error()))
			return
		}
		ctx.Redirect("/admin/poets")
	}
}

func poetDelete(st *store.Store) iris.Handler {
	return func(ctx iris.Context) {
		st.DeletePoet(poetName(ctx))
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
