package dashboard

import (
	"embed"
	"io/fs"
)

// webdist 是 web/dist 的构建产物副本。
// 构建流程：cd web && npm run build && rm -rf ../dashboard/webdist && cp -r dist ../dashboard/webdist
//
//go:embed all:webdist
var embeddedFS embed.FS

var staticFS fs.FS

func init() {
	if sub, err := fs.Sub(embeddedFS, "webdist"); err == nil {
		staticFS = sub
	}
}
