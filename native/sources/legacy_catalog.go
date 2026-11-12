package sources

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"duanjuapp/native/core"
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
	pool      []core.Drama
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
func (w *legacyWalk) fill(ctx context.Context, provider *legacyProvider, want int) error {
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
			result, err := provider.feedPage(ctx, feed, page)
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

// feedPage 取某条流的第 page 页，遇到 4010（频率限制）退避重试。
func (p *legacyProvider) feedPage(ctx context.Context, feed legacyFeed, page int) (legacyList, error) {
	params := map[string]string{
		"pageNumber": strconv.Itoa(page),
		"pageSize":   strconv.Itoa(legacyPageSize),
	}
	if feed.tab {
		params["tabSortType"] = "1"
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var result legacyList
		err := p.call(ctx, feed.path, params, &result)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !legacyRateLimited(err) || ctx.Err() != nil {
			break
		}
		if err := legacySleep(ctx, time.Duration(attempt+1)*700*time.Millisecond); err != nil {
			return legacyList{}, err
		}
	}
	return legacyList{}, lastErr
}

func (p *legacyProvider) feeds(ctx context.Context, category string) ([]legacyFeed, error) {
	var tabs legacyTabList
	if err := p.call(ctx, "/api/app/playlet-tab/all", nil, &tabs); err != nil {
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
			return nil, errors.New("黄果旧版未返回可用分类")
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

// catalogWalk 取出（或新建）某个分类的爬取进度。过期或分类变更时重建。
func (p *legacyProvider) catalogWalk(ctx context.Context, category string) (*legacyWalk, error) {
	key := category
	if key == "" {
		key = "*"
	}
	p.mu.Lock()
	walk := p.walks[key]
	p.mu.Unlock()
	if walk != nil && !walk.stale() {
		return walk, nil
	}
	feeds, err := p.feeds(ctx, category)
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
	p.mu.Lock()
	p.walks[key] = walk
	p.mu.Unlock()
	return walk, nil
}

func (p *legacyProvider) Categories(ctx context.Context, force bool) ([]core.Category, error) {
	var tabs legacyTabList
	if err := p.call(ctx, "/api/app/playlet-tab/all", nil, &tabs); err != nil {
		return nil, err
	}
	categories := make([]core.Category, 0, len(tabs)+1)
	for _, tab := range tabs {
		if tab.ID == "" {
			continue
		}
		categories = append(categories, core.Category{ID: tab.ID, Name: core.Truncate(core.FirstNonEmpty(tab.Name, tab.ID), 24)})
	}
	if len(categories) == 0 {
		return nil, errors.New("黄果旧版未返回可用分类")
	}
	return append(categories, core.Category{ID: legacyRecommendID, Name: legacyRecommendName}), nil
}

func (p *legacyProvider) CatalogPage(ctx context.Context, page int, category string) ([]core.Drama, bool, error) {
	if category == legacyRecommendID && page > 1 {
		return []core.Drama{}, false, nil
	}
	walk, err := p.catalogWalk(ctx, category)
	if err != nil {
		return nil, false, err
	}
	size := legacyPageSize
	from := max(0, (max(1, page)-1)*size)
	to := from + size
	if err := walk.fill(ctx, p, to); len(walk.pool) == 0 && err != nil {
		return nil, false, err
	}
	if from >= len(walk.pool) {
		return nil, false, nil
	}
	if to > len(walk.pool) {
		to = len(walk.pool)
	}
	return append([]core.Drama{}, walk.pool[from:to]...), walk.more(to), nil
}

// legacyDramaItem 把站点条目规范成 App 内部格式：补站源前缀、渠道名和封面地址。
func legacyDramaItem(drama core.Drama, channel string) core.Drama {
	drama.SourceID, drama.Source = drama.ID, sourceCloudFront
	drama.ID = core.ProviderDramaID(sourceCloudFront, drama.SourceID)
	if channel != "" {
		drama.ChannelName = channel
	}
	drama.CategoryName = core.FirstNonEmpty(drama.CategoryName, drama.CategoryNameSnake, drama.Category, channel)
	for _, cover := range []any{drama.Cover, drama.CoverURL, drama.CoverURLSnake, drama.Image, drama.ImageURL, drama.ImageURLSnake, drama.Img, drama.Pic, drama.Picture, drama.Poster, drama.Thumb, drama.Thumbnail} {
		if address := legacyCoverURL(cover); address != "" {
			drama.Cover, drama.CoverURL = address, address
			break
		}
	}
	return drama
}

// Search 用站点的关键词接口做真正的在线搜索。站点是「标题包含关键词」的模糊匹配，
// 命中数和关键词写法强相关（错一个字就是 0 条）。
func (p *legacyProvider) Search(ctx context.Context, page int, query string) ([]core.Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 64 {
		return nil, false, errors.New("搜索关键词无效")
	}
	params := map[string]string{
		"keyword":    query,
		"pageNumber": strconv.Itoa(max(1, page)),
		"pageSize":   strconv.Itoa(legacyPageSize),
	}
	var result legacyList
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result = legacyList{}
		err := p.call(ctx, "/api/app/playlet/keyword/search", params, &result)
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
	items := make([]core.Drama, 0, len(result.List))
	for _, drama := range result.List {
		if drama.ID == "" || seen[drama.ID] {
			continue
		}
		seen[drama.ID] = true
		items = append(items, legacyDramaItem(drama, ""))
	}
	return items, len(result.List) >= legacyPageSize, nil
}

func (p *legacyProvider) Detail(ctx context.Context, sourceID string) (core.Drama, []core.Chapter, error) {
	if strings.TrimSpace(sourceID) == "" {
		return core.Drama{}, nil, errors.New("黄果旧版剧集标识无效")
	}
	var detail legacyDetail
	if err := p.call(ctx, "/api/app/playlet/detail/"+url.PathEscape(sourceID), nil, &detail); err != nil {
		return core.Drama{}, nil, err
	}
	chapters := detail.Chapters
	if len(chapters) == 0 {
		if err := p.call(ctx, "/api/app/playlet-chapter/list/"+url.PathEscape(sourceID), nil, &chapters); err != nil {
			return core.Drama{}, nil, err
		}
	}
	chapters = core.UniqueChapters(chapters)
	for index := range chapters {
		chapter := &chapters[index]
		chapter.Source = sourceCloudFront
		chapter.ID = core.ProviderChapterID(sourceCloudFront, sourceID, core.FirstNonEmpty(chapter.ID, strconv.Itoa(index+1)))
		chapter.Referer = legacyFrontendURL + "/"
		if core.ChapterEpisodeNumber(*chapter, 0) <= 0 {
			chapter.CurrentEpisode = core.RawEpisode(index + 1)
		}
		chapter.Title = core.FirstNonEmpty(chapter.Title, "第"+chapter.EpisodeString(index+1)+"集")
	}
	core.SortProviderChapters(chapters)
	drama := detail.Drama
	drama.Title, drama.Name = detail.Title, detail.Name
	if drama.ID != "" && drama.ID != sourceID && drama.ID != core.ProviderDramaID(sourceCloudFront, sourceID) {
		return core.Drama{}, nil, errors.New("黄果旧版详情与请求剧集不符")
	}
	drama.ID, drama.Source, drama.SourceID = core.ProviderDramaID(sourceCloudFront, sourceID), sourceCloudFront, sourceID
	for _, cover := range []any{drama.CoverURL, drama.CoverURLSnake, drama.Cover, drama.ImageURL, drama.ImageURLSnake, drama.Image, drama.Img, drama.Pic, drama.Poster} {
		if address := legacyCoverURL(cover); address != "" {
			drama.Cover, drama.CoverURL = address, address
			break
		}
	}
	return drama, chapters, nil
}

func (p *legacyProvider) ResolveMedia(ctx context.Context, task core.Task) (core.ProviderMedia, error) {
	if task.Chapter.VideoURL == "" {
		return core.ProviderMedia{}, errors.New("此分集没有可用的播放地址")
	}
	key, err := legacyDecodeHexKey("")
	if err != nil {
		return core.ProviderMedia{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		access, err := p.credentials(ctx)
		if err != nil {
			return core.ProviderMedia{}, err
		}
		base, err := p.apiEndpoint(ctx)
		if err != nil {
			return core.ProviderMedia{}, err
		}
		address := task.Chapter.VideoURL
		if !core.IsProviderHTTPMediaURL(address) {
			address = base + "/api/app/vid/h5/m3u8/" + url.PathEscape(strings.Trim(address, "/")) + "?" + url.Values{
				"token": {access.Token}, "c": {legacyDefaultCDNURL},
			}.Encode()
		}
		playlist, finalURL, err := p.host.FetchPlaylist(ctx, address, legacyFrontendURL+"/", nil)
		if err != nil {
			var expired *legacyAPIError
			if attempt == 0 && access.Anonymous && errors.As(err, &expired) && expired.code == "5005" {
				p.invalidateToken(access)
				continue
			}
			return core.ProviderMedia{}, err
		}
		return core.ProviderMedia{
			URL:      finalURL,
			Playlist: playlist,
			Referer:  legacyFrontendURL + "/",
			HLSKey:   key,
		}, nil
	}
	return core.ProviderMedia{}, errors.New("黄果旧版播放会话已失效，请稍后重试")
}

// RepairCover 把历史库里存下的 CDN 绝对地址还原成当前可用的封面地址。
func (p *legacyProvider) RepairCover(drama core.LibraryDrama) string {
	address, err := url.Parse(drama.Cover)
	if err != nil {
		return legacyCoverURL(drama.Cover)
	}
	media, _ := url.Parse(legacyDefaultCDNURL)
	if strings.EqualFold(address.Hostname(), media.Hostname()) {
		address.Scheme, address.Host = "", ""
		return legacyCoverURL(address.String())
	}
	return legacyCoverURL(drama.Cover)
}

// legacyDetail 兼容 detail 接口把剧目字段平铺在顶层、同时带 chapters 的写法。
type legacyDetail struct {
	core.Drama
	Title    string         `json:"title"`
	Name     string         `json:"name"`
	Chapters []core.Chapter `json:"chapters"`
}

func (detail *legacyDetail) UnmarshalJSON(body []byte) error {
	var head struct {
		Title    string         `json:"title"`
		Name     string         `json:"name"`
		Chapters []core.Chapter `json:"chapters"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return err
	}
	var fields map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return err
	}
	detail.Title, detail.Name, detail.Chapters = head.Title, head.Name, head.Chapters
	detail.Drama = core.Drama{
		ID:    core.MapString(fields, "id"),
		Desc:  core.MapString(fields, "desc", "description", "summary"),
		Intro: core.MapString(fields, "intro"),
		Cover: fields["cover"], CoverURL: fields["coverUrl"], CoverURLSnake: fields["cover_url"],
		Image: fields["image"], ImageURL: fields["imageUrl"], ImageURLSnake: fields["image_url"],
		Img: fields["img"], Pic: fields["pic"], Picture: fields["picture"], Poster: fields["poster"],
		Thumb: fields["thumb"], Thumbnail: fields["thumbnail"],
		TotalEpisode: fields["totalEpisode"], TotalEpisodeSnake: fields["total_episode"],
		EpisodeCount: fields["episodeCount"], EpisodeCountSnake: fields["episode_count"],
		ChapterCount: fields["chapterCount"], ChapterCountSnake: fields["chapter_count"],
		Total: fields["total"], Episodes: fields["episodes"],
		CategoryName:  core.MapString(fields, "categoryName", "category_name", "typeName", "type_name", "sortName", "sort_name", "category"),
		Heat:          core.MapString(fields, "heat"),
		Views:         core.MapString(fields, "views", "play_count"),
		OnlineDate:    core.MapString(fields, "onlineDate", "online_date", "issue_date"),
		Tags:          core.MapStringSlice(fields, "tags"),
		ReleaseStatus: core.MapString(fields, "releaseStatus", "release_status"),
	}
	if value, known := fields["vip"].(bool); known {
		detail.Drama.VIP = &value
	}
	if detail.Drama.ReleaseStatus == "" {
		detail.Drama.ReleaseStatus = core.ReleaseStatusFromRemark(core.MapString(fields, "remark"))
	}
	return nil
}
