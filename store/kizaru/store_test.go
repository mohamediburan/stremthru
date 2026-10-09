package kizaru

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MunifTanjim/stremthru/core"
	"github.com/MunifTanjim/stremthru/store"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*StoreClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewStoreClient(&StoreClientConfig{BaseURL: srv.URL}), srv
}

// The API key must reach Kizaru as X-API-Key. StremThru passes it as the
// per-user StoreAuthToken, so if this header is dropped every call is anonymous
// and Kizaru answers 401.
func TestClientSendsAPIKeyHeader(t *testing.T) {
	var gotKey string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u1","userId":"u1","plan":"pro","status":"active"}`))
	})

	params := &store.GetUserParams{}
	params.APIKey = "kz_secret_key"

	user, err := c.GetUser(params)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if gotKey != "kz_secret_key" {
		t.Errorf("X-API-Key = %q, want the caller's key", gotKey)
	}
	if user.SubscriptionStatus != store.UserSubscriptionStatusPremium {
		t.Errorf("subscription_status = %q, want premium", user.SubscriptionStatus)
	}
}

// With no caller key there is no fallback: the request must carry no credential
// rather than borrow a baked-in one.
func TestNoAPIKeyFallback(t *testing.T) {
	var gotKey string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u1","userId":"u1","status":"active"}`))
	})

	if _, err := c.GetUser(&store.GetUserParams{}); err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	// The header is sent unconditionally, matching torbox/debrider/alldebrid. What
	// matters is that its VALUE is empty: no key may be baked in as a fallback.
	if gotKey != "" {
		t.Errorf("X-API-Key = %q, want empty — a fallback key must never be baked in", gotKey)
	}
}

// A user with no pass gets a null body, not an error. Reporting that as premium
// would hand out paid bandwidth for free.
func TestGetUserWithoutPassIsExpired(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`null`))
	})

	user, err := c.GetUser(&store.GetUserParams{})
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.SubscriptionStatus != store.UserSubscriptionStatusExpired {
		t.Errorf("subscription_status = %q, want expired", user.SubscriptionStatus)
	}
}

// A lapsed pass must not be reported as premium.
func TestGetUserLapsedPassIsExpired(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u1","userId":"u1","plan":"pro","status":"expired"}`))
	})

	user, err := c.GetUser(&store.GetUserParams{})
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.SubscriptionStatus != store.UserSubscriptionStatusExpired {
		t.Errorf("subscription_status = %q, want expired", user.SubscriptionStatus)
	}
}

// CheckMagnet must map Kizaru's cache-check items onto StremThru's shape in one
// call. Kizaru does the user-scoping and the uploaded-file filtering server side,
// so anything it returns is already playable and must be passed through intact.
func TestCheckMagnetMapsDetailedResponse(t *testing.T) {
	const cachedHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const missHash = "cccccccccccccccccccccccccccccccccccccccc"

	var calls int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/api/v1/torrents/cache-check" {
			t.Errorf("path = %q, want the cache-check endpoint", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{
			"hash":"` + cachedHash + `",
			"status":"cached",
			"name":"Some.Movie.2024.1080p",
			"totalSize":100,
			"files":[{"fileIndex":0,"path":"Some.Movie.2024.1080p/movie.mkv","name":"movie.mkv","size":90}]
		}]}`))
	})

	params := &store.CheckMagnetParams{}
	params.Magnets = []string{cachedHash, missHash}

	data, err := c.CheckMagnet(params)
	if err != nil {
		t.Fatalf("CheckMagnet: %v", err)
	}
	// One request, not one per cached hash — that is the whole point.
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("made %d requests, want 1 (no per-hash detail calls)", n)
	}
	if len(data.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(data.Items))
	}

	item := data.Items[0]
	if item.Hash != cachedHash {
		t.Errorf("hash = %q", item.Hash)
	}
	if item.Status != store.MagnetStatusCached {
		t.Errorf("status = %q, want cached", item.Status)
	}
	if item.Name != "Some.Movie.2024.1080p" {
		t.Errorf("name = %q", item.Name)
	}
	if len(item.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(item.Files))
	}
	if item.Files[0].Name != "movie.mkv" {
		t.Errorf("file name = %q", item.Files[0].Name)
	}
	// Path must be StremThru's canonical form, not Kizaru's raw torrent path:
	// root folder stripped and a leading "/" (see canonicalPath).
	if item.Files[0].Path != "/movie.mkv" {
		t.Errorf("file path = %q, want %q", item.Files[0].Path, "/movie.mkv")
	}
	if item.Files[0].Size != 90 {
		t.Errorf("file size = %d", item.Files[0].Size)
	}
	// The link token is handed back verbatim to GenerateLink.
	if want := FileLink("").Create(cachedHash, 0); item.Files[0].Link != want {
		t.Errorf("file link = %q, want %q", item.Files[0].Link, want)
	}
}

// GenerateLink must hit /stream and read the URL from its JSON body.
//
// /stream used to answer 302 with the URL in Location, which meant parsing a
// redirect body and blew up in Response.Unmarshal. It now returns {"url": ...}
// like every other store's GenerateLink.
func TestGenerateLinkUsesStreamEndpoint(t *testing.T) {
	const signed = "https://cdn.example.com/presigned?sig=abc"
	const hash = "dddddddddddddddddddddddddddddddddddddddd"

	var gotPath string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"` + signed + `"}`))
	})

	res, err := c.GenerateLink(&store.GenerateLinkParams{Link: FileLink("").Create(hash, 7)})
	if err != nil {
		t.Fatalf("GenerateLink: %v", err)
	}
	if res.Link != signed {
		t.Errorf("link = %q, want the presigned url from the JSON body", res.Link)
	}
	if want := "/api/v1/torrents/" + hash + "/files/7/stream"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestGenerateLinkRejectsMalformedToken(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made for a malformed token")
	})

	for _, bad := range []string{"", "no-colon", "hash:notanumber", "hash:-1"} {
		if _, err := c.GenerateLink(&store.GenerateLinkParams{Link: bad}); err == nil {
			t.Errorf("GenerateLink(%q) accepted a malformed token", bad)
		}
	}
}

// A 404 on the library detail must surface as core.ErrorCodeNotFound so callers
// (and CheckMagnet's skip logic) can branch on it.
func TestUpstreamErrorTranslatesStatus(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Torrent not found"}`))
	})

	_, err := c.GetMagnet(&store.GetMagnetParams{Id: "deadbeef"})
	if err == nil {
		t.Fatal("expected an error")
	}
	upstream, ok := err.(*core.UpstreamError)
	if !ok {
		t.Fatalf("error type = %T, want *core.UpstreamError", err)
	}
	if upstream.Code != core.ErrorCodeNotFound {
		t.Errorf("code = %q, want NOT_FOUND", upstream.Code)
	}
	if upstream.StoreName != string(store.StoreNameKizaru) {
		t.Errorf("store name = %q", upstream.StoreName)
	}
	if upstream.Msg != "Torrent not found" {
		t.Errorf("msg = %q, want Kizaru's message", upstream.Msg)
	}
	if upstream.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", upstream.StatusCode)
	}
}

// A 429 must be retryable and carry Retry-After through.
func TestRateLimitPreservesRetryAfter(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	})

	_, err := c.GetUser(&store.GetUserParams{})
	if err == nil {
		t.Fatal("expected an error")
	}
	upstream, ok := err.(*core.UpstreamError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if upstream.Code != core.ErrorCodeTooManyRequests {
		t.Errorf("code = %q, want TOO_MANY_REQUESTS", upstream.Code)
	}
	if upstream.RetryAfter != "42" {
		t.Errorf("RetryAfter = %q, want 42", upstream.RetryAfter)
	}
}

// canonicalPath must produce the form StremThru stores and looks up.
//
// torrent_stream.Record skips any path without a leading "/", and GetMediaInfo
// keys on the exact string — so a raw torrent path silently never matches and
// the stream loses its media info.
func TestCanonicalPathMatchesStremThruConvention(t *testing.T) {
	cases := map[string]string{
		"Show.S01.1080p/e01.mkv":    "/e01.mkv",
		"/Show.S01.1080p/e01.mkv":   "/e01.mkv",
		"Show.S01/sub/e01.mkv":      "/sub/e01.mkv",
		"Some.Movie.2024/movie.mkv": "/movie.mkv",
	}
	for in, want := range cases {
		if got := canonicalPath(in); got != want {
			t.Errorf("canonicalPath(%q) = %q, want %q", in, got, want)
		}
	}

	// Every non-empty result must satisfy the Record() precondition.
	for _, in := range []string{"Show.S01/e01.mkv", "/Show.S01/e01.mkv", "Show.S01/sub/e01.mkv"} {
		if got := canonicalPath(in); !strings.HasPrefix(got, "/") {
			t.Errorf("canonicalPath(%q) = %q, which torrent_stream.Record would discard", in, got)
		}
	}
}

func TestMagnetStatusMapping(t *testing.T) {
	cases := map[string]store.MagnetStatus{
		"cached":       store.MagnetStatusCached,
		"uploaded":     store.MagnetStatusCached,
		"seeding":      store.MagnetStatusCached,
		"downloading":  store.MagnetStatusDownloading,
		"meta_pending": store.MagnetStatusDownloading,
		"stalled":      store.MagnetStatusDownloading,
		"uploading":    store.MagnetStatusUploading,
		"error":        store.MagnetStatusFailed,
		"":             store.MagnetStatusQueued,
		"mystery":      store.MagnetStatusQueued,
	}
	for in, want := range cases {
		if got := magnetStatus(in); got != want {
			t.Errorf("magnetStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// The add response must round-trip through the shape Comet reads: status plus a
// file list whose links are the opaque tokens GenerateLink accepts.
func TestAddMagnetReturnsPlayableFiles(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"message":"Torrent registered successfully","torrent":{"id":"t1","infoHash":"abc","name":"Show.S01","totalSize":50,"status":"cached"}}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"torrent":{"id":"t1","infoHash":"abc","name":"Show.S01","totalSize":50,"status":"cached"},"files":[{"fileIndex":2,"path":"Show.S01/e01.mkv","size":50,"s3Key":"k"}]}`))
		}
	})

	data, err := c.AddMagnet(&store.AddMagnetParams{Magnet: "magnet:?xt=urn:btih:abc"})
	if err != nil {
		t.Fatalf("AddMagnet: %v", err)
	}
	if data.Hash != "abc" || data.Status != store.MagnetStatusCached {
		t.Errorf("hash/status = %q/%q", data.Hash, data.Status)
	}
	if len(data.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(data.Files))
	}
	if data.Files[0].Link != FileLink("").Create("abc", 2) {
		t.Errorf("file link = %q, want the abc:2 token", data.Files[0].Link)
	}
	if data.Magnet == "" {
		t.Error("magnet must be echoed back for the client to keep")
	}
}

func TestFileLinkRoundTrip(t *testing.T) {
	for _, index := range []int{0, 1, 42, 999} {
		token := FileLink("").Create("deadbeef", index)
		gotHash, gotIndex, err := FileLink(token).Parse()
		if err != nil {
			t.Fatalf("Parse(%q): %v", token, err)
		}
		if gotHash != "deadbeef" || gotIndex != index {
			t.Errorf("round trip %q -> %q/%d", token, gotHash, gotIndex)
		}
	}
}

func TestResponseErrorShape(t *testing.T) {
	e := &ResponseError{Err: "boom", Status: http.StatusConflict}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"error":"boom"`) {
		t.Errorf("ResponseError JSON = %s", b)
	}
}
