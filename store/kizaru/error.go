package kizaru

import (
	"net/http"

	"github.com/MunifTanjim/stremthru/core"
	"github.com/MunifTanjim/stremthru/store"
)

// ErrorCode names the failure classes Kizaru can produce.
//
// Kizaru's API returns a bare message with an HTTP status and no vendor error
// code, so this vocabulary is derived from the status rather than parsed from
// the body — the opposite of a provider like Real-Debrid, where the status is
// coarse and the body carries the real code.
type ErrorCode string

const (
	ErrorCodeBadRequest   ErrorCode = "BAD_REQUEST"
	ErrorCodeUnauthorized ErrorCode = "UNAUTHORIZED"
	ErrorCodeForbidden    ErrorCode = "FORBIDDEN"
	ErrorCodeNotFound     ErrorCode = "NOT_FOUND"
	ErrorCodeConflict     ErrorCode = "CONFLICT"
	ErrorCodeRateLimited  ErrorCode = "TOO_MANY_REQUESTS"
	ErrorCodeInternal     ErrorCode = "INTERNAL_SERVER_ERROR"
	ErrorCodeBadGateway   ErrorCode = "BAD_GATEWAY"
	ErrorCodeUnavailable  ErrorCode = "SERVICE_UNAVAILABLE"
	ErrorCodeUnknownError ErrorCode = "UNKNOWN_ERROR"
)

var errorCodeByErrorCode = map[ErrorCode]core.ErrorCode{
	ErrorCodeBadRequest:   core.ErrorCodeBadRequest,
	ErrorCodeUnauthorized: core.ErrorCodeUnauthorized,
	ErrorCodeForbidden:    core.ErrorCodeForbidden,
	ErrorCodeNotFound:     core.ErrorCodeNotFound,
	ErrorCodeConflict:     core.ErrorCodeConflict,
	// 429 is retryable and the caller should honour Retry-After.
	ErrorCodeRateLimited:  core.ErrorCodeTooManyRequests,
	ErrorCodeInternal:     core.ErrorCodeInternalServerError,
	ErrorCodeBadGateway:   core.ErrorCodeBadGateway,
	ErrorCodeUnavailable:  core.ErrorCodeServiceUnavailable,
	ErrorCodeUnknownError: core.ErrorCodeUnknown,
}

var errorCodeByStatusCode = map[int]ErrorCode{
	http.StatusBadRequest:          ErrorCodeBadRequest,
	http.StatusUnauthorized:        ErrorCodeUnauthorized,
	http.StatusForbidden:           ErrorCodeForbidden,
	http.StatusNotFound:            ErrorCodeNotFound,
	http.StatusConflict:            ErrorCodeConflict,
	http.StatusTooManyRequests:     ErrorCodeRateLimited,
	http.StatusInternalServerError: ErrorCodeInternal,
	http.StatusBadGateway:          ErrorCodeBadGateway,
	http.StatusServiceUnavailable:  ErrorCodeUnavailable,
}

func TranslateErrorCode(errorCode ErrorCode) core.ErrorCode {
	if code, found := errorCodeByErrorCode[errorCode]; found {
		return code
	}
	return core.ErrorCodeUnknown
}

// translateStatusCode maps the status Kizaru answered with onto our vocabulary.
func translateStatusCode(statusCode int) ErrorCode {
	if code, found := errorCodeByStatusCode[statusCode]; found {
		return code
	}
	if statusCode >= http.StatusInternalServerError {
		return ErrorCodeInternal
	}
	return ErrorCodeUnknownError
}

func UpstreamErrorWithCause(cause error) *core.UpstreamError {
	err := core.NewUpstreamError("")
	err.StoreName = string(store.StoreNameKizaru)

	if rerr, ok := cause.(*ResponseError); ok {
		err.Msg = rerr.Err
		err.Code = TranslateErrorCode(translateStatusCode(rerr.Status))
		err.UpstreamCause = rerr
	} else {
		err.Cause = cause
	}

	return err
}
