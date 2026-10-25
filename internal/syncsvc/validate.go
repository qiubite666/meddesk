package syncsvc

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"time"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

const (
	// maxRecordIDLen 是记录 id 的最大长度（UUID 或客户端自定义短 id）。
	maxRecordIDLen = 64
	// allowedClockSkew 容忍的客户端时钟前移上限。
	allowedClockSkew = 5 * time.Minute
	// resetConfirmWord 是清空云端同步数据时必须提交的确认词。
	resetConfirmWord = "DELETE-ALL"
)

// recordIDPattern 允许的 id 字符集：字母、数字、下划线、连字符、点、冒号。
var recordIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// itemError 描述一条校验失败的变更项，用于错误响应的 details 字段。
type itemError struct {
	Index  int    `json:"index"`           // 在 changes 中的下标；-1 表示批次级错误
	Table  string `json:"table,omitempty"` // 出错项的表名（可为空）
	ID     string `json:"id,omitempty"`    // 出错项的记录 id（可为空）
	Field  string `json:"field"`           // 出错字段
	Reason string `json:"reason"`          // 失败原因（中文，可直接展示）
}

// validationDetails 是校验失败 details 的统一结构。
type validationDetails struct {
	Items []itemError `json:"items"`
}

// validatePush 对整批入参做校验。服务端只看密文信封的元数据，绝不解析业务字段。
// 全部通过返回 nil；否则返回 *apierr.Error（CodeBadRequest），details 中给出条目索引与原因。
func validatePush(cfg config.SyncConfig, now time.Time, changes []model.SyncItem) error {
	var errs []itemError
	add := func(i int, it *model.SyncItem, field, reason string) {
		e := itemError{Index: i, Field: field, Reason: reason}
		if it != nil {
			e.Table, e.ID = it.Table, it.ID
		}
		errs = append(errs, e)
	}

	if cfg.MaxItemsPerPush > 0 && len(changes) > cfg.MaxItemsPerPush {
		return apierr.Newf(apierr.CodeBadRequest, "单次同步条数超过上限 %d", cfg.MaxItemsPerPush).
			WithDetails(validationDetails{Items: []itemError{{
				Index:  -1,
				Field:  "changes",
				Reason: fmt.Sprintf("本次 %d 条，上限 %d 条", len(changes), cfg.MaxItemsPerPush),
			}}})
	}

	seen := make(map[string]int, len(changes))
	deadline := now.Add(allowedClockSkew).UnixMilli()
	for i := range changes {
		it := &changes[i]

		if !model.IsKnownTable(it.Table) {
			add(i, it, "table", "不在允许同步的表名白名单内")
		}
		switch {
		case it.ID == "":
			add(i, it, "id", "不能为空")
		case len(it.ID) > maxRecordIDLen:
			add(i, it, "id", fmt.Sprintf("长度超过 %d 字符", maxRecordIDLen))
		case !recordIDPattern.MatchString(it.ID):
			add(i, it, "id", "只允许字母、数字与 . _ : -")
		}

		if it.Ver < 1 {
			add(i, it, "ver", "必须大于等于 1")
		}
		switch {
		case it.UpdatedAt <= 0:
			add(i, it, "updatedAt", "必须为正的 Unix 毫秒时间戳")
		case it.UpdatedAt > deadline:
			add(i, it, "updatedAt", "超过服务端当前时间 5 分钟以上")
		}

		if it.IV == "" {
			add(i, it, "iv", "不能为空")
		} else if _, ok := decodeBase64(it.IV); !ok {
			add(i, it, "iv", "不是合法的 base64 字符串")
		}

		switch {
		case it.CT == "":
			// 删除墓碑允许空密文，但 iv 仍必填（上面已校验）。
			if !it.Deleted {
				add(i, it, "ct", "不能为空（仅删除墓碑可为空）")
			}
		default:
			n, ok := decodeBase64(it.CT)
			if !ok {
				add(i, it, "ct", "不是合法的 base64 字符串")
			} else if cfg.MaxRecordBytes > 0 && int64(n) > cfg.MaxRecordBytes {
				add(i, it, "ct", fmt.Sprintf("解码后 %d 字节，超过单条上限 %d 字节", n, cfg.MaxRecordBytes))
			}
		}

		key := it.Table + "\x00" + it.ID
		if prev, dup := seen[key]; dup {
			add(i, it, "id", fmt.Sprintf("与第 %d 条重复的 (table,id)", prev))
		} else {
			seen[key] = i
		}
	}

	if len(errs) > 0 {
		return apierr.New(apierr.CodeBadRequest, "同步数据校验失败").WithDetails(validationDetails{Items: errs})
	}
	return nil
}

// decodeBase64 解析 base64（兼容带填充与不带填充两种写法），返回解码后的字节数与是否合法。
func decodeBase64(s string) (int, bool) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return len(b), true
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return len(b), true
	}
	return 0, false
}
