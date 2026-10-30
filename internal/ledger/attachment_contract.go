package ledger

import (
	"context"
	"io"
)

// 附件相关的共享契约（由 lead 冻结，B4 实现、B6 消费，双方都不得再自行声明同名类型）。
//
// 为什么要单独放一个文件：B4（附件）与 B6（导出）都要用到它，但它又不能被两方
// 各自声明（Go 里重复声明会编译失败），因此由主 agent 独占此文件。

// AttachmentRef 是导出/备份时需要的附件元信息。
type AttachmentRef struct {
	ID       string `json:"id"`
	RecordID string `json:"recordId"`
	Mime     string `json:"mime"`
	Ext      string `json:"ext"` // 不含点，例如 "jpg"
	Bytes    int64  `json:"bytes"`
}

// AttachmentSource 是导出模块读取附件字节的入口。
//
// 账号隔离由实现方保证：传错 userID 一律返回 not found。
type AttachmentSource interface {
	// Open 打开附件内容；调用方负责 Close。
	Open(ctx context.Context, userID, attachmentID string) (io.ReadCloser, error)
	// ListByRecord 列出某条账单下的全部附件。
	ListByRecord(ctx context.Context, userID, recordID string) ([]AttachmentRef, error)
}
