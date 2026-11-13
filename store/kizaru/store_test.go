package kizaru

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		_, _ = w.Write([]byte(`{"data":{"id":"u1","userId":"u1","plan":"pro","status":"active"}}`))
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
		_, _ = w.Write([]byte(`{"data":{"id":"u1","userId":"u1","status":"active"}}`))
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
		_, _ = w.Write([]byte(`{"data":null}`))
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
		_, _ = w.Write([]byte(`{"data":{"id":"u1","userId":"u1","plan":"pro","status":"expired"}}`))
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
		_, _ = w.Write([]byte(`{"data":{"items":[{
			"torrentId":"99999999-9999-9999-9999-999999999999",
			"hash":"` + cachedHash + `",
			"status":"cached",
			"name":"Some.Movie.2024.1080p",
			"totalSize":100,
			"files":[{"id":"11111111-1111-1111-1111-111111111111","path":"Some.Movie.2024.1080p/movie.mkv","name":"movie.mkv","size":90}]
		}]}}`))
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
	// The link token is handed back verbatim to GenerateLink, and it carries the
	// GLOBAL torrent id: Kizaru addresses a file by torrentId + fileId.
	if want := "99999999-9999-9999-9999-999999999999:11111111-1111-1111-1111-111111111111"; item.Files[0].Link != want {
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
	const torrentID = "11111111-1111-1111-1111-111111111111"
	const fileID = "22222222-2222-2222-2222-222222222222"

	var gotPath, gotTorrentID, gotFileID, gotAccept string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTorrentID = r.URL.Query().Get("torrentId")
		gotFileID = r.URL.Query().Get("fileId")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"url":"` + signed + `"}}`))
	})

	res, err := c.GenerateLink(&store.GenerateLinkParams{Link: FileLink("").Create(torrentID, fileID)})
	if err != nil {
		t.Fatalf("GenerateLink: %v", err)
	}
	if res.Link != signed {
		t.Errorf("link = %q, want the presigned url from the JSON body", res.Link)
	}
	if gotPath != "/api/v1/torrents/files/stream" {
		t.Errorf("path = %q, want /api/v1/torrents/files/stream", gotPath)
	}
	if gotTorrentID != torrentID {
		t.Errorf("torrentId = %q, want %q", gotTorrentID, torrentID)
	}
	if gotFileID != fileID {
		t.Errorf("fileId = %q, want %q", gotFileID, fileID)
	}
	// Without this header the endpoint is a 307 and the HTTP client would follow
	// it into the media body instead of decoding {"url": ...}.
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
}

func TestGenerateLinkRejectsMalformedToken(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made for a malformed token")
	})

	for _, bad := range []string{
		"",
		"no-colon",
		"not-a-uuid",
		"hash:notanumber",
		// file id alone is not an address: the token needs both ids.
		"22222222-2222-2222-2222-222222222222",
		"11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222:extra",
		"22222222-2222-2222-2222-222222222222:not-a-uuid",
	} {
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
		_, _ = w.Write([]byte(`{"error":{"code":"TORRENT_NOT_FOUND","message":"Torrent not found"}}`))
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
		_, _ = w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"slow down"}}`))
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
//
// The mock deliberately omits s3Key. Kizaru stopped publishing it, and the old
// per-file filter treated every file as un-uploaded once it was gone — which is
// how the integration went from working to "No video files found" overnight.
func TestAddMagnetReturnsPlayableFiles(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			if got := r.PostForm.Get("magnetUri"); got != "magnet:?xt=urn:btih:abc" {
				t.Errorf("magnetUri = %q, want the magnet as form data", got)
			}
			if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
				t.Errorf("Content-Type = %q, want form encoding", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"data":{"message":"Torrent registered successfully","torrent":{"id":"t1","userTorrentId":"ut1","infoHash":"abc","name":"Show.S01","totalSize":50,"status":"cached"}}}`))
		case http.MethodGet:
			if r.URL.Path != "/api/v1/torrents/item" {
				t.Errorf("path = %q, want the item endpoint", r.URL.Path)
			}
			if got := r.URL.Query().Get("id"); got != "ut1" {
				t.Errorf("id = %q, want ut1", got)
			}
			_, _ = w.Write([]byte(`{"data":{"torrent":{"id":"t1","userTorrentId":"ut1","infoHash":"abc","name":"Show.S01","totalSize":50,"status":"cached"},"files":[{"id":"33333333-3333-3333-3333-333333333333","path":"Show.S01/e01.mkv","size":50}]}}`))
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
	if want := "t1:33333333-3333-3333-3333-333333333333"; data.Files[0].Link != want {
		t.Errorf("file link = %q, want %q", data.Files[0].Link, want)
	}
	if data.Magnet == "" {
		t.Error("magnet must be echoed back for the client to keep")
	}
}

// Readiness is torrent-wide, never per file. Kizaru publishes no per-file key,
// so the status is the only signal there is — and it is the right one, because
// a torrent is marked `cached` only once every file has been uploaded.
func TestPlayableFilesRequiresReadyTorrent(t *testing.T) {
	files := []TorrentFile{{ID: "44444444-4444-4444-4444-444444444444", Path: "Show.S01/e01.mkv", Size: 10}}

	for _, waiting := range []string{"meta_pending", "downloading", "uploading", "error", ""} {
		if got := playableFiles("t1", waiting, files); len(got) != 0 {
			t.Errorf("%q: got %d files, want 0", waiting, len(got))
		}
	}
	for _, ready := range []string{"cached", "uploaded", "seeding"} {
		if got := playableFiles("t1", ready, files); len(got) != 1 {
			t.Errorf("%q: got %d files, want 1", ready, len(got))
		}
	}
}

// ListMagnets must send offset/limit and use `meta.hasMore`, the shape the
// current library endpoint publishes. It deliberately publishes no total.
func TestListMagnetsUsesOffset(t *testing.T) {
	var queries []url.Values
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/torrents" {
			t.Errorf("path = %q, want the library endpoint", r.URL.Path)
		}
		queries = append(queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"t3","userTorrentId":"ut3","infoHash":"h3","name":"c","totalSize":3,"status":"cached"}
		],"meta":{"offset":2,"limit":2,"hasMore":false}}`))
	})

	data, err := c.ListMagnets(&store.ListMagnetsParams{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListMagnets: %v", err)
	}
	if len(queries) != 1 {
		t.Fatalf("made %d requests, want 1", len(queries))
	}
	if got := queries[0].Get("offset"); got != "2" {
		t.Errorf("offset = %q, want 2", got)
	}
	if got := queries[0].Get("limit"); got != "2" {
		t.Errorf("limit = %q, want 2", got)
	}
	if len(data.Items) != 1 || data.Items[0].Id != "ut3" {
		t.Errorf("items = %+v, want just ut3", data.Items)
	}
	if data.TotalItems != 3 {
		t.Errorf("total = %d, want 3 (a lower bound: offset + rows)", data.TotalItems)
	}
}

// A page the library cannot reach must be empty, not silently page one.
func TestListMagnetsPastTheEndIsEmpty(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"meta":{"offset":4,"limit":2,"hasMore":false}}`))
	})

	data, err := c.ListMagnets(&store.ListMagnetsParams{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("ListMagnets: %v", err)
	}
	if len(data.Items) != 0 {
		t.Errorf("items = %+v, want none", data.Items)
	}
	if data.TotalItems != 4 {
		t.Errorf("total = %d, want the offset lower bound 4", data.TotalItems)
	}
}

func TestFileLinkRoundTrip(t *testing.T) {
	const torrentID = "77777777-7777-7777-7777-777777777777"
	for _, fileID := range []string{
		"55555555-5555-5555-5555-555555555555",
		"66666666-6666-6666-6666-666666666666",
	} {
		token := FileLink("").Create(torrentID, fileID)
		gotTorrentID, gotFileID, err := FileLink(token).Parse()
		if err != nil {
			t.Fatalf("Parse(%q): %v", token, err)
		}
		if gotTorrentID != torrentID || gotFileID != fileID {
			t.Errorf("round trip %q -> %q:%q, want %q:%q", token, gotTorrentID, gotFileID, torrentID, fileID)
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
