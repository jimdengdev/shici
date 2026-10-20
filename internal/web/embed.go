// Package web 前台：模板与静态资源内嵌，公开路由（页面 + JSON API）。
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
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

// assetETags 每个静态文件的内容哈希（ETag）与整体版本号。
// embed 文件系统的 modtime 为零值，无法用 Last-Modified 做协商缓存，改为内容哈希。
var assetETags, assetVersion = buildAssetMeta()

// AssetVersion 静态资源内容版本：模板以 ?v= 引用，配合长缓存实现「内容变则 URL 变」。
func AssetVersion() string { return assetVersion }

func buildAssetMeta() (map[string]string, string) {
	etags := make(map[string]string)
	fsys := Static()
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		etags[path] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})

	names := make([]string, 0, len(etags))
	for name := range etags {
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		h.Write([]byte(name))
		h.Write([]byte(etags[name]))
	}
	return etags, hex.EncodeToString(h.Sum(nil))[:12]
}
