// Package web 内嵌管理面板静态资源。
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html
var assets embed.FS

// Index 返回面板 HTML。
func Index() []byte {
	b, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return []byte("<!doctype html><meta charset=utf-8><h1>panel missing</h1>")
	}
	return b
}
