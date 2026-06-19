//go:build !embed_web

package main

import "embed"

// 默认构建（无 embed_web 标签）：不内嵌前端，运行时从 --web 目录读取。
// 此兜底文件保证缺少 server/dist 时 `go build ./...` 仍可编译。
var embeddedWeb embed.FS

var hasEmbeddedWeb = false
