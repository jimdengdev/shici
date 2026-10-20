package web

import (
	"net/http"
	"strings"

	"github.com/kataras/iris/v12"
)

// StaticHandler 提供带内容哈希 ETag 与长缓存的静态资源。
// 模板以 /static/style.css?v=<AssetVersion> 引用：内容不变时浏览器一年内不再请求，
// 内容变化时 URL 版本号同步变化，不会读到旧样式。
func StaticHandler() iris.Handler {
	files := http.StripPrefix("/static/", http.FileServer(http.FS(Static())))
	return func(ctx iris.Context) {
		name := strings.TrimPrefix(ctx.Params().Get("file"), "/")
		etag, ok := assetETags[name]
		if !ok {
			ctx.NotFound()
			return
		}
		ctx.Header("ETag", etag)
		ctx.Header("Cache-Control", "public, max-age=31536000, immutable")
		// http.ServeContent（FileServer 内部）会读取已设置的 ETag 处理 If-None-Match。
		files.ServeHTTP(ctx.ResponseWriter(), ctx.Request())
	}
}
