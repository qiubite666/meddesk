// Package apierr 定义统一的 API 错误码与响应结构。
// 约定：所有 HTTP 错误响应体均为 {"code": "...", "message": "...", "details": ...}
package apierr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 是稳定的机器可读错误码。
type Code string

const (
	CodeBadRequest       Code = "bad_request"
	CodeUnauthorized     Code = "unauthorized"
	CodeForbidden        Code = "forbidden"
	CodeNotFound         Code = "not_found"
	CodeConflict         Code = "conflict"
	CodePayloadTooLarge  Code = "payload_too_large"
	CodeRateLimited      Code = "rate_limited"
	CodeUnavailable      Code = "unavailable"
	CodeNotImplemented   Code = "not_implemented"
	CodeInternal         Code = "internal"
)

// Error 实现 error 接口，同时可直接作为 JSON 响应体序列化。
type Error struct {
	Code    Code        `json:"code"`
	Message string      `json:"message"`
	Details interface{} `json:"details,omitempty"`
}

// Error 返回可读错误信息（便于日志）。
func (e *Error) Error() string {
	if e.Details != nil {
		return string(e.Code) + ": " + e.Message
	}
	return string(e.Code) + ": " + e.Message
}

// New 构造错误。
func New(c Code, msg string) *Error { return &Error{Code: c, Message: msg} }

// Newf 构造带格式化信息的错误。
func Newf(c Code, format string, args ...interface{}) *Error {
	return &Error{Code: c, Message: fmt.Sprintf(format, args...)}
}

// WithDetails 附加结构化详情。
func (e *Error) WithDetails(d interface{}) *Error {
	e.Details = d
	return e
}

// Wrap 包装底层错误：保留原错误用于日志，对外只暴露 code 与 message。
func Wrap(c Code, err error, msg string) *Error {
	if err == nil {
		return New(c, msg)
	}
	return &Error{Code: c, Message: msg, Details: nil}
}

// HTTPStatus 映射错误码到 HTTP 状态码。
func HTTPStatus(c Code) int {
	switch c {
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// Is 判断 error 是否属于给定 Code。
func Is(err error, c Code) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == c
	}
	return false
}

// FromError 将任意 error 转为 *Error（未知错误归为 internal）。
func FromError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return New(CodeInternal, "服务器内部错误")
}
