package kizaru

import (
	"net/http"
	"net/url"

	"github.com/MunifTanjim/stremthru/core"
	"github.com/MunifTanjim/stremthru/internal/config"
	"github.com/MunifTanjim/stremthru/internal/request"
	"github.com/MunifTanjim/stremthru/store"
)

var DefaultHTTPClient = config.DefaultHTTPClient

type APIClientConfig struct {
	BaseURL    string // default: https://dev.kizaru.app
	APIKey     string
	HTTPClient *http.Client
	UserAgent  string
}

type APIClient struct {
	BaseURL    *url.URL
	HTTPClient *http.Client
	apiKey     string
	agent      string
	reqQuery   func(query *url.Values, params request.Context)
	reqHeader  func(header *http.Header, params request.Context)
}

func NewAPIClient(conf *APIClientConfig) *APIClient {
	if conf.UserAgent == "" {
		conf.UserAgent = "stremthru"
	}

	if conf.BaseURL == "" {
		conf.BaseURL = "https://dev.kizaru.app"
	}

	if conf.HTTPClient == nil {
		conf.HTTPClient = DefaultHTTPClient
	}

	c := &APIClient{}

	baseUrl, err := url.Parse(conf.BaseURL)
	if err != nil {
		panic(err)
	}

	c.BaseURL = baseUrl
	c.HTTPClient = conf.HTTPClient
	c.apiKey = conf.APIKey
	c.agent = conf.UserAgent

	c.reqQuery = func(query *url.Values, params request.Context) {
	}

	// Kizaru authenticates with a Better Auth API key in X-API-Key. The key
	// arrives as StremThru's StoreAuthToken, i.e. the per-user credential the
	// client presented, so every call is scoped to that user. There is no
	// fallback: an unauthenticated request must fail, not borrow someone
	// else's identity.
	c.reqHeader = func(header *http.Header, params request.Context) {
		header.Add("X-API-Key", params.GetAPIKey(c.apiKey))
		header.Add("User-Agent", c.agent)
	}

	return c
}

type Ctx = request.Ctx

func (c APIClient) Request(method, path string, params request.Context, v request.ResponseContainer) (*http.Response, error) {
	if params == nil {
		params = &Ctx{}
	}
	req, err := params.NewRequest(c.BaseURL, method, path, c.reqHeader, c.reqQuery)
	if err != nil {
		error := core.NewStoreError("failed to create request")
		error.StoreName = string(store.StoreNameKizaru)
		error.Cause = err
		return nil, error
	}
	res, err := c.HTTPClient.Do(req)
	err = request.ProcessResponseBody(res, err, v)
	if err != nil {
		err := UpstreamErrorWithCause(err)
		err.InjectReq(req)
		if res != nil && res.StatusCode >= http.StatusBadRequest {
			err.StatusCode = res.StatusCode
			if err.StatusCode == http.StatusTooManyRequests {
				err.Code = core.ErrorCodeTooManyRequests
				err.RetryAfter = res.Header.Get("Retry-After")
			}
		}
		return res, err
	}
	return res, nil
}
