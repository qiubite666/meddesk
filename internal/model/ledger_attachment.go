package model

import "time"

// LedgerAttachment 是账单的凭证附件（小票、发票照片等）。
//
// 【为什么凭证是明文图片】
// 记账本整体放弃端到端加密（见 docs/CONTRACTS-LEDGER.md 第 0 节），凭证也跟着明文落盘：
// 服务端要做每日自动备份与 HTML/ZIP 导出，密文状态下无法生成可读的报告。
// 代价是凭证图片能被服务端读到，健康数据不受影响（仍为 E2EE）。
//
// 【为什么只存 rel_path】
// 磁盘上的绝对路径依赖部署环境（上传根目录可配置），写进库会让迁移/换目录失效；
// 库里只存"相对上传根目录"的路径，且该路径**一律由服务端生成**，用户输入不参与。
type LedgerAttachment struct {
	ID       string `json:"id"`
	UserID   string `json:"-"` // 归属不对外输出，避免与会话账号混淆
	RecordID string `json:"recordId"`
	RelPath  string `json:"-"` // 相对上传根目录，仅服务端可见（对外暴露会给出存储布局线索）
	Mime     string `json:"mime"`
	Bytes    int64  `json:"bytes"`
	// Width/Height 由服务端解码得到，用于导出排版；解码失败时为 0（不影响业务）。
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	CreatedAt time.Time `json:"createdAt"`
}

// 凭证允许的 MIME 与对应扩展名（不含点）。
const (
	MimeJPEG = "image/jpeg"
	MimePNG  = "image/png"
	MimeWebP = "image/webp"

	ExtJPEG = "jpg"
	ExtPNG  = "png"
	ExtWebP = "webp"
)
