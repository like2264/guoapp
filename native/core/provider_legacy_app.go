package core

import (
	"context"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// legacyCatalogMaxPage 限制单个分类最多翻到第几页，避免异常数据把翻页拉爆。
	legacyCatalogMaxPage = 40
	// legacyCatalogMaxItems 限制一次合并的剧目总量（推荐位是无限流，必须有上限）。
	legacyCatalogMaxItems = 1200
	// legacyCatalogTTL 剧库快照的有效期，过期后重新爬，避免长期显示旧数据。
	legacyCatalogTTL = 10 * time.Minute
	// legacyPageGap 两次列表请求之间的间隔。站点对密集请求会返回 4010（频率限制），
	// 之前「全部」一次并发打 5 个分类正好触发，导致整页被判成空的。
	legacyPageGap = 200 * time.Millisecond
	// legacyRecommendID 是站点 playlet-recommend/home/choose 推荐流的分类标识。
	legacyRecommendID   = "recommend"
	legacyRecommendName = "推荐"
)

// legacyFeed 是一条可以逐页往下翻的剧目流：要么是分类 tab，要么是推荐位。
type legacyFeed struct {
	id   string
	name string
	path string
	tab  bool
}

// legacyWalk 保存一条剧库流的爬取进度。站点只认 pageNumber，且 count/hasNext 都是假值
// （count 恒为 9999、hasNext 恒为 false），所以只能用「翻到空页」来判断到底。
type legacyWalk struct {
	feeds     []legacyFeed
	pool      []Drama
	seen      map[string]bool
	next      map[string]int
	exhausted map[string]bool
	loadedAt  time.Time
}

func (w *legacyWalk) stale() bool {
	return time.Since(w.loadedAt) > legacyCatalogTTL
}

func (w *legacyWalk) more(end int) bool {
	if len(w.pool) > end {
		return true
	}
	if len(w.pool) >= legacyCatalogMaxItems {
		return false
	}
	for _, feed := range w.feeds {
		if !w.exhausted[feed.id] {
			return true
		}
	}
	return false
}

// fill 一直爬到 pool 至少有 want 条为止。每一轮给每条流各取一页，这样各分类的剧是
// 交错出现的，而不是先看完第一个分类才轮到第二个。
func (w *legacyWalk) fill(ctx context.Context, d *Downloader, want int) error {
	if want > legacyCatalogMaxItems {
		want = legacyCatalogMaxItems
	}
	var lastErr error
	for len(w.pool) < want && ctx.Err() == nil {
		before := len(w.pool)
		var roundErr error
		for _, feed := range w.feeds {
			if w.exhausted[feed.id] {
				continue
			}
			page := w.next[feed.id]
			if page < 1 {
				page = 1
			}
			if page > legacyCatalogMaxPage {
				w.exhausted[feed.id] = true
				continue
			}
			result, err := d.legacyFeedPage(ctx, feed, page)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				roundErr, lastErr = err, err
				continue
			}
			w.next[feed.id] = page + 1
			if len(result.List) == 0 {
				w.exhausted[feed.id] = true
			}
			for _, drama := range result.List {
				if drama.ID == "" || w.seen[drama.ID] {
					continue
				}
				w.seen[drama.ID] = true
				w.pool = append(w.pool, legacyDramaItem(drama, feed.name))
			}
			if err := legacySleep(ctx, legacyPageGap); err != nil {
				return err
			}
		}
		if len(w.pool) == before {
			// 一整轮一条新剧都没有：站点确实翻完了（没有报错时才敢这么判定，
			// 有报错说明是本轮被限流，留待下次请求重试）。
			if roundErr == nil {
				for _, feed := range w.feeds {
					w.exhausted[feed.id] = true
				}
			}
			break
		}
	}
	return lastErr
}

func legacySleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func legacyRateLimited(err error) bool {
	var apiErr *legacyAPIError
	return errors.As(err, &apiErr) && apiErr.code == "4010"
}

// legacyFeedPage 取某条流的第 page 页，遇到 4010（频率限制）退避重试。
func (d *Downloader) legacyFeedPage(ctx context.Context, feed legacyFeed, page int) (listResponse, error) {
	params := map[string]string{
		"pageNumber": strconv.Itoa(page),
		"pageSize":   strconv.Itoa(max(1, d.cfg.PageSize)),
	}
	if feed.tab {
		params["tabSortType"] = "1"
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var result listResponse
		err := d.fetchAPI(ctx, feed.path, params, &result)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !legacyRateLimited(err) || ctx.Err() != nil {
			break
		}
		if err := legacySleep(ctx, time.Duration(attempt+1)*700*time.Millisecond); err != nil {
			return listResponse{}, err
		}
	}
	return listResponse{}, lastErr
}

func (d *Downloader) legacyFeeds(ctx context.Context, category string) ([]legacyFeed, error) {
	var tabs legacyTabList
	if err := d.fetchAPI(ctx, "/api/app/playlet-tab/all", nil, &tabs); err != nil {
		return nil, err
	}
	recommend := legacyFeed{id: legacyRecommendID, name: legacyRecommendName, path: "/api/app/playlet-recommend/home/choose"}
	switch category {
	case legacyRecommendID:
		return []legacyFeed{recommend}, nil
	case "":
		feeds := make([]legacyFeed, 0, len(tabs)+1)
		for _, tab := range tabs {
			if tab.ID == "" {
				continue
			}
			feeds = append(feeds, legacyFeed{id: tab.ID, name: tab.Name, path: "/api/app/playlet/home/tab/" + url.PathEscape(tab.ID), tab: true})
		}
		if len(feeds) == 0 {
			return nil, errors.New("黄果旧 API 未返回可用分类")
		}
		// 推荐位放最后：先保证原分类顺序不变，再把分类里没有的剧补在后面。
		return append(feeds, recommend), nil
	default:
		for _, tab := range tabs {
			if tab.ID == category {
				return []legacyFeed{{id: tab.ID, name: tab.Name, path: "/api/app/playlet/home/tab/" + url.PathEscape(tab.ID), tab: true}}, nil
			}
		}
		return nil, errors.New("此分类已调整，请刷新分类列表")
	}
}

// legacyCatalogWalk 取出（或新建）某个分类的爬取进度。过期或分类变更时重建。
func (d *Downloader) legacyCatalogWalk(ctx context.Context, category string) (*legacyWalk, error) {
	key := category
	if key == "" {
		key = "*"
	}
	d.legacyCatalogMu.Lock()
	walk := d.legacyCatalogWalks[key]
	d.legacyCatalogMu.Unlock()
	if walk != nil && !walk.stale() {
		return walk, nil
	}
	feeds, err := d.legacyFeeds(ctx, category)
	if err != nil {
		return nil, err
	}
	walk = &legacyWalk{
		feeds:     feeds,
		seen:      map[string]bool{},
		next:      map[string]int{},
		exhausted: map[string]bool{},
		loadedAt:  time.Now(),
	}
	d.legacyCatalogMu.Lock()
	if d.legacyCatalogWalks == nil {
		d.legacyCatalogWalks = map[string]*legacyWalk{}
	}
	d.legacyCatalogWalks[key] = walk
	d.legacyCatalogMu.Unlock()
	return walk, nil
}

func (d *Downloader) fetchLegacyCatalogPage(ctx context.Context, page int) ([]Drama, bool, error) {
	return d.fetchLegacyCatalogCategoryPage(ctx, page, "")
}

func (d *Downloader) fetchLegacyCatalogCategoryPage(ctx context.Context, page int, category string) ([]Drama, bool, error) {
	walk, err := d.legacyCatalogWalk(ctx, category)
	if err != nil {
		return nil, false, err
	}
	size := max(1, d.cfg.PageSize)
	from := max(0, (max(1, page)-1)*size)
	to := from + size
	if err := walk.fill(ctx, d, to); len(walk.pool) == 0 && err != nil {
		return nil, false, err
	}
	if from >= len(walk.pool) {
		return nil, false, nil
	}
	if to > len(walk.pool) {
		to = len(walk.pool)
	}
	return append([]Drama{}, walk.pool[from:to]...), walk.more(to), nil
}

// legacyDramaItem 把站点的条目规范成 App 内部格式：补站源前缀、渠道名和封面地址。
func legacyDramaItem(drama Drama, channel string) Drama {
	drama.SourceID, drama.Source = drama.ID, sourceCloudFront
	drama.ID = providerDramaID(sourceCloudFront, drama.SourceID)
	if channel != "" {
		drama.ChannelName = channel
	}
	drama.CategoryName = firstNonEmpty(drama.CategoryName, drama.CategoryNameSnake, drama.Category, channel)
	for _, cover := range []any{drama.Cover, drama.CoverURL, drama.CoverURLSnake, drama.Image, drama.ImageURL, drama.ImageURLSnake, drama.Img, drama.Pic, drama.Picture, drama.Poster, drama.Thumb, drama.Thumbnail} {
		if address := legacyCoverURL(cover); address != "" {
			drama.Cover, drama.CoverURL = address, address
			break
		}
	}
	return drama
}

// searchLegacyCatalog 用站点的关键词接口做真正的在线搜索。旧版没接这个接口时，
// 搜索是拿本地已加载的剧库做过滤，重装后剧库只剩第一屏，搜一部剧只能命中一条。
// 站点是「标题包含关键词」的模糊匹配，命中数和关键词写法强相关（错一个字就是 0 条）。
func (d *Downloader) searchLegacyCatalog(ctx context.Context, page int, query string) ([]Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 64 {
		return nil, false, errors.New("搜索关键词无效")
	}
	params := map[string]string{
		"keyword":    query,
		"pageNumber": strconv.Itoa(max(1, page)),
		"pageSize":   strconv.Itoa(max(1, d.cfg.PageSize)),
	}
	var result listResponse
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result = listResponse{}
		err := d.fetchAPI(ctx, "/api/app/playlet/keyword/search", params, &result)
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		if !legacyRateLimited(err) || ctx.Err() != nil {
			break
		}
		if err := legacySleep(ctx, time.Duration(attempt+1)*700*time.Millisecond); err != nil {
			return nil, false, err
		}
	}
	if lastErr != nil {
		return nil, false, lastErr
	}
	seen := map[string]bool{}
	items := make([]Drama, 0, len(result.List))
	for _, drama := range result.List {
		if drama.ID == "" || seen[drama.ID] {
			continue
		}
		seen[drama.ID] = true
		items = append(items, legacyDramaItem(drama, ""))
	}
	return items, len(result.List) >= max(1, d.cfg.PageSize), nil
}

func (d *Downloader) fetchLegacyChapters(ctx context.Context, sourceID string) (string, []Chapter, error) {
	drama, chapters, err := d.fetchLegacyDetail(ctx, sourceID)
	return drama.DisplayTitle(), chapters, err
}

func (d *Downloader) fetchLegacyDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	var detail detailResponse
	if err := d.fetchAPI(ctx, "/api/app/playlet/detail/"+url.PathEscape(sourceID), nil, &detail); err != nil {
		return Drama{}, nil, err
	}
	chapters := detail.Chapters
	if len(chapters) == 0 {
		if err := d.fetchAPI(ctx, "/api/app/playlet-chapter/list/"+url.PathEscape(sourceID), nil, &chapters); err != nil {
			return Drama{}, nil, err
		}
	}
	chapters = uniqueChapters(chapters)
	for index := range chapters {
		chapter := &chapters[index]
		chapter.Source = sourceCloudFront
		chapter.ID = providerChapterID(sourceCloudFront, sourceID, firstNonEmpty(chapter.ID, strconv.Itoa(index+1)))
		chapter.Referer = legacyFrontendURL + "/"
		if chapterEpisodeNumber(*chapter, 0) <= 0 {
			chapter.CurrentEpisode = rawEpisode(index + 1)
		}
		chapter.Title = firstNonEmpty(chapter.Title, "第"+chapter.EpisodeString(index+1)+"集")
	}
	sortProviderChapters(chapters)
	drama := detail.Drama
	drama.Title, drama.Name = detail.Title, detail.Name
	if drama.ID != "" && drama.ID != sourceID && drama.ID != providerDramaID(sourceCloudFront, sourceID) {
		return Drama{}, nil, errors.New("黄果旧版详情与请求剧集不符")
	}
	drama.ID, drama.Source, drama.SourceID = providerDramaID(sourceCloudFront, sourceID), sourceCloudFront, sourceID
	for _, cover := range []any{drama.CoverURL, drama.CoverURLSnake, drama.Cover, drama.ImageURL, drama.ImageURLSnake, drama.Image, drama.Img, drama.Pic, drama.Poster} {
		if address := legacyCoverURL(cover); address != "" {
			drama.Cover, drama.CoverURL = address, address
			break
		}
	}
	return drama, chapters, nil
}

func (d *Downloader) resolveLegacyMedia(ctx context.Context, task Task) (providerMedia, error) {
	if task.Chapter.VideoURL == "" {
		return providerMedia{}, errors.New("此分集没有可用的播放地址")
	}
	var key []byte
	if d.cfg.AESKeyHex != "" {
		var err error
		key, err = hex.DecodeString(d.cfg.AESKeyHex)
		if err != nil || len(key) != 16 {
			return providerMedia{}, errors.New("aesKeyHex 必须是 16 字节 AES 密钥的 hex 编码")
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		access, err := d.legacyCredentials(ctx)
		if err != nil {
			return providerMedia{}, err
		}
		base, err := d.apiEndpoint(ctx)
		if err != nil {
			return providerMedia{}, err
		}
		address := task.Chapter.VideoURL
		if !isProviderHTTPMediaURL(address) {
			address = base + "/api/app/vid/h5/m3u8/" + url.PathEscape(strings.Trim(address, "/")) + "?" + url.Values{
				"token": {access.Token}, "c": {firstNonEmpty(d.cfg.CDNURL, defaultCDNURL)},
			}.Encode()
		}
		playlist, finalURL, err := d.fetchMediaPlaylist(ctx, address, legacyFrontendURL+"/")
		if err != nil {
			var expired *legacyAPIError
			if attempt == 0 && access.Anonymous && errors.As(err, &expired) && expired.code == "5005" {
				d.invalidateLegacyToken(access)
				continue
			}
			return providerMedia{}, err
		}
		return providerMedia{URL: finalURL, Playlist: playlist, Referer: legacyFrontendURL + "/", HLSKey: key, Duration: m3u8Duration(playlist)}, nil
	}
	return providerMedia{}, errors.New("黄果旧 API 的播放会话已失效，请稍后重试")
}
