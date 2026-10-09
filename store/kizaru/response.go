package kizaru

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/MunifTanjim/stremthru/core"
	"github.com/MunifTanjim/stremthru/internal/request"
)

// errorResponse is Kizaru's failure envelope: {"error": "..."}.
type errorResponse struct {
	Error string `json:"error"`
}

// ResponseError is an error Kizaru answered with.
//
// Kizaru carries no machine-readable error code — its HTTP status is the signal —
// so Status is what TranslateErrorCode keys off.
type ResponseError struct {
	Err    string `json:"error,omitempty"`
	Status int    `json:"status_code,omitempty"`
}

func (e *ResponseError) Error() string {
	ret, _ := json.Marshal(e)
	return string(ret)
}

// Response is the envelope passed to request.ProcessResponseBody.
//
// Unlike the providers that wrap payloads in a success/data object, Kizaru
// returns the payload bare and signals failure purely by status code, so the
// only branch that matters here is "did this fail, and what did it say".
type Response[T any] struct {
	Data T
	err  *ResponseError
}

func (r *Response[T]) Unmarshal(res *http.Response, body []byte, v any) error {
	if res.StatusCode >= http.StatusBadRequest {
		parsed := errorResponse{}
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
			r.err = &ResponseError{Err: parsed.Error, Status: res.StatusCode}
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

	return core.UnmarshalJSON(res.StatusCode, body, &r.Data)
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
