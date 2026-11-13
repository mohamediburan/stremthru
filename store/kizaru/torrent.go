package kizaru

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MunifTanjim/stremthru/internal/util"
	"github.com/MunifTanjim/stremthru/store"
	"github.com/google/uuid"
)

// Torrent mirrors Kizaru's torrent record.
//
// Id is the GLOBAL torrent id, and it is what every Kizaru endpoint addresses —
// including detail and removal. UserTorrentID is the caller's own library row:
// Kizaru reports it for information only, and no request is made with it. The
// store uses Id for every magnet id, never the hash.
type Torrent struct {
	Id            string `json:"id"`
	UserTorrentID string `json:"userTorrentId"`
	InfoHash      string `json:"infoHash"`
	Name          string `json:"name"`
	TotalSize     int64  `json:"totalSize"`
	Status        string `json:"status"`
	FileCount     int    `json:"fileCount"`
	CreatedAt     string `json:"createdAt"`
}

// TorrentFile mirrors Kizaru's file record.
//
// There is deliberately NO s3Key field. Kizaru stopped publishing it — it is
// the object layout of its bucket, not client state — and a filter on it is
// what silently emptied every file list: the field never unmarshals, so every
// file looks un-uploaded. Playability is a TORRENT-wide property (a `cached`
// torrent has every file uploaded), so it is read from the torrent status,
// never inferred per file.
type TorrentFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type TorrentDetailData struct {
	Torrent Torrent       `json:"torrent"`
	Files   []TorrentFile `json:"files"`
}

// magnetStatus maps Kizaru's torrent lifecycle onto StremThru's magnet statuses.
//
// Kizaru: meta_pending | downloading | stalled | uploading | uploaded | seeding
// | cached | error. Only the terminal-ready set maps to "cached", because Comet
// treats anything else as "not cached yet" and refuses to build a stream.
func magnetStatus(kizaruStatus string) store.MagnetStatus {
	switch kizaruStatus {
	case "cached", "uploaded", "seeding":
		return store.MagnetStatusCached
	case "downloading", "meta_pending", "stalled":
		return store.MagnetStatusDownloading
	case "uploading":
		return store.MagnetStatusUploading
	case "error":
		return store.MagnetStatusFailed
	default:
		return store.MagnetStatusQueued
	}
}

// FileLink is the opaque token handed to the client with each file and posted
// back verbatim to GenerateLink. Kizaru's stream endpoint takes `torrentId` and
// `fileId` together — the file id alone is not an address — so the token carries
// both: "<globalTorrentId>:<fileId>". Neither id is a secret; the API still
// authorises the request.
type FileLink string

func (l FileLink) Create(torrentID, fileID string) string {
	return torrentID + ":" + fileID
}

func (l FileLink) Parse() (torrentID, fileID string, err error) {
	parts := strings.Split(string(l), ":")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid link")
	}
	if _, err := uuid.Parse(parts[0]); err != nil {
		return "", "", fmt.Errorf("invalid link")
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		return "", "", fmt.Errorf("invalid link")
	}
	return parts[0], parts[1], nil
}

type CheckTorrentsCachedParams struct {
	Ctx
	Hashes []string
}

// CachedFile is one playable file Kizaru reports for a cached torrent.
type CachedFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type CheckTorrentsCachedDataItem struct {
	// TorrentID is the GLOBAL torrents.id. It is what a file link token needs
	// alongside the file id, and it is present whether or not the torrent is in
	// the caller's library — a cache hit is content-addressed.
	TorrentID string       `json:"torrentId"`
	Hash      string       `json:"hash"`
	Status    string       `json:"status"`
	Name      string       `json:"name"`
	TotalSize int64        `json:"totalSize"`
	Files     []CachedFile `json:"files"`
}

type CheckTorrentsCachedData struct {
	Items []CheckTorrentsCachedDataItem `json:"items"`
}

// CheckTorrentsCached asks Kizaru which of these hashes are cached, with their
// file lists.
//
// Kizaru's cache-check answers all of that in one round trip: it is
// content-addressed, so a hit comes back whether or not the torrent is in the
// caller's library, and it reports only uploaded files. Older builds returned a
// flat {"hash": bool} map, which forced a detail request per hit just to learn
// the filenames.
func (c APIClient) CheckTorrentsCached(params *CheckTorrentsCachedParams) (APIResponse[CheckTorrentsCachedData], error) {
	empty := CheckTorrentsCachedData{Items: []CheckTorrentsCachedDataItem{}}
	if len(params.Hashes) == 0 {
		return newAPIResponse(nil, empty), nil
	}

	params.JSON = map[string]any{"hashes": params.Hashes}
	response := &Response[CheckTorrentsCachedData]{}
	res, err := c.Request(http.MethodPost, "/api/v1/torrents/cache-check", params, response)
	data := response.Data
	if data.Items == nil {
		data.Items = []CheckTorrentsCachedDataItem{}
	}
	return newAPIResponse(res, data), err
}

type CreateTorrentData struct {
	Torrent Torrent `json:"torrent"`
}

type CreateTorrentParams struct {
	Ctx
	Magnet string
}

func (c APIClient) CreateTorrent(params *CreateTorrentParams) (APIResponse[CreateTorrentData], error) {
	// The add endpoint reads form data, not JSON: it is the same form the SPA
	// and a download manager post. `source` is optional and defaults to "api".
	form := url.Values{}
	form.Set("magnetUri", params.Magnet)
	params.Form = &form
	response := &Response[CreateTorrentData]{}
	res, err := c.Request(http.MethodPost, "/api/v1/torrents", params, response)
	return newAPIResponse(res, response.Data), err
}

type ListTorrentsParams struct {
	Ctx
	Limit  int
	Offset int
}

type ListTorrentsData []Torrent

// ListTorrentsPage is one page plus the paging coordinates that travel in
// `meta`. Kizaru deliberately publishes no library total, only `hasMore`.
type ListTorrentsPage struct {
	Rows    ListTorrentsData
	HasMore bool
}

func (c APIClient) ListTorrents(params *ListTorrentsParams) (APIResponse[ListTorrentsPage], error) {
	query := &url.Values{}
	query.Add("limit", strconv.Itoa(params.Limit))
	query.Add("offset", strconv.Itoa(params.Offset))
	params.Query = query

	response := &Response[ListTorrentsData]{}
	res, err := c.Request(http.MethodGet, "/api/v1/torrents", params, response)
	page := ListTorrentsPage{
		Rows:    response.Data,
		HasMore: response.Meta.HasMore,
	}
	return newAPIResponse(res, page), err
}

type GetTorrentParams struct {
	Ctx
	// Id is the global torrent id returned by the add/list/cache-check responses.
	Id string
}

func (c APIClient) GetTorrent(params *GetTorrentParams) (APIResponse[TorrentDetailData], error) {
	query := &url.Values{}
	query.Set("torrentId", params.Id)
	params.Query = query
	response := &Response[TorrentDetailData]{}
	res, err := c.Request(http.MethodGet, "/api/v1/torrents/item", params, response)
	return newAPIResponse(res, response.Data), err
}

type RemoveTorrentParams struct {
	Ctx
	Id string
}

type RemoveTorrentData struct {
	Id string
}

func (c APIClient) RemoveTorrent(params *RemoveTorrentParams) (APIResponse[RemoveTorrentData], error) {
	query := &url.Values{}
	query.Set("torrentId", params.Id)
	params.Query = query
	response := &Response[struct{}]{}
	res, err := c.Request(http.MethodDelete, "/api/v1/torrents/item", params, response)
	return newAPIResponse(res, RemoveTorrentData{Id: params.Id}), err
}

type GetStreamURLParams struct {
	Ctx
	TorrentID string
	FileID    string
}

type GetStreamURLData struct {
	URL string `json:"url"`
}

// GetStreamURL resolves a presigned playback URL.
//
// /stream answers {"url": ...} only with `Accept: application/json`; without it
// the endpoint is a 307 to the CDN and the HTTP client would follow it and
// download the media. /stream and /download are aliases of one handler on the
// Kizaru side, so either would do.
func (c APIClient) GetStreamURL(params *GetStreamURLParams) (APIResponse[GetStreamURLData], error) {
	query := &url.Values{}
	query.Set("torrentId", params.TorrentID)
	query.Set("fileId", params.FileID)
	params.Query = query
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	params.Headers = &headers

	response := &Response[GetStreamURLData]{}
	res, err := c.Request(http.MethodGet, "/api/v1/torrents/files/stream", params, response)
	return newAPIResponse(res, response.Data), err
}

// magnetFiles maps Kizaru's cache-check files onto StremThru's MagnetFile,
// minting the opaque link token that GenerateLink accepts. Kizaru has already
// filtered to uploaded files, so nothing is dropped here.
// canonicalPath converts Kizaru's torrent-relative path into the form StremThru
// stores and looks up.
//
// StremThru keys media info by path — torrent_stream.GetMediaInfo(hash, path)
// runs `WHERE hash = ? AND path = ?` — and it only ever records paths that begin
// with "/" (see torrent_stream.Record, which skips everything else). Kizaru
// reports the raw torrent path ("Show.S01/e01.mkv"), so it must go through the
// same RemoveRootFolderFromPath that every other store's GetPath() uses.
// Without this the lookup silently never matches and streams lose their media
// info; nothing errors, the enrichment is just always empty.
func canonicalPath(path string) string {
	cleaned, _ := util.RemoveRootFolderFromPath(path)
	return cleaned
}

func magnetFiles(torrentID string, files []CachedFile) []store.MagnetFile {
	out := make([]store.MagnetFile, 0, len(files))
	for i, f := range files {
		out = append(out, store.MagnetFile{
			Idx:  i,
			Link: FileLink("").Create(torrentID, f.ID),
			Path: canonicalPath(f.Path),
			Name: f.Name,
			Size: f.Size,
		})
	}
	return out
}

// playableFiles returns the files a caller may mint a link for.
//
// Readiness is per TORRENT, never per file: Kizaru marks a torrent `cached`
// only once every file in it has been uploaded, and a subset is not something
// the API can transfer. The per-file s3Key this used to test is no longer
// published, which made the old filter drop every file; the status is the
// signal now, and it is the same one the engine gates /stream on.
func playableFiles(torrentID, status string, files []TorrentFile) []store.MagnetFile {
	out := []store.MagnetFile{}
	if magnetStatus(status) != store.MagnetStatusCached {
		return out
	}
	for i, f := range files {
		name := f.Path
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		out = append(out, store.MagnetFile{
			Idx:  i,
			Link: FileLink("").Create(torrentID, f.ID),
			Path: canonicalPath(f.Path),
			Name: name,
			Size: f.Size,
		})
	}
	return out
}

func torrentAddedAt(t Torrent) time.Time {
	if ts, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil {
		return ts
	}
	return time.Time{}
}
