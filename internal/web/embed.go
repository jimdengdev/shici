// Package web 前台：模板与静态资源内嵌，公开路由（页面 + JSON API）。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:templates all:static
var assets embed.FS

// Templates 模板文件系统（web 包内嵌，供 Iris 视图引擎使用）。
func Templates() fs.FS {
	sub, err := fs.Sub(assets, "templates")
	if err != nil {
		panic(err)
	}
	return sub
}

// Static 静态资源文件系统。
func Static() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
