package core

import (
	"errors"
	"fmt"
)

// Kind 是错误分类，决定 HTTP 状态码。中间件只需读 Kind，不必解析字符串。
type Kind int

const (
	KindInternal    Kind = iota // 500 服务端内部错误
	KindBadInput                // 400 输入不合法
	KindUnsupport               // 400 不支持的链接 / 平台
	KindNotFound                // 404 内容不存在或已删除
	KindUpstream                // 502 上游平台异常
	KindRateLimit               // 429 被上游或本地限流
	KindForbidden               // 403 需要登录态 / Cookie 失效
	KindTimeout                 // 504 上游超时
	KindUnavailable             // 503 本地过载：并发闸门排队已满或排队超时
)

// Error 是带分类与平台信息的领域错误。
type Error struct {
	Kind     Kind
	Platform Platform
	Op       string
	Msg      string
	Err      error
}

func (e *Error) Error() string {
	var b []byte
	if e.Platform != "" {
		b = append(b, string(e.Platform)...)
	}
	if e.Op != "" {
		if len(b) > 0 {
			b = append(b, '.')
		}
		b = append(b, e.Op...)
	}
	if len(b) > 0 {
		b = append(b, ": "...)
	}
	b = append(b, e.Msg...)
	if e.Err != nil {
		b = append(b, ": "...)
		b = append(b, e.Err.Error()...)
	}
	return string(b)
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf 提取错误分类；非领域错误一律视作内部错误。
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// E 构造一个领域错误。err 可为 nil。
func E(kind Kind, platform Platform, op, msg string, err error) *Error {
	return &Error{Kind: kind, Platform: platform, Op: op, Msg: msg, Err: err}
}

// Errf 与 E 相同，但允许格式化 msg。
func Errf(kind Kind, platform Platform, op, format string, args ...any) *Error {
	return &Error{Kind: kind, Platform: platform, Op: op, Msg: fmt.Sprintf(format, args...)}
}

// 下面几个是最常用的快捷构造。

func BadInput(platform Platform, format string, args ...any) *Error {
	return Errf(KindBadInput, platform, "input", format, args...)
}

func Unsupported(platform Platform, format string, args ...any) *Error {
	return Errf(KindUnsupport, platform, "route", format, args...)
}

func NotFound(platform Platform, format string, args ...any) *Error {
	return Errf(KindNotFound, platform, "parse", format, args...)
}

func Upstream(platform Platform, op string, err error) *Error {
	return E(KindUpstream, platform, op, "上游请求失败", err)
}

func Forbidden(platform Platform, format string, args ...any) *Error {
	return Errf(KindForbidden, platform, "auth", format, args...)
}
