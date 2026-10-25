package store

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// uuidUniqueViolation 是 PostgreSQL 唯一约束冲突的 SQLSTATE。
const pgUniqueViolation = "23505"

// newUUID 生成一个新的 UUIDv4 字符串。
func newUUID() string { return uuid.NewString() }

// uuidParam 把字符串形式的 ID 转成 pgx 可编码的 uuid 参数。
// 说明：pgx 无法把 Go 的 string 直接编码成 uuid（该类型偏好二进制格式），
// 必须显式转换；传入非法 UUID 时返回 apierr.CodeBadRequest。
func uuidParam(s string) (pgtype.UUID, error) {
	var v pgtype.UUID
	if err := v.Scan(s); err != nil {
		return v, apierr.Newf(apierr.CodeBadRequest, "非法 ID %q：不是合法 UUID", s)
	}
	return v, nil
}

// uuidPtrParam 把可选的（可能为 nil 或空）字符串 ID 转成 uuid 参数，空值表示 NULL。
func uuidPtrParam(s *string) (pgtype.UUID, error) {
	if s == nil || *s == "" {
		return pgtype.UUID{}, nil
	}
	return uuidParam(*s)
}

// timePtrParam 把可选的时间转成 timestamptz 参数，nil 表示 NULL。
func timePtrParam(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// notFound 把 pgx.ErrNoRows 归一化为仓储层的 ErrNotFound，其他错误原样返回。
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// isUniqueViolation 判断错误是否为唯一约束冲突（如邮箱重复）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// decodeBase64 解码密文字段。同步信封的 iv/ct 在传输层是标准 base64，
// 落库为 bytea；非法值属于请求体错误。
func decodeBase64(field, value string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, apierr.Newf(apierr.CodeBadRequest, "%s 不是合法的 base64 字符串", field)
	}
	return b, nil
}

// encodeBase64 把库中的密文字节编码为传输层使用的标准 base64。
func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// conflictError 是「资源冲突」错误（HTTP 409 语义）。
// 它同时满足 apierr.Is(err, apierr.CodeConflict) 与 errors.Is(err, ErrConflict)，
// 便于 HTTP 层直接序列化，也便于业务层用哨兵错误判断。
type conflictError struct {
	apiErr *apierr.Error
}

// Error 实现 error 接口。
func (e *conflictError) Error() string { return e.apiErr.Error() }

// Is 使 errors.Is(err, ErrConflict) 成立。
func (e *conflictError) Is(target error) bool { return target == ErrConflict }

// Unwrap 暴露内层的 *apierr.Error，使 errors.As 能取到错误码。
func (e *conflictError) Unwrap() error { return e.apiErr }

// newConflict 构造冲突错误（如邮箱已被注册）。
func newConflict(msg string) error {
	return &conflictError{apiErr: apierr.New(apierr.CodeConflict, msg)}
}

// wrap 统一包装底层错误，保留 %w 以便 errors.Is/As 透传。
func wrap(msg string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", msg, err)
}
