package kizaru

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/MunifTanjim/stremthru/core"
	"github.com/MunifTanjim/stremthru/internal/request"
)

// errorEnvelope is Kizaru's failure body:
// {"error": {"code": "…", "message": "…"}}.
//
// `code` is stable; `message` is copy. The store's error vocabulary is still
// derived from the status — Code is carried for logs, never matched on.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ListMeta is the page metadata Kizaru sends beside the rows.
//
// `Total` is a pointer because Kizaru omits it rather than approximate it: it is
// absent when the count could not be served, and on any Kizaru too old to send
// it. Absent is not zero.
type ListMeta struct {
	Offset  int  `json:"offset"`
	Limit   int  `json:"limit"`
	HasMore bool `json:"hasMore"`
	Total   *int `json:"total"`
}

// ResponseError is an error Kizaru answered with.
//
// Status is what TranslateErrorCode keys off; Code is Kizaru's own identifier
// and is kept for logs.
type ResponseError struct {
	Err    string `json:"error,omitempty"`
	Code   string `json:"code,omitempty"`
	Status int    `json:"status_code,omitempty"`
}

func (e *ResponseError) Error() string {
	ret, _ := json.Marshal(e)
	return string(ret)
}

// Response is the envelope passed to request.ProcessResponseBody.
//
// Every Kizaru response is one of two shapes:
//
//	{"data": …, "meta": …}                 // 2xx
//	{"error": {"code": …, "message": …}}   // 4xx/5xx
//
// so the decode has exactly one branch: which of the two arrived.
type Response[T any] struct {
	Data T
	Meta ListMeta
	err  *ResponseError
}

func (r *Response[T]) Unmarshal(res *http.Response, body []byte, v any) error {
	if res.StatusCode >= http.StatusBadRequest {
		parsed := errorEnvelope{}
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
			r.err = &ResponseError{Err: parsed.Error.Message, Code: parsed.Error.Code, Status: res.StatusCode}
			return nil
		}
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = res.Status
		}
		r.err = &ResponseError{Err: msg, Status: res.StatusCode}
		return nil
	}

	// 204 and empty bodies carry no payload.
	if res.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	// Decode the envelope first: the payload belongs to the caller's type, and the
	// page numbers belong to Meta. A body that is not an envelope is handed over
	// unchanged so the decode error names the real problem.
	var envelope struct {
		Data json.RawMessage `json:"data"`
		Meta ListMeta        `json:"meta"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return core.UnmarshalJSON(res.StatusCode, body, &r.Data)
	}
	r.Meta = envelope.Meta
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	return core.UnmarshalJSON(res.StatusCode, envelope.Data, &r.Data)
}

func (r *Response[T]) GetError(res *http.Response) error {
	if r.err == nil {
		return nil
	}
	return r.err
}

type APIResponse[T any] = request.APIResponse[T]

func newAPIResponse[T any](res *http.Response, data T) APIResponse[T] {
	return request.NewAPIResponse(res, data)
}
