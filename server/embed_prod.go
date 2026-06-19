//go:build embed_web

package main

import "embed"

// 启用 embed_web 构建标签时，把前端构建产物（构建前由 CI/Docker
// 将 web/dist 拷到 server/dist）内嵌进二进制，实现自包含单二进制。
//
//go:embed all:dist
var embeddedWeb embed.FS

var hasEmbeddedWeb = true
