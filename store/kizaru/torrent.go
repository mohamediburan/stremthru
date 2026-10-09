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
)

// Torrent mirrors Kizaru's global torrent record.
type Torrent struct {
	Id        string `json:"id"`
	InfoHash  string `json:"infoHash"`
	Name      string `json:"name"`
	TotalSize int64  `json:"totalSize"`
	Status    string `json:"status"`
	FileCount int    `json:"fileCount"`
	CreatedAt string `json:"createdAt"`
}

// TorrentFile mirrors Kizaru's file record. S3Key is only populated once the
// file has been fetched and uploaded, which is what makes it playable.
type TorrentFile struct {
	FileIndex int    `json:"fileIndex"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	S3Key     string `json:"s3Key"`
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
// back verbatim to GenerateLink. "<hash>:<index>" is all that is needed to
// rebuild the stream URL.
type FileLink string

func (l FileLink) Create(hash string, fileIndex int) string {
	return hash + ":" + strconv.Itoa(fileIndex)
}

func (l FileLink) Parse() (hash string, fileIndex int, err error) {
	hash, indexStr, ok := strings.Cut(string(l), ":")
	if !ok {
		return "", 0, fmt.Errorf("invalid link")
	}
	fileIndex, err = strconv.Atoi(indexStr)
	if err != nil || fileIndex < 0 {
		return "", 0, fmt.Errorf("invalid link")
	}
	return hash, fileIndex, nil
}

type CheckTorrentsCachedParams struct {
	Ctx
	Hashes []string
}

// CachedFile is one playable file Kizaru reports for a cached torrent.
type CachedFile struct {
	FileIndex int    `json:"fileIndex"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
}

type CheckTorrentsCachedDataItem struct {
	Hash      string       `json:"hash"`
	Status    string       `json:"status"`
	Name      string       `json:"name"`
	TotalSize int64        `json:"totalSize"`
	Files     []CachedFile `json:"files"`
}

type CheckTorrentsCachedData struct {
	Items []CheckTorrentsCachedDataItem `json:"items"`
}

// CheckTorrentsCached asks Kizaru which of these hashes are cached AND playable
// by the caller, with their file lists.
//
// Kizaru's cache-check answers all of that in one round trip: it already scopes
// to the caller's library and reports only uploaded files. Older builds returned
// a flat {"hash": bool} map, which forced a detail request per hit just to learn
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
	params.JSON = map[string]any{"magnetUri": params.Magnet}
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

func (c APIClient) ListTorrents(params *ListTorrentsParams) (APIResponse[ListTorrentsData], error) {
	query := &url.Values{}
	query.Add("limit", strconv.Itoa(params.Limit))
	query.Add("offset", strconv.Itoa(params.Offset))
	params.Query = query

	response := &Response[ListTorrentsData]{}
	res, err := c.Request(http.MethodGet, "/api/v1/torrents", params, response)
	return newAPIResponse(res, response.Data), err
}

type GetTorrentParams struct {
	Ctx
	Hash string
}

func (c APIClient) GetTorrent(params *GetTorrentParams) (APIResponse[TorrentDetailData], error) {
	response := &Response[TorrentDetailData]{}
	res, err := c.Request(http.MethodGet, "/api/v1/torrents/"+url.PathEscape(params.Hash), params, response)
	return newAPIResponse(res, response.Data), err
}

type RemoveTorrentParams struct {
	Ctx
	Hash string
}

type RemoveTorrentData struct {
	Hash string
}

func (c APIClient) RemoveTorrent(params *RemoveTorrentParams) (APIResponse[RemoveTorrentData], error) {
	response := &Response[struct{}]{}
	res, err := c.Request(http.MethodDelete, "/api/v1/torrents/"+url.PathEscape(params.Hash), params, response)
	return newAPIResponse(res, RemoveTorrentData{Hash: params.Hash}), err
}

type GetStreamURLParams struct {
	Ctx
	Hash      string
	FileIndex int
}

type GetStreamURLData struct {
	URL string `json:"url"`
}

// GetStreamURL resolves a presigned playback URL.
//
// /stream answers {"url": ...} — the same JSON shape every other store's
// GenerateLink yields — so the response flows through the normal Response[T]
// path and needs no redirect handling. (/stream and /download are aliases of one
// handler on the Kizaru side, so either would do.)
func (c APIClient) GetStreamURL(params *GetStreamURLParams) (APIResponse[GetStreamURLData], error) {
	path := fmt.Sprintf("/api/v1/torrents/%s/files/%d/stream", url.PathEscape(params.Hash), params.FileIndex)

	response := &Response[GetStreamURLData]{}
	res, err := c.Request(http.MethodGet, path, params, response)
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

func magnetFiles(hash string, files []CachedFile) []store.MagnetFile {
	out := make([]store.MagnetFile, 0, len(files))
	for _, f := range files {
		out = append(out, store.MagnetFile{
			Idx:  f.FileIndex,
			Link: FileLink("").Create(hash, f.FileIndex),
			Path: canonicalPath(f.Path),
			Name: f.Name,
			Size: f.Size,
		})
	}
	return out
}

// playableFiles keeps only the files Kizaru has actually fetched and uploaded.
//
// A torrent can be globally cached while individual files are still being
// pulled, and an unuploaded file has no presigned URL to hand out.
func playableFiles(hash string, files []TorrentFile) []store.MagnetFile {
	out := []store.MagnetFile{}
	for _, f := range files {
		if f.S3Key == "" {
			continue
		}
		name := f.Path
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		out = append(out, store.MagnetFile{
			Idx:  f.FileIndex,
			Link: FileLink("").Create(hash, f.FileIndex),
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
