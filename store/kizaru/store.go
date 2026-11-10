package kizaru

import (
	"net/http"
	"time"

	"github.com/MunifTanjim/stremthru/core"
	"github.com/MunifTanjim/stremthru/internal/cache"
	"github.com/MunifTanjim/stremthru/store"
)

var (
	_ store.Store = (*StoreClient)(nil)
)

type StoreClientConfig struct {
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
}

type StoreClient struct {
	Name           store.StoreName
	client         *APIClient
	getUserCache   cache.Cache[store.User]
	getMagnetCache cache.Cache[store.GetMagnetData]
}

func NewStoreClient(config *StoreClientConfig) *StoreClient {
	c := &StoreClient{}
	c.client = NewAPIClient(&APIClientConfig{
		BaseURL:    config.BaseURL,
		HTTPClient: config.HTTPClient,
		UserAgent:  config.UserAgent,
	})
	c.Name = store.StoreNameKizaru

	c.getUserCache = cache.NewCache[store.User](&cache.CacheConfig{
		Name:     "store:kizaru:getUser",
		Lifetime: 10 * time.Minute,
	})
	c.getMagnetCache = cache.NewCache[store.GetMagnetData](&cache.CacheConfig{
		Name:     "store:kizaru:getMagnet",
		Lifetime: 1 * time.Minute,
	})

	return c
}

func (c *StoreClient) GetName() store.StoreName {
	return c.Name
}

func (c *StoreClient) GetUser(params *store.GetUserParams) (*store.User, error) {
	cacheKey := params.GetAPIKey("")
	cached := &store.User{}
	if c.getUserCache.Get(cacheKey, cached) {
		return cached, nil
	}

	res, err := c.client.GetUser(&GetUserParams{
		Ctx: params.Ctx,
	})
	if err != nil {
		// A user with no pass is answered with a null body, not an error, so a
		// 404 here means the endpoint itself is wrong — not an unentitled user.
		return nil, err
	}

	data := &store.User{
		SubscriptionStatus: subscriptionStatus(res.Data),
	}
	if res.Data != nil {
		data.Id = res.Data.UserId
	}

	c.getUserCache.Add(cacheKey, *data)
	return data, nil
}

func (c *StoreClient) CheckMagnet(params *store.CheckMagnetParams) (*store.CheckMagnetData, error) {
	hashes := make([]string, 0, len(params.Magnets))
	for _, m := range params.Magnets {
		magnet, err := core.ParseMagnetLink(m)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, magnet.Hash)
	}

	// One round trip: Kizaru already scopes the answer to this user's library and
	// returns only files that are uploaded, so there is nothing to filter here.
	res, err := c.client.CheckTorrentsCached(&CheckTorrentsCachedParams{
		Ctx:    params.Ctx,
		Hashes: hashes,
	})
	if err != nil {
		return nil, err
	}

	data := &store.CheckMagnetData{Items: []store.CheckMagnetDataItem{}}
	for _, item := range res.Data.Items {
		data.Items = append(data.Items, store.CheckMagnetDataItem{
			Hash:   item.Hash,
			Magnet: "magnet:?xt=urn:btih:" + item.Hash,
			Name:   item.Name,
			Status: magnetStatus(item.Status),
			Files:  magnetFiles(item.Hash, item.Files),
		})
	}

	return data, nil
}

func (c *StoreClient) AddMagnet(params *store.AddMagnetParams) (*store.AddMagnetData, error) {
	var magnet *core.MagnetLink
	if params.Magnet != "" {
		m, err := core.ParseMagnetLink(params.Magnet)
		if err != nil {
			return nil, err
		}
		magnet = &m
	}

	res, err := c.client.CreateTorrent(&CreateTorrentParams{
		Ctx:    params.Ctx,
		Magnet: params.Magnet,
	})
	if err != nil {
		return nil, err
	}

	torrent := res.Data.Torrent
	data := &store.AddMagnetData{
		Id:      torrent.Id,
		Hash:    torrent.InfoHash,
		Name:    torrent.Name,
		Size:    torrent.TotalSize,
		Status:  magnetStatus(torrent.Status),
		Files:   []store.MagnetFile{},
		AddedAt: torrentAddedAt(torrent),
	}
	if magnet != nil {
		data.Magnet = magnet.Link
	} else {
		data.Magnet = "magnet:?xt=urn:btih:" + torrent.InfoHash
	}

	// Kizaru's add response carries no file list, so fetch the detail to build
	// one. Only worth doing once the torrent is ready.
	detail, err := c.client.GetTorrent(&GetTorrentParams{
		Ctx:  params.Ctx,
		Hash: torrent.InfoHash,
	})
	if err == nil {
		data.Status = magnetStatus(detail.Data.Torrent.Status)
		data.Name = detail.Data.Torrent.Name
		data.Size = detail.Data.Torrent.TotalSize
		data.Files = playableFiles(torrent.InfoHash, detail.Data.Torrent.Status, detail.Data.Files)
	}

	return data, nil
}

func (c *StoreClient) GetMagnet(params *store.GetMagnetParams) (*store.GetMagnetData, error) {
	cacheKey := params.GetAPIKey("") + ":" + params.Id
	cached := &store.GetMagnetData{}
	if c.getMagnetCache.Get(cacheKey, cached) {
		return cached, nil
	}

	res, err := c.client.GetTorrent(&GetTorrentParams{
		Ctx:  params.Ctx,
		Hash: params.Id,
	})
	if err != nil {
		return nil, err
	}

	torrent := res.Data.Torrent
	data := &store.GetMagnetData{
		Id:      torrent.Id,
		Hash:    torrent.InfoHash,
		Name:    torrent.Name,
		Size:    torrent.TotalSize,
		Status:  magnetStatus(torrent.Status),
		Files:   playableFiles(torrent.InfoHash, torrent.Status, res.Data.Files),
		AddedAt: torrentAddedAt(torrent),
	}

	c.getMagnetCache.Add(cacheKey, *data)
	return data, nil
}

func (c *StoreClient) ListMagnets(params *store.ListMagnetsParams) (*store.ListMagnetsData, error) {
	limit := params.Limit
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if limit < 1 {
		limit = maxListLimit
	}

	// Kizaru pages by keyset cursor, not by offset: the head of the list moves
	// whenever a torrent is added, so an OFFSET would repeat or skip a row — and
	// the engine ignores the parameter outright, which made every page after the
	// first re-serve page one. The store interface still pages by position, so
	// walk the cursor chain from the head to the page the caller asked for and
	// drop the remainder. Bounded by offset/limit round trips.
	offset := params.Offset
	if offset < 0 {
		offset = 0
	}
	skip := offset % limit

	var res APIResponse[ListTorrentsPage]
	var err error
	cursor := ""
	for remaining := offset / limit; ; remaining-- {
		res, err = c.client.ListTorrents(&ListTorrentsParams{
			Ctx:    params.Ctx,
			Limit:  limit,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		if remaining <= 0 {
			break
		}
		cursor = res.Data.NextCursor
		if cursor == "" {
			// The library ended before that offset: there is no such page.
			return &store.ListMagnetsData{
				Items:      []store.ListMagnetsDataItem{},
				TotalItems: listTotalItems(res.Data, offset, limit),
			}, nil
		}
	}

	data := &store.ListMagnetsData{
		Items:      []store.ListMagnetsDataItem{},
		TotalItems: listTotalItems(res.Data, offset, limit),
	}
	for _, t := range res.Data.Rows {
		data.Items = append(data.Items, store.ListMagnetsDataItem{
			Id:      t.Id,
			Hash:    t.InfoHash,
			Name:    t.Name,
			Size:    t.TotalSize,
			Status:  magnetStatus(t.Status),
			AddedAt: torrentAddedAt(t),
		})
	}
	if skip > 0 {
		if skip >= len(data.Items) {
			data.Items = []store.ListMagnetsDataItem{}
		} else {
			data.Items = data.Items[skip:]
		}
	}

	return data, nil
}

// maxListLimit is Kizaru's cap on a single page of the library.
const maxListLimit = 100

// listTotalItems is the `meta.total` the engine sends with every page. A page
// that omits it (an older engine) is inferred the old way: a full page means
// there is probably at least one more.
func listTotalItems(page ListTorrentsPage, offset, limit int) int {
	if page.Total > 0 {
		return page.Total
	}
	count := len(page.Rows)
	if count == 0 {
		return 0
	}
	total := offset + count
	if count == limit {
		total++
	}
	return total
}

func (c *StoreClient) RemoveMagnet(params *store.RemoveMagnetParams) (*store.RemoveMagnetData, error) {
	// Kizaru identifies torrents by info hash.
	_, err := c.client.RemoveTorrent(&RemoveTorrentParams{
		Ctx:  params.Ctx,
		Hash: params.Id,
	})
	if err != nil {
		return nil, err
	}
	return &store.RemoveMagnetData{Id: params.Id}, nil
}

func (c *StoreClient) GenerateLink(params *store.GenerateLinkParams) (*store.GenerateLinkData, error) {
	hash, fileIndex, err := FileLink(params.Link).Parse()
	if err != nil {
		error := core.NewAPIError("invalid link")
		error.StatusCode = http.StatusBadRequest
		error.Cause = err
		return nil, error
	}

	res, err := c.client.GetStreamURL(&GetStreamURLParams{
		Ctx:       params.Ctx,
		Hash:      hash,
		FileIndex: fileIndex,
	})
	if err != nil {
		return nil, err
	}

	return &store.GenerateLinkData{Link: res.Data.URL}, nil
}
