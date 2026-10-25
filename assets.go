// Package meddesk 暴露内嵌的前端静态资源（web/ 目录）。
// 若设置 MEDDESK_WEB_DIR，服务端会改为从磁盘读取，便于前端联调。
package meddesk

import "embed"

//go:embed all:web
var WebFS embed.FS
