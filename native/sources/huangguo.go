package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"duanjuapp/native/core"
)

// 黄果AI（huangguoai.com）与黄果视频（huangguo.video）两个站源。
//
// 黄果AI：所有 /api/* 走 edge 桥（POST /api/edge/request，信封 method/path/body/content_type），
// 列表 /api/videos/category/{id}?sort=hot&page=&size=24，详情 /api/videos/{id}，
// 播放 /api/videos/{id}/play?ep=&page_texts=0，搜索 /api/search?q=&page=&size=20。
// 站点有多个镜像域名，请求失败时按候选域名轮换。
//
// 黄果视频：纯 SSR HTML。列表 /videos?page=&category=，详情 /series/{id} 或 /video/{id}，
// 分集是页面里的 series/video 链接，播放地址取 data-hls。搜索 /search?q= 只给单页 10 条。
const (
	sourceHuangguoAI    = "huangguoai"
	sourceHuangguoVideo = "huangguo-video"
	huangguoAIName      = "黄果AI"
	huangguoVideoName   = "黄果视频"
	huangguoAIBaseURL   = "https://huangguoai.com"
	huangguoVideoURL    = "https://huangguo.video"
	huangguoAISearchCap = 20
	huangguoAIPageCap   = 24
	huangguoVideoCap    = 20
)

var (
	huangguoAIBaseMirrors = []string{
		"https://huangguoai.com",
		"https://thu.lbvrhyir.cc",
		"https://pku.xxbgwypx.cc",
		"https://ttvoij.ediayikma.cc",
		"https://thu.ediayikma.cc",
		"https://pku.ediayikma.cc",
		"https://fdu.ediayikma.cc",
		"https://thu.agdkczeyx.cc",
	}
	huangguoAIMirrorDomains = []string{"lbvrhyir.cc", "xxbgwypx.cc", "ediayikma.cc", "agdkczeyx.cc"}

	huangguoAICategories = []core.Category{
		{ID: "ai-duanju", Name: "AI 短剧"},
		{ID: "ai-manju", Name: "AI 漫剧"},
		{ID: "ai-huanlian", Name: "AI 换脸"},
		{ID: "ai-mogai", Name: "AI 魔改"},
	}

	reHuangguoCard      = regexp.MustCompile(`(?is)<article\b[^>]*class=["'][^"']*\bvideo-card\b[^"']*["'][^>]*>.*?</article>`)
	reHuangguoCardMeta  = regexp.MustCompile(`(?is)<p\b[^>]*class=["'][^"']*\bfont-mono-meta\b[^"']*["'][^>]*>(.*?)</p>`)
	reHuangguoDetailH   = regexp.MustCompile(`(?is)</h1>.*?<p\b[^>]*class=["'][^"']*\bfont-mono-meta\b[^"']*["'][^>]*>(.*?)</p>`)
	reHuangguoArticle   = regexp.MustCompile(`(?is)<article\b[^>]*>.*?</article>`)
	reHuangguoLink      = regexp.MustCompile(`(?is)<a\b[^>]*href=["']([^"']*/(?:series|video)/([^"'/?#]+)[^"']*)["'][^>]*>.*?</a>`)
	reHuangguoDataHLS   = regexp.MustCompile(`(?is)data-hls=["']([^"']+)["']`)
	reHuangguoCategoryA = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	reHuangguoStripTag  = regexp.MustCompile(`(?s)<[^>]*>`)
	reHuangguoID        = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)
)

var huangguoAIDescriptor = core.SourceDescriptor{
	ID:            sourceHuangguoAI,
	Name:          huangguoAIName,
	Description:   "AI 短剧聚合 · 在线搜索",
	BaseURL:       huangguoAIBaseURL,
	DefaultOff:    true,
	HostSuffixes:  []string{"huangguoai.com"},
	ValidCategory: validHuangguoAICategory,
}

var huangguoVideoDescriptor = core.SourceDescriptor{
	ID:            sourceHuangguoVideo,
	Name:          huangguoVideoName,
	Description:   "黄果视频 · 在线搜索",
	BaseURL:       huangguoVideoURL,
	DefaultOff:    true,
	HostSuffixes:  []string{"huangguo.video"},
	ValidCategory: validHuangguoVideoCategory,
}

func init() {
	core.Register(huangguoAIDescriptor, func(host core.Host) core.Provider {
		return &huangguoAIProvider{host: host}
	})
	core.Register(huangguoVideoDescriptor, func(host core.Host) core.Provider {
		return &huangguoVideoProvider{host: host}
	})
}

func validHuangguoAICategory(category string) bool {
	category = strings.TrimSpace(category)
	if category == "" {
		return true
	}
	for _, entry := range huangguoAICategories {
		if entry.ID == category {
			return true
		}
	}
	return false
}

func validHuangguoVideoCategory(category string) bool {
	id, err := strconv.Atoi(strings.TrimSpace(category))
	return category == "" || (err == nil && id > 0)
}

// ---------- 黄果AI ----------

type huangguoAIProvider struct {
	host core.Host
}

func (p *huangguoAIProvider) Descriptor() core.SourceDescriptor { return huangguoAIDescriptor }

func (p *huangguoAIProvider) Categories(ctx context.Context, force bool) ([]core.Category, error) {
	return append([]core.Category{}, huangguoAICategories...), nil
}

func (p *huangguoAIProvider) CatalogPage(ctx context.Context, page int, category string) ([]core.Drama, bool, error) {
	entries := huangguoAICategories
	if category != "" {
		if !validHuangguoAICategory(category) {
			return nil, false, errors.New("黄果AI 分类无效")
		}
		for _, entry := range entries {
			if entry.ID == category {
				entries = []core.Category{entry}
				break
			}
		}
	}
	site := p.host.BaseURL(sourceHuangguoAI) + "/"
	var items []core.Drama
	more := false
	var failures []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		path := fmt.Sprintf("/api/videos/category/%s?sort=hot&page=%d&size=%d", url.PathEscape(entry.ID), max(1, page), huangguoAIPageCap)
		var raw json.RawMessage
		if err := p.api(ctx, path, &raw); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", entry.Name, err))
			more = true
			continue
		}
		found := parseHuangguoAICards(raw, site, entry.Name)
		items = append(items, found...)
		more = more || len(found) >= huangguoAIPageCap
	}
	if len(items) == 0 && len(failures) == len(entries) && len(failures) > 0 {
		return nil, false, failures[0]
	}
	return items, more, nil
}

func (p *huangguoAIProvider) Detail(ctx context.Context, sourceID string) (core.Drama, []core.Chapter, error) {
	sourceID = strings.TrimPrefix(strings.Trim(strings.TrimSpace(sourceID), "/"), "detail/")
	if !reHuangguoID.MatchString(sourceID) {
		return core.Drama{}, nil, errors.New("无效的黄果AI 剧集 ID")
	}
	site := p.host.BaseURL(sourceHuangguoAI) + "/"
	var detail map[string]any
	if err := p.api(ctx, "/api/videos/"+url.PathEscape(sourceID), &detail); err != nil {
		return core.Drama{}, nil, err
	}
	drama, ok := huangguoDramaFromMap(detail, site, "")
	if !ok || drama.SourceID != sourceID {
		return core.Drama{}, nil, errors.New("黄果AI 详情与请求剧集不符")
	}
	rows, _ := detail["episodes"].([]any)
	var chapters []core.Chapter
	for _, value := range rows {
		row, _ := value.(map[string]any)
		episode, err := strconv.Atoi(core.MapString(row, "ep_num"))
		if err != nil || episode < 1 {
			continue
		}
		chapters = append(chapters, huangguoAIChapter(site, sourceID, episode, core.MapString(row, "title")))
	}
	if len(rows) == 0 {
		chapters = append(chapters, huangguoAIChapter(site, sourceID, 1, drama.DisplayTitle()))
	}
	core.SortProviderChapters(chapters)
	return drama, core.UniqueChapters(chapters), nil
}

func huangguoAIChapter(base, sourceID string, episode int, title string) core.Chapter {
	key, pageURL := "auto-1", base+"video/"+sourceID+"/"
	if episode > 1 {
		key = "ep-" + strconv.Itoa(episode)
		pageURL += key + "/"
	}
	return core.Chapter{
		ID: core.ProviderChapterID(sourceHuangguoAI, sourceID, key), Source: sourceHuangguoAI,
		Title:          core.FirstNonEmpty(title, fmt.Sprintf("第%d集", episode)),
		PageURL:        pageURL,
		CurrentEpisode: core.RawEpisode(episode),
	}
}

func (p *huangguoAIProvider) ResolveMedia(ctx context.Context, task core.Task) (core.ProviderMedia, error) {
	source, sourceID, ok := core.SplitProviderDramaID(task.DramaID)
	if !ok || source != sourceHuangguoAI || !reHuangguoID.MatchString(sourceID) {
		return core.ProviderMedia{}, errors.New("黄果AI 播放分集信息无效，请刷新详情")
	}
	episode := core.ChapterEpisodeNumber(task.Chapter, 0)
	if episode < 1 {
		return core.ProviderMedia{}, errors.New("黄果AI 集数无效")
	}
	var play struct {
		VideoURL string `json:"video_url"`
	}
	path := fmt.Sprintf("/api/videos/%s/play?ep=%d&page_texts=0", url.PathEscape(sourceID), episode)
	if err := p.api(ctx, path, &play); err != nil {
		return core.ProviderMedia{}, err
	}
	site := p.host.BaseURL(sourceHuangguoAI) + "/"
	address := core.NormalizeProviderMediaURL(site, play.VideoURL)
	if address == "" {
		return core.ProviderMedia{}, errors.New("黄果AI 该集暂无可播放地址")
	}
	return p.host.PrepareWebMedia(ctx, core.ProviderMedia{URL: address, Referer: site}, huangguoAIName)
}

func (p *huangguoAIProvider) Search(ctx context.Context, page int, query string) ([]core.Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 64 {
		return nil, false, errors.New("黄果AI 搜索关键词无效")
	}
	var result struct {
		Items      json.RawMessage `json:"items"`
		Pagination struct {
			Pages int `json:"pages"`
		} `json:"pagination"`
	}
	path := "/api/search?" + url.Values{
		"q": {query}, "page": {strconv.Itoa(max(1, page))}, "size": {strconv.Itoa(huangguoAISearchCap)},
	}.Encode()
	if err := p.api(ctx, path, &result); err != nil {
		return nil, false, err
	}
	site := p.host.BaseURL(sourceHuangguoAI) + "/"
	return parseHuangguoAICards(result.Items, site, ""), page < result.Pagination.Pages, nil
}

// api 通过站点的 edge 桥调用接口。
func (p *huangguoAIProvider) api(ctx context.Context, path string, out any) error {
	envelope, err := json.Marshal(map[string]string{
		"method": http.MethodGet, "path": path, "body": "", "content_type": "",
	})
	if err != nil {
		return err
	}
	body, err := p.text(ctx, "/api/edge/request", envelope)
	if err != nil {
		return err
	}
	var reply struct {
		Status int             `json:"status"`
		Msg    string          `json:"msg"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		return fmt.Errorf("黄果AI 接口返回格式无效：%w", err)
	}
	if reply.Status != 1 {
		return fmt.Errorf("黄果AI 接口返回失败：%s", core.FirstNonEmpty(reply.Msg, "未知错误"))
	}
	if err := json.Unmarshal(reply.Data, out); err != nil {
		return fmt.Errorf("黄果AI 接口数据格式已变化：%w", err)
	}
	return nil
}

// text 依次尝试镜像域名取文本，成功后记住可用域名。
func (p *huangguoAIProvider) text(ctx context.Context, path string, payload []byte) (string, error) {
	base := strings.TrimRight(p.host.BaseURL(sourceHuangguoAI), "/")
	candidates := huangguoAIMirrorCandidates(base, path)
	var lastErr error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		request := core.HTTPRequest{
			Label:   huangguoAIName,
			Method:  http.MethodGet,
			URL:     candidate,
			Header:  map[string]string{"Accept": "application/json", "Accept-Language": "zh-CN,zh;q=0.9"},
			Limit:   20 << 20,
			Timeout: 15,
		}
		if payload != nil {
			request.Method = http.MethodPost
			request.Body = payload
			request.Header["Content-Type"] = "application/json"
		}
		if index := strings.Index(candidate, "/api/"); index > 0 {
			request.Header["Referer"] = candidate[:index]
		} else {
			request.Header["Referer"] = candidate
		}
		response, err := p.host.Fetch(ctx, request)
		if err != nil {
			lastErr = err
			continue
		}
		return string(response.Body), nil
	}
	if lastErr == nil {
		lastErr = errors.New("黄果AI 无可用域名")
	}
	return "", lastErr
}

func huangguoAIMirrorCandidates(base, path string) []string {
	out := make([]string, 0, len(huangguoAIBaseMirrors)+8)
	seen := map[string]bool{}
	add := func(candidate string) {
		if candidate == "" || seen[candidate] {
			return
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	add(base + path)
	for _, mirror := range huangguoAIBaseMirrors {
		add(strings.TrimRight(mirror, "/") + path)
	}
	for _, generated := range huangguoGeneratedMirrors(base+path, 4) {
		add(generated + path)
	}
	return out
}

func huangguoGeneratedMirrors(_ string, n int) []string {
	out := make([]string, 0, n)
	seen := map[string]bool{}
	for i := 0; len(out) < n && i < n*8; i++ {
		sum := fmt.Sprintf("%x", rand.Uint64()^uint64(i))
		sub := strings.TrimLeft(sum[:6], "0123456789")
		if len(sub) < 3 || seen[sub] {
			continue
		}
		seen[sub] = true
		out = append(out, "https://"+sub+"."+huangguoAIMirrorDomains[len(out)%len(huangguoAIMirrorDomains)])
	}
	return out
}

// ---------- 黄果视频 ----------

type huangguoVideoProvider struct {
	host core.Host
}

func (p *huangguoVideoProvider) Descriptor() core.SourceDescriptor { return huangguoVideoDescriptor }

func (p *huangguoVideoProvider) Categories(ctx context.Context, force bool) ([]core.Category, error) {
	site := strings.TrimRight(p.host.BaseURL(sourceHuangguoVideo), "/")
	body, err := p.text(ctx, site+"/videos")
	if err != nil {
		return nil, err
	}
	return parseHuangguoVideoCategories(body), nil
}

func (p *huangguoVideoProvider) CatalogPage(ctx context.Context, page int, category string) ([]core.Drama, bool, error) {
	if !validHuangguoVideoCategory(category) {
		return nil, false, errors.New("黄果视频分类无效")
	}
	site := strings.TrimRight(p.host.BaseURL(sourceHuangguoVideo), "/")
	address := fmt.Sprintf("%s/videos?page=%d", site, max(1, page))
	if category != "" {
		address += "&category=" + url.QueryEscape(category)
	}
	body, err := p.text(ctx, address)
	if err != nil {
		return nil, false, err
	}
	items := parseHuangguoVideoCards(body, address)
	return items, len(items) >= huangguoVideoCap, nil
}

func (p *huangguoVideoProvider) Detail(ctx context.Context, sourceID string) (core.Drama, []core.Chapter, error) {
	sourceID = strings.Trim(strings.TrimSpace(sourceID), "/")
	if sourceID == "" {
		return core.Drama{}, nil, errors.New("黄果视频剧集标识无效")
	}
	site := strings.TrimRight(p.host.BaseURL(sourceHuangguoVideo), "/")
	detailPath := sourceID
	if !strings.HasPrefix(detailPath, "series/") && !strings.HasPrefix(detailPath, "video/") {
		detailPath = "series/" + detailPath
	}
	detailURL := site + "/" + detailPath
	body, err := p.text(ctx, detailURL)
	if err != nil {
		return core.Drama{}, nil, err
	}
	drama := huangguoVideoMetadata(body, detailURL, sourceID)
	title := drama.DisplayTitle()
	var episodes []huangguoEpisode
	if strings.HasPrefix(detailPath, "video/") {
		if hls := parseHuangguoHLS(body, detailURL); hls != "" {
			episodes = []huangguoEpisode{{Key: "1", Title: title, URL: detailURL, Index: 1, HLS: hls}}
		}
	} else {
		episodes = parseHuangguoVideoEpisodes(body, detailURL)
	}
	if len(episodes) == 0 {
		if hls := parseHuangguoHLS(body, detailURL); hls != "" {
			episodes = []huangguoEpisode{{Key: "1", Title: title, URL: detailURL, Index: 1, HLS: hls}}
		}
	}
	var chapters []core.Chapter
	for _, ep := range episodes {
		index := ep.Index
		if index <= 0 {
			index = len(chapters) + 1
		}
		key := ep.Key
		if key == "" {
			key = strconv.Itoa(index)
		}
		chapters = append(chapters, core.Chapter{
			ID: core.ProviderChapterID(sourceHuangguoVideo, sourceID, key), Source: sourceHuangguoVideo,
			Title:          core.FirstNonEmpty(strings.TrimSpace(ep.Title), fmt.Sprintf("第%d集", index)),
			VideoURL:       ep.HLS,
			PageURL:        ep.URL,
			CurrentEpisode: core.RawEpisode(index),
			Referer:        site + "/",
		})
	}
	core.SortProviderChapters(chapters)
	return drama, core.UniqueChapters(chapters), nil
}

func (p *huangguoVideoProvider) ResolveMedia(ctx context.Context, task core.Task) (core.ProviderMedia, error) {
	address := strings.TrimSpace(task.Chapter.VideoURL)
	if !core.IsProviderHTTPMediaURL(address) {
		if task.Chapter.PageURL == "" {
			return core.ProviderMedia{}, errors.New("黄果视频该集未解析到播放地址")
		}
		return core.ProviderMedia{}, errors.New("黄果视频该集暂无可用播放地址")
	}
	referer := core.FirstNonEmpty(task.Chapter.Referer, task.Chapter.PageURL)
	return p.host.PrepareWebMedia(ctx, core.ProviderMedia{URL: address, Referer: referer}, huangguoVideoName)
}

func (p *huangguoVideoProvider) Search(ctx context.Context, page int, query string) ([]core.Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 64 {
		return nil, false, errors.New("黄果搜索关键词无效")
	}
	if page > 1 {
		return []core.Drama{}, false, nil
	}
	site := strings.TrimRight(p.host.BaseURL(sourceHuangguoVideo), "/")
	address := site + "/search?q=" + url.QueryEscape(query)
	body, err := p.text(ctx, address)
	if err != nil {
		return nil, false, err
	}
	// 站点只给第一页 10 条：page/p/pageNum/size/limit 等参数全部被忽略（实测 8 种组合）。
	return parseHuangguoVideoCards(body, address), false, nil
}

func (p *huangguoVideoProvider) text(ctx context.Context, address string) (string, error) {
	site := strings.TrimRight(p.host.BaseURL(sourceHuangguoVideo), "/")
	response, err := p.host.FetchText(ctx, core.TextRequest{
		URL:     address,
		Referer: site + "/",
	})
	if err != nil {
		return "", err
	}
	return response.Body, nil
}

func parseHuangguoVideoCategories(body string) []core.Category {
	var categories []core.Category
	seen := map[string]bool{}
	for _, match := range reHuangguoCategoryA.FindAllStringSubmatch(body, -1) {
		href := html.UnescapeString(core.ExtractAttr("<a "+match[1]+">", "href"))
		address, err := url.Parse(href)
		if err != nil || address.Path != "" && strings.TrimRight(address.Path, "/") != "/videos" {
			continue
		}
		id := address.Query().Get("category")
		name := strings.TrimSpace(html.UnescapeString(reHuangguoStripTag.ReplaceAllString(match[2], "")))
		if !validHuangguoVideoCategory(id) || id == "" || seen[id] || name == "" || len([]rune(name)) > 24 {
			continue
		}
		seen[id] = true
		categories = append(categories, core.Category{ID: id, Name: name})
	}
	return categories
}

// ---------- 共用解析 ----------

func parseHuangguoAICards(raw []byte, pageURL, category string) []core.Drama {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []core.Drama
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			if drama, ok := huangguoDramaFromMap(x, pageURL, category); ok {
				if !seen[drama.SourceID] {
					seen[drama.SourceID] = true
					out = append(out, drama)
				}
				return
			}
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(decoded)
	return out
}

func huangguoDramaFromMap(m map[string]any, pageURL, category string) (core.Drama, bool) {
	sourceID := core.FirstNonEmpty(
		core.MapString(m, "id"), core.MapString(m, "videoId"), core.MapString(m, "video_id"),
		core.MapString(m, "vid"), core.MapString(m, "slug"),
	)
	if detailID := detailIDFromText(sourceID); detailID != "" {
		sourceID = detailID
	}
	sourceID = cleanHuangguoID(strings.Trim(sourceID, " /"))
	if sourceID == "" {
		return core.Drama{}, false
	}
	title := core.FirstNonEmpty(
		core.MapString(m, "title"), core.MapString(m, "name"), core.MapString(m, "videoTitle"),
		core.MapString(m, "video_title"), core.MapString(m, "videoName"), core.MapString(m, "video_name"),
	)
	if huangguoTitleBroken(title, sourceID) {
		return core.Drama{}, false
	}
	cover := core.ResolveProviderURL(pageURL, core.FirstNonEmpty(
		core.MapString(m, "cover"), core.MapString(m, "coverUrl"), core.MapString(m, "cover_url"),
		core.MapString(m, "coverImage"), core.MapString(m, "cover_image"), core.MapString(m, "image"),
		core.MapString(m, "imageUrl"), core.MapString(m, "thumbnail"), core.MapString(m, "poster"),
		core.MapString(m, "posterUrl"),
	))
	desc := core.FirstNonEmpty(
		core.MapString(m, "desc"), core.MapString(m, "description"),
		core.MapString(m, "intro"), core.MapString(m, "summary"),
	)
	eps := huangguoEpisodeCount(m)
	status := ""
	switch strings.ToLower(core.MapString(m, "is_finished")) {
	case "true", "1":
		status = "finished"
	case "false", "0":
		status = "ongoing"
	}
	remark := core.FirstNonEmpty(core.MapString(m, "vod_remarks"), core.MapString(m, "remark"), core.MapString(m, "remarks"))
	if remark == "" && eps != "" {
		if status == "finished" {
			remark = "全" + eps + "集"
		} else {
			remark = "更新至 " + eps + " 集"
		}
	}
	return core.Drama{
		ID: core.ProviderDramaID(sourceHuangguoAI, sourceID), Source: sourceHuangguoAI, SourceID: sourceID,
		Title: title, Name: title, Desc: desc, Intro: desc, Cover: cover, CoverURL: cover,
		TotalEpisode: eps, EpisodeCount: eps, CategoryName: category, ChannelName: "huangguoai.com",
		Remark: remark,
		Score:  core.FirstNonEmpty(core.MapString(m, "score"), core.MapString(m, "rating")),
		Views: core.NormalizeViews(core.FirstNonEmpty(
			core.MapString(m, "views"), core.MapString(m, "view_count"), core.MapString(m, "viewCount"),
			core.MapString(m, "play_count"), core.MapString(m, "playCount"), core.MapString(m, "plays"),
		)),
		OnlineDate: core.ProviderReleaseDate(core.FirstNonEmpty(
			core.MapString(m, "onlineDate"), core.MapString(m, "online_date"),
			core.MapString(m, "publish_date"), core.MapString(m, "publishDate"),
			core.MapString(m, "release_date"), core.MapString(m, "releaseDate"),
		)),
		Tags:          core.MapStringSlice(m, "tags", "tag", "categories", "category"),
		ReleaseStatus: status,
	}, true
}

func huangguoTitleBroken(title, sourceID string) bool {
	title = strings.TrimSpace(title)
	if title == "" || title == sourceID || title == "短剧" || title == "搜索" {
		return true
	}
	return strings.Contains(title, "搜索记录") && strings.Contains(title, "暂无搜索记录")
}

func huangguoEpisodeCount(row map[string]any) string {
	for _, key := range []string{"episode_count", "episodeCount", "total_episodes", "totalEpisodes", "totalEpisode", "total_episode", "episodes", "total"} {
		if count, err := strconv.Atoi(core.MapString(row, key)); err == nil && count > 0 {
			return strconv.Itoa(count)
		}
	}
	return ""
}

func parseHuangguoVideoCards(rawHTML, pageURL string) []core.Drama {
	blocks := reHuangguoCard.FindAllString(rawHTML, -1)
	if len(blocks) == 0 {
		for _, block := range reHuangguoArticle.FindAllString(rawHTML, -1) {
			if strings.Contains(strings.ToLower(block), "video-card") {
				blocks = append(blocks, block)
			}
		}
	}
	// 搜索结果页的卡片没有 video-card 这个 class，改用「带剧集链接的任意 article」兜底；
	// 仍然要求块内有 /series/ 或 /video/ 链接，所以不会把导航或广告块当成剧集。
	if len(blocks) == 0 {
		blocks = reHuangguoArticle.FindAllString(rawHTML, -1)
	}
	// 既没有 article 包裹时，退到直接用剧集链接当块（标题在链接内部，实测可以取到）。
	if len(blocks) == 0 {
		blocks = reHuangguoLink.FindAllString(rawHTML, -1)
	}
	seen := map[string]bool{}
	var out []core.Drama
	for _, block := range blocks {
		link := reHuangguoLink.FindStringSubmatch(block)
		if len(link) < 3 {
			continue
		}
		path, sourceID := huangguoVideoSourceID(link[1])
		if sourceID == "" || seen[sourceID] {
			continue
		}
		seen[sourceID] = true
		title := core.FirstNonEmpty(core.ExtractAttr(block, "title", "alt", "aria-label"), core.CleanText(block))
		cover := core.ResolveProviderURL(pageURL, core.ExtractAttr(block, "data-src", "data-original", "src"))
		category := "video"
		if strings.HasPrefix(path, "series/") {
			category = "series"
		}
		desc := core.FirstNonEmpty(core.ExtractAttr(block, "data-description", "description"), core.ExtractDescription(block))
		status := ""
		if category == "series" {
			if meta := reHuangguoCardMeta.FindStringSubmatch(block); len(meta) > 1 {
				status = core.ReleaseStatusFromRemark(core.CleanText(meta[1]))
			}
		}
		out = append(out, core.Drama{
			ID: core.ProviderDramaID(sourceHuangguoVideo, sourceID), Source: sourceHuangguoVideo, SourceID: sourceID,
			Title: title, Name: title, Desc: desc, Intro: desc, Cover: cover, CoverURL: cover,
			CategoryName: category, ChannelName: "huangguo.video",
			Remark:        core.FirstNonEmpty(huangguoClassText(block, "bg-black/55"), huangguoClassText(block, "text-gold-dim")),
			ReleaseStatus: status,
		})
	}
	return out
}

func huangguoClassText(raw, className string) string {
	if className == "" {
		return ""
	}
	re := regexp.MustCompile(`(?is)<[^>]+class=["'][^"']*` + regexp.QuoteMeta(className) + `[^"']*["'][^>]*>(.*?)</[^>]+>`)
	if m := re.FindStringSubmatch(raw); len(m) > 1 {
		return core.CleanText(m[1])
	}
	return ""
}

type huangguoEpisode struct {
	Key   string
	Title string
	URL   string
	Index int
	HLS   string
}

func parseHuangguoVideoEpisodes(rawHTML, pageURL string) []huangguoEpisode {
	blocks := reHuangguoCard.FindAllString(rawHTML, -1)
	if len(blocks) == 0 {
		blocks = reHuangguoLink.FindAllString(rawHTML, -1)
	}
	positions := map[string]int{}
	var episodes []huangguoEpisode
	numbered := false
	for _, block := range blocks {
		link := reHuangguoLink.FindStringSubmatch(block)
		if len(link) < 2 {
			continue
		}
		path, key := huangguoVideoSourceID(link[1])
		if key == "" || !strings.HasPrefix(path, "video/") {
			continue
		}
		fullURL := core.ResolveProviderURL(pageURL, link[1])
		title := core.FirstNonEmpty(core.ExtractAttr(block, "title"), core.ExtractAttr(block, "alt"), core.CleanText(block))
		index := core.EpisodeIndex(core.CleanText(block), core.EpisodeIndex(title, 0))
		numbered = numbered || index > 0
		episode := huangguoEpisode{Key: key, Title: title, URL: fullURL, Index: index, HLS: parseHuangguoHLS(block, pageURL)}
		if position, found := positions[fullURL]; found {
			if episodes[position].Index <= 0 && index > 0 {
				episodes[position] = episode
			}
		} else {
			positions[fullURL] = len(episodes)
			episodes = append(episodes, episode)
		}
	}
	var result []huangguoEpisode
	used := map[int]bool{}
	for _, episode := range episodes {
		if numbered && episode.Index <= 0 {
			continue
		}
		episode.Index = nextHuangguoIndex(episode.Index, used)
		used[episode.Index] = true
		result = append(result, episode)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Index != result[j].Index {
			return result[i].Index < result[j].Index
		}
		return result[i].Key < result[j].Key
	})
	return result
}

func nextHuangguoIndex(preferred int, used map[int]bool) int {
	if preferred <= 0 {
		preferred = 1
	}
	if !used[preferred] {
		return preferred
	}
	for i := 1; ; i++ {
		if !used[i] {
			return i
		}
	}
}

func parseHuangguoHLS(rawHTML, pageURL string) string {
	if m := reHuangguoDataHLS.FindStringSubmatch(rawHTML); len(m) > 1 {
		return core.NormalizeProviderMediaURL(pageURL, m[1])
	}
	return ""
}

func huangguoVideoSourceID(href string) (path, sourceID string) {
	u, err := url.Parse(html.UnescapeString(href))
	if err == nil {
		path = strings.Trim(u.Path, "/")
	} else {
		path = strings.Trim(href, "/")
	}
	parts := strings.Split(path, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "series" || parts[i] == "video" {
			code := cleanHuangguoID(parts[i+1])
			if code == "" {
				return "", ""
			}
			return parts[i] + "/" + code, parts[i] + "/" + code
		}
	}
	return "", ""
}

func detailIDFromText(s string) string {
	u, err := url.Parse(html.UnescapeString(s))
	path := s
	if err == nil {
		path = u.Path
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "detail" {
			return cleanHuangguoID(parts[i+1])
		}
	}
	return ""
}

func cleanHuangguoID(s string) string {
	s = strings.TrimSpace(html.UnescapeString(s))
	s = strings.Trim(s, " /\t\r\n\"'")
	if q := strings.IndexAny(s, "?#"); q >= 0 {
		s = s[:q]
	}
	return s
}

func huangguoVideoMetadata(body, pageURL, sourceID string) core.Drama {
	title := core.FirstNonEmpty(core.ExtractPageTitle(body), sourceID)
	desc := core.ExtractDescription(body)
	drama := core.Drama{
		ID: core.ProviderDramaID(sourceHuangguoVideo, sourceID), Source: sourceHuangguoVideo, SourceID: sourceID,
		Title: title, Name: title, Desc: desc, Intro: desc,
		Cover: core.ResolveProviderURL(pageURL, core.ExtractAttr(body, "og:image")),
		Tags:  core.ExtractTags(body), ChannelName: "huangguo.video",
	}
	if header := reHuangguoDetailH.FindStringSubmatch(body); len(header) > 1 {
		drama.ReleaseStatus = core.ReleaseStatusFromRemark(core.CleanText(header[1]))
	}
	return drama
}
