package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 柚子短剧（youzidj.com）适配。
//
// 站点是 Cloudflare Workers + 服务端渲染的自建站，没有 maccms 接口，页面与接口都
// 直接吐 HTML 片段：
//
//	列表：/dramas/、/ai-face-swap/、/ai-adult/，翻页 /{段落}/page/{n}/，每页 24 条
//	搜索：/api/catalogue/search?q={关键词}&kind=works&page={n}，返回 <div data-pages="N"> 片段
//	详情：/dramas/{slug}/ 是系列页，ld+json 是 TVSeries，页面里直接列全部分集链接
//	      /dramas/{slug}/season-N-episode-M/；单片（AI换脸、AI成人视频）是
//	      /ai-face-swap/{code}/ 与 /ai-adult/{code}/，ld+json 是 VideoObject。
//	播放：分集页 <section id="player" data-video="..."> 给出视频 ID，然后
//	      POST /api/account/bootstrap                      -> Set-Cookie eden_account
//	      POST /api/playback/{videoID}?mode=mse            -> {url, expires_at}
//	                                                          + Set-Cookie eden_play_{videoID}
//	      m3u8 主列表公开可读，但子播放列表与分片必须带 eden_play 这个 HttpOnly Cookie，
//	      所以这里把两个 Cookie 一起交给 providerMediaCredentials，由 core 的本地流服务
//	      统一带凭证取流。
const (
	youzidjBaseURL     = "https://youzidj.com"
	youzidjUserAgent   = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	youzidjChannelName = "柚子短剧"
	youzidjPageSize    = 24
	youzidjMaxPage     = 2000
	youzidjMaxEpisodes = 2000
	youzidjSearchKind  = "works"

	youzidjCategoryDramas   = "dramas"
	youzidjCategoryFaceSwap = "face-swap"
	youzidjCategoryAdult    = "adult"

	youzidjAccountCookie = "eden_account"
	youzidjAccountTTL    = 12 * time.Hour

	// 站点入口的 18+ 年龄确认。列表/详情这些 HTML 页照常返回（门是前端遮罩），
	// 但 /api/catalogue/search 与 /api/playback/* 对未确认的请求直接回 403
	// {"error":"请先完成网站入口的年龄确认"}。
	youzidjAgeCookieName = "eden_age_v2"
	youzidjAgeTTL        = 12 * time.Hour
	youzidjAgeRetryTTL   = time.Minute
)

var (
	reYouzidjArticle      = regexp.MustCompile(`(?is)<article[^>]*class="[^"]*\bdrama-card\b[^"]*"[^>]*>.*?</article>`)
	reYouzidjCardHref     = regexp.MustCompile(`href="(?:https?://[^"/]+)?(/(?:dramas/[a-z0-9][a-z0-9-]*/season-\d+|ai-face-swap/[a-z0-9]{4,32}|ai-adult/[a-z0-9]{4,32})/)"`)
	reYouzidjCardTitle    = regexp.MustCompile(`(?is)<p[^>]*class="[^"]*\bdrama-card-title\b[^"]*"[^>]*title="([^"]*)"`)
	reYouzidjCardTitleTxt = regexp.MustCompile(`(?is)<p[^>]*class="[^"]*\bdrama-card-title\b[^"]*"[^>]*>(.*?)</p>`)
	reYouzidjCardImage    = regexp.MustCompile(`(?is)<img[^>]+src="([^"]+)"`)
	reYouzidjCardEpisodes = regexp.MustCompile(`(?is)<span[^>]*class="[^"]*\bhome-card-episodes-(?:long|short)\b[^"]*"[^>]*>(.*?)</span>`)
	reYouzidjNextPage     = regexp.MustCompile(`(?is)<a[^>]*rel="next"[^>]*href="([^"]+)"`)
	reYouzidjNextPageAlt  = regexp.MustCompile(`(?is)<a[^>]*href="([^"]+)"[^>]*rel="next"`)
	reYouzidjPageCount    = regexp.MustCompile(`(?is)data-pages="(\d+)"`)
	reYouzidjEpisodeLink  = regexp.MustCompile(`href="(?:https?://[^"/]+)?/dramas/([a-z0-9][a-z0-9-]*)/season-(\d+)-episode-(\d+)/"`)
	reYouzidjLdJSON       = regexp.MustCompile(`(?is)<script[^>]*type="application/ld\+json"[^>]*>(.*?)</script>`)
	reYouzidjPlayer       = regexp.MustCompile(`(?is)<section[^>]*id="player"[^>]*>`)
	reYouzidjDataVideo    = regexp.MustCompile(`data-video="([^"]+)"`)
	reYouzidjTagLink      = regexp.MustCompile(`(?is)<a[^>]*href="(?:https?://[^"/]+)?/tags/([a-z0-9-]+)/"[^>]*>(.*?)</a>`)
	reYouzidjOGImage      = regexp.MustCompile(`(?is)<meta[^>]*property="og:image"[^>]*content="([^"]+)"`)
	reYouzidjOGTitle      = regexp.MustCompile(`(?is)<meta[^>]*property="og:title"[^>]*content="([^"]+)"`)
	reYouzidjHTMLTag      = regexp.MustCompile(`(?is)<[^>]+>`)
	reYouzidjSlug         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)
	reYouzidjCode         = regexp.MustCompile(`^[a-z0-9]{4,32}$`)
)

// youzidjCategories 是站点首页导航里的一级栏目。站点把「AI短剧 / AI换脸 /
// AI成人视频」做成三个独立片库，之间没有聚合列表，所以直接按栏目拆开。
var youzidjCategories = []nativeCategory{
	{ID: youzidjCategoryDramas, Name: "AI短剧"},
	{ID: youzidjCategoryFaceSwap, Name: "AI换脸"},
	{ID: youzidjCategoryAdult, Name: "AI成人视频"},
}

func validYouzidjCategory(category string) bool {
	switch strings.TrimSpace(category) {
	case "", youzidjCategoryDramas, youzidjCategoryFaceSwap, youzidjCategoryAdult:
		return true
	default:
		return false
	}
}

// validYouzidjSourceID 只接受站点里真实存在的三种详情页路径：
// dramas/{slug}、ai-face-swap/{code}、ai-adult/{code}。
func validYouzidjSourceID(id string) bool {
	kind, rest := youzidjSplitSourceID(id)
	if rest == "" {
		return false
	}
	switch kind {
	case youzidjCategoryDramas:
		return reYouzidjSlug.MatchString(rest)
	case youzidjCategoryFaceSwap, youzidjCategoryAdult:
		return reYouzidjCode.MatchString(rest)
	default:
		return false
	}
}

func youzidjSplitSourceID(id string) (kind, rest string) {
	id = strings.Trim(strings.TrimSpace(id), "/")
	switch {
	case strings.HasPrefix(id, youzidjCategoryDramas+"/"):
		return youzidjCategoryDramas, strings.TrimPrefix(id, youzidjCategoryDramas+"/")
	case strings.HasPrefix(id, "ai-face-swap/"):
		return youzidjCategoryFaceSwap, strings.TrimPrefix(id, "ai-face-swap/")
	case strings.HasPrefix(id, "ai-adult/"):
		return youzidjCategoryAdult, strings.TrimPrefix(id, "ai-adult/")
	default:
		return "", ""
	}
}

func youzidjKindName(kind string) string {
	switch kind {
	case youzidjCategoryFaceSwap:
		return "AI换脸"
	case youzidjCategoryAdult:
		return "AI成人视频"
	default:
		return "AI短剧"
	}
}

func youzidjClean(text string) string {
	text = reYouzidjHTMLTag.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = strings.NewReplacer("\u3000", " ", "\u00a0", " ").Replace(text)
	return strings.Join(strings.Fields(text), " ")
}

func youzidjAbsURL(site, raw string) string {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parsed.IsAbs() {
		return parsed.String()
	}
	base, err := url.Parse(site)
	if err != nil {
		return raw
	}
	return base.ResolveReference(parsed).String()
}

func (d *Downloader) youzidjGet(ctx context.Context, address string) (string, error) {
	site := d.providerBaseURL(sourceYouzidj)
	target := address
	if !strings.HasPrefix(address, "http") {
		target = site + address
	}
	body, err := d.youzidjGetOnce(ctx, site, target)
	if err == nil || !youzidjAgeRejected(err) {
		return body, err
	}
	// 年龄确认的凭证过期，或站点换了签名，重新确认一次再试。
	// 403 已经被限流器记成 60 秒退避，这里必须先清掉，否则重试会被本地直接挡回去。
	d.youzidjForgetAge()
	d.clearProviderBackoff(target)
	if d.youzidjAge(ctx, site, site+"/") == "" {
		return "", err
	}
	return d.youzidjGetOnce(ctx, site, target)
}

func (d *Downloader) youzidjGetOnce(ctx context.Context, site, target string) (string, error) {
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, youzidjUserAgent)
	if cookie := youzidjAgeCookieText(d.youzidjAge(ctx, site, site+"/")); cookie != "" {
		pageContext = context.WithValue(pageContext, providerTextCookieKey{}, cookie)
	}
	return d.fetchProviderText(pageContext, target, site+"/")
}

// youzidjAgeCookieText 把缓存的确认值拼成 Cookie 头。
func youzidjAgeCookieText(value string) string {
	if value == "" {
		return ""
	}
	return youzidjAgeCookieName + "=" + value
}

func youzidjJoinCookies(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

// youzidjAgeRejected 判断这次失败是不是 403。缺年龄确认凭证时站点回的就是 403。
func youzidjAgeRejected(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 403")
}

// youzidjAgeOnce 提交站点入口的年龄确认，取回 eden_age_v2。
//
// 表单是 POST /age-check（字段 return_to、decision=accepted）。站点会校验来源：
// 必须带 Origin: https://youzidj.com，只带 Referer 会回 403「请求来源无效，请刷新页面后重试」。
// 成功时回 303 并在跳转响应上下发 Cookie，所以这里不跟跳转，直接从响应里取。
func (d *Downloader) youzidjAgeOnce(ctx context.Context, site, referer string) (string, error) {
	form := url.Values{}
	form.Set("return_to", "/")
	form.Set("decision", "accepted")
	address := site + "/age-check"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("User-Agent", youzidjUserAgent)
	request.Header.Set("Referer", referer)
	if parsed, parseErr := url.Parse(address); parseErr == nil {
		request.Header.Set("Origin", providerMediaOrigin(parsed))
	}
	client := *d.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return "", fmt.Errorf("站点返回 HTTP %d", response.StatusCode)
	}
	value := youzidjCookieValue(response.Cookies(), youzidjAgeCookieName)
	if value == "" {
		return "", errors.New("站点未下发年龄确认凭证")
	}
	return value, nil
}

// youzidjAge 返回当前可用的年龄确认凭证。取不到就返回空串，请求照常发出，
// 由站点自己决定怎么回——不因为这一层把整条链路卡死。
func (d *Downloader) youzidjAge(ctx context.Context, site, referer string) string {
	d.youzidjAgeMu.Lock()
	cached, cachedAt := d.youzidjAgeTicket, d.youzidjAgeAt
	d.youzidjAgeMu.Unlock()
	ttl := youzidjAgeTTL
	if cached == "" {
		ttl = youzidjAgeRetryTTL
	}
	if !cachedAt.IsZero() && time.Since(cachedAt) < ttl {
		return cached
	}
	value, err := d.youzidjAgeOnce(ctx, site, referer)
	d.youzidjAgeMu.Lock()
	if err != nil {
		d.youzidjAgeTicket, d.youzidjAgeAt = "", time.Now()
	} else {
		d.youzidjAgeTicket, d.youzidjAgeAt = value, time.Now()
	}
	d.youzidjAgeMu.Unlock()
	return value
}

func (d *Downloader) youzidjForgetAge() {
	d.youzidjAgeMu.Lock()
	d.youzidjAgeTicket, d.youzidjAgeAt = "", time.Time{}
	d.youzidjAgeMu.Unlock()
}

func youzidjListPath(category string, page int) string {
	base := "/" + youzidjCategoryDramas + "/"
	switch strings.TrimSpace(category) {
	case youzidjCategoryFaceSwap:
		base = "/ai-face-swap/"
	case youzidjCategoryAdult:
		base = "/ai-adult/"
	}
	if page > 1 {
		return fmt.Sprintf("%spage/%d/", base, page)
	}
	return base
}

func youzidjSearchPath(query string, page int) string {
	values := url.Values{}
	values.Set("q", strings.TrimSpace(query))
	values.Set("kind", youzidjSearchKind)
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	return "/api/catalogue/search?" + values.Encode()
}

// youzidjSourceIDFromPath 把卡片链接收敛成站源里的剧集标识：系列页去掉季号，
// 单片保留完整栏目路径。
func youzidjSourceIDFromPath(path string) (sourceID, kind string) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	switch {
	case strings.HasPrefix(path, youzidjCategoryDramas+"/"):
		rest := strings.TrimPrefix(path, youzidjCategoryDramas+"/")
		if index := strings.Index(rest, "/season-"); index > 0 {
			rest = rest[:index]
		}
		if reYouzidjSlug.MatchString(rest) {
			return youzidjCategoryDramas + "/" + rest, youzidjCategoryDramas
		}
	case strings.HasPrefix(path, "ai-face-swap/"):
		rest := strings.TrimPrefix(path, "ai-face-swap/")
		if reYouzidjCode.MatchString(rest) {
			return "ai-face-swap/" + rest, youzidjCategoryFaceSwap
		}
	case strings.HasPrefix(path, "ai-adult/"):
		rest := strings.TrimPrefix(path, "ai-adult/")
		if reYouzidjCode.MatchString(rest) {
			return "ai-adult/" + rest, youzidjCategoryAdult
		}
	}
	return "", ""
}

func parseYouzidjCards(body, site string) []Drama {
	items := make([]Drama, 0, youzidjPageSize)
	seen := map[string]bool{}
	for _, block := range reYouzidjArticle.FindAllString(body, -1) {
		match := reYouzidjCardHref.FindStringSubmatch(block)
		if len(match) < 2 {
			continue
		}
		sourceID, kind := youzidjSourceIDFromPath(match[1])
		if sourceID == "" || seen[sourceID] {
			continue
		}
		title := ""
		if m := reYouzidjCardTitle.FindStringSubmatch(block); len(m) > 1 {
			title = youzidjClean(m[1])
		}
		if title == "" {
			if m := reYouzidjCardTitleTxt.FindStringSubmatch(block); len(m) > 1 {
				title = youzidjClean(m[1])
			}
		}
		if title == "" {
			continue
		}
		cover := ""
		if m := reYouzidjCardImage.FindStringSubmatch(block); len(m) > 1 {
			cover = youzidjAbsURL(site, m[1])
		}
		remark := ""
		if m := reYouzidjCardEpisodes.FindStringSubmatch(block); len(m) > 1 {
			remark = truncate(youzidjClean(m[1]), 32)
		}
		seen[sourceID] = true
		items = append(items, Drama{
			ID: providerDramaID(sourceYouzidj, sourceID), Source: sourceYouzidj, SourceID: sourceID,
			Title: truncate(title, 512), Name: truncate(title, 512),
			Cover: cover, CoverURL: cover,
			ChannelName: youzidjChannelName, CategoryName: youzidjKindName(kind),
			Remark: remark,
		})
	}
	return items
}

// youzidjHasNext 优先看翻页链接与 data-pages，最后再按「整页 24 条」兜底：
// 搜索接口返回的是片段，末页不会给 rel="next"，但会带 data-pages。
func youzidjHasNext(body string, page, count int) bool {
	if reYouzidjNextPage.MatchString(body) || reYouzidjNextPageAlt.MatchString(body) {
		return true
	}
	if m := reYouzidjPageCount.FindStringSubmatch(body); len(m) > 1 {
		if pages, err := strconv.Atoi(m[1]); err == nil && pages > 0 {
			return page < pages
		}
	}
	return count >= youzidjPageSize
}

func (d *Downloader) fetchYouzidjCategories(ctx context.Context) ([]nativeCategory, error) {
	return append([]nativeCategory{}, youzidjCategories...), nil
}

func (d *Downloader) fetchYouzidjCatalogPage(ctx context.Context, page int, category, query string) ([]Drama, bool, error) {
	if page < 1 || page > youzidjMaxPage {
		return nil, false, errors.New("柚子短剧目录页码无效")
	}
	if !validYouzidjCategory(category) {
		return nil, false, errors.New("柚子短剧内容分类无效")
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) > 40 {
		return nil, false, errors.New("柚子短剧搜索关键词无效")
	}
	site := d.providerBaseURL(sourceYouzidj)
	path := youzidjListPath(category, page)
	if query != "" {
		path = youzidjSearchPath(query, page)
	}
	body, err := d.youzidjGet(ctx, path)
	if err != nil {
		return nil, false, err
	}
	items := parseYouzidjCards(body, site)
	if len(items) == 0 {
		return []Drama{}, false, nil
	}
	return items, youzidjHasNext(body, page, len(items)), nil
}

type youzidjLdItem struct {
	Type             string          `json:"@type"`
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	Image            json.RawMessage `json:"image"`
	ThumbnailURL     json.RawMessage `json:"thumbnailUrl"`
	ContentURL       string          `json:"contentUrl"`
	URL              string          `json:"url"`
	NumberOfEpisodes int             `json:"numberOfEpisodes"`
	Episode          []struct {
		URL           string `json:"url"`
		Name          string `json:"name"`
		EpisodeNumber int    `json:"episodeNumber"`
	} `json:"episode"`
}

func youzidjLdItems(body string) []youzidjLdItem {
	items := make([]youzidjLdItem, 0, 2)
	for _, match := range reYouzidjLdJSON.FindAllStringSubmatch(body, -1) {
		var item youzidjLdItem
		if err := json.Unmarshal([]byte(match[1]), &item); err == nil && item.Type != "" {
			items = append(items, item)
		}
	}
	return items
}

func youzidjLdImage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return strings.TrimSpace(single)
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, item := range list {
			if strings.TrimSpace(item) != "" {
				return strings.TrimSpace(item)
			}
		}
	}
	return ""
}

type youzidjEpisodeKey struct {
	season  int
	episode int
}

// youzidjChaptersFromSeries 从系列页里的分集链接还原分集表。站点一季一页，
// 但系列页会把所有季的分集链接平铺出来，所以这里按 (季, 集) 排序后再连续编号，
// 单季剧只显示「第N集」，多季剧带上季号。
func youzidjChaptersFromSeries(body, sourceID, site string) []Chapter {
	_, slug := youzidjSplitSourceID(sourceID)
	if slug == "" {
		return nil
	}
	seen := map[youzidjEpisodeKey]bool{}
	keys := make([]youzidjEpisodeKey, 0, 64)
	seasons := map[int]bool{}
	for _, match := range reYouzidjEpisodeLink.FindAllStringSubmatch(body, -1) {
		if match[1] != slug {
			continue
		}
		season, seasonErr := strconv.Atoi(match[2])
		episode, episodeErr := strconv.Atoi(match[3])
		if seasonErr != nil || episodeErr != nil || season < 1 || episode < 1 {
			continue
		}
		key := youzidjEpisodeKey{season: season, episode: episode}
		if seen[key] || len(keys) >= youzidjMaxEpisodes {
			continue
		}
		seen[key] = true
		seasons[season] = true
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].season != keys[j].season {
			return keys[i].season < keys[j].season
		}
		return keys[i].episode < keys[j].episode
	})
	multiSeason := len(seasons) > 1
	chapters := make([]Chapter, 0, len(keys))
	for index, key := range keys {
		chapterKey := fmt.Sprintf("season-%d-episode-%d", key.season, key.episode)
		title := fmt.Sprintf("第%d集", key.episode)
		if multiSeason {
			title = fmt.Sprintf("第%d季 第%d集", key.season, key.episode)
		}
		chapters = append(chapters, Chapter{
			ID: providerChapterID(sourceYouzidj, sourceID, chapterKey), Source: sourceYouzidj,
			Title: truncate(title, 128), CurrentEpisode: rawEpisode(index + 1),
			VideoURL: "youzidj-play://" + chapterKey,
			PageURL:  site + "/" + sourceID + "/" + chapterKey + "/",
			Referer:  site + "/",
		})
	}
	return chapters
}

func youzidjChaptersFromSeason(item youzidjLdItem, sourceID, site string) []Chapter {
	_, slug := youzidjSplitSourceID(sourceID)
	if slug == "" || len(item.Episode) == 0 {
		return nil
	}
	episodes := make([]youzidjEpisodeKey, 0, len(item.Episode))
	for index, episode := range item.Episode {
		number := episode.EpisodeNumber
		if number <= 0 {
			number = index + 1
		}
		episodes = append(episodes, youzidjEpisodeKey{season: 1, episode: number})
	}
	sort.SliceStable(episodes, func(i, j int) bool { return episodes[i].episode < episodes[j].episode })
	chapters := make([]Chapter, 0, len(episodes))
	for index, key := range episodes {
		chapterKey := fmt.Sprintf("season-%d-episode-%d", key.season, key.episode)
		chapters = append(chapters, Chapter{
			ID: providerChapterID(sourceYouzidj, sourceID, chapterKey), Source: sourceYouzidj,
			Title: truncate(fmt.Sprintf("第%d集", key.episode), 128), CurrentEpisode: rawEpisode(index + 1),
			VideoURL: "youzidj-play://" + chapterKey,
			PageURL:  site + "/" + sourceID + "/" + chapterKey + "/",
			Referer:  site + "/",
		})
	}
	return chapters
}

func youzidjTags(body string) []string {
	tags := make([]string, 0, 8)
	seen := map[string]bool{}
	for _, match := range reYouzidjTagLink.FindAllStringSubmatch(body, -1) {
		tag := truncate(youzidjClean(match[2]), 16)
		if tag == "" || seen[tag] || len(tags) >= 8 {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	return tags
}

func (d *Downloader) fetchYouzidjDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	sourceID = strings.Trim(strings.TrimSpace(sourceID), "/")
	if !validYouzidjSourceID(sourceID) {
		return Drama{}, nil, errors.New("柚子短剧剧集标识无效")
	}
	site := d.providerBaseURL(sourceYouzidj)
	kind, _ := youzidjSplitSourceID(sourceID)
	pageURL := site + "/" + sourceID + "/"
	body, err := d.youzidjGet(ctx, pageURL)
	if err != nil && kind == youzidjCategoryDramas {
		// 少数老剧没有系列页，退回第 1 季页，仍然能拿到该季分集。
		if fallback, fallbackErr := d.youzidjGet(ctx, pageURL+"season-1/"); fallbackErr == nil {
			body, pageURL, err = fallback, pageURL+"season-1/", nil
		}
	}
	if err != nil {
		return Drama{}, nil, err
	}

	var series, season, video *youzidjLdItem
	for _, item := range youzidjLdItems(body) {
		switch item.Type {
		case "TVSeries":
			if series == nil {
				copy := item
				series = &copy
			}
		case "TVSeason":
			if season == nil {
				copy := item
				season = &copy
			}
		case "VideoObject", "Movie":
			if video == nil {
				copy := item
				video = &copy
			}
		}
	}

	title, description, cover := "", "", ""
	chapters := make([]Chapter, 0, 32)
	switch {
	case series != nil:
		title = youzidjClean(series.Name)
		description = youzidjClean(series.Description)
		cover = youzidjLdImage(series.Image)
		chapters = youzidjChaptersFromSeries(body, sourceID, site)
		if len(chapters) == 0 && season != nil {
			chapters = youzidjChaptersFromSeason(*season, sourceID, site)
		}
	case season != nil:
		title = youzidjClean(season.Name)
		description = youzidjClean(season.Description)
		cover = youzidjLdImage(season.Image)
		chapters = youzidjChaptersFromSeries(body, sourceID, site)
		if len(chapters) == 0 {
			chapters = youzidjChaptersFromSeason(*season, sourceID, site)
		}
	case video != nil:
		title = youzidjClean(video.Name)
		description = youzidjClean(video.Description)
		cover = youzidjLdImage(video.ThumbnailURL)
	}
	if title == "" {
		if match := reYouzidjOGTitle.FindStringSubmatch(body); len(match) > 1 {
			title = youzidjClean(match[1])
		}
	}
	if cover == "" {
		if match := reYouzidjOGImage.FindStringSubmatch(body); len(match) > 1 {
			cover = youzidjAbsURL(site, match[1])
		}
	}
	if title == "" {
		return Drama{}, nil, errors.New("柚子短剧详情缺少剧集名称")
	}

	// 单片没有分集列表，整页就是播放器，合成 1 集。
	if len(chapters) == 0 && kind != youzidjCategoryDramas {
		chapters = []Chapter{{
			ID: providerChapterID(sourceYouzidj, sourceID, "single"), Source: sourceYouzidj,
			Title: "正片", CurrentEpisode: rawEpisode(1),
			VideoURL: "youzidj-play://single", PageURL: pageURL, Referer: site + "/",
		}}
	}
	if len(chapters) == 0 {
		return Drama{}, nil, errors.New("柚子短剧该剧暂无可用分集")
	}
	cover = youzidjAbsURL(site, cover)

	drama := Drama{
		ID: providerDramaID(sourceYouzidj, sourceID), Source: sourceYouzidj, SourceID: sourceID,
		Title: truncate(title, 512), Name: truncate(title, 512),
		Desc: truncate(description, 12000), Intro: truncate(description, 12000),
		Cover: cover, CoverURL: cover,
		ChannelName: youzidjChannelName, CategoryName: youzidjKindName(kind),
		EpisodeCount: len(chapters), TotalEpisode: len(chapters),
		Remark: fmt.Sprintf("全%d集", len(chapters)),
	}
	if tags := youzidjTags(body); len(tags) > 0 {
		drama.Tags = tags
	}
	return drama, chapters, nil
}

func youzidjPlayerVideoID(body string) string {
	tag := reYouzidjPlayer.FindString(body)
	if tag == "" {
		return ""
	}
	if match := reYouzidjDataVideo.FindStringSubmatch(tag); len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func youzidjCookieValue(cookies []*http.Cookie, name string) string {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

type youzidjPlaybackPayload struct {
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
	Error     string `json:"error"`
}

func (d *Downloader) youzidjPost(ctx context.Context, address, referer, cookie string) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader("{}"))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("User-Agent", youzidjUserAgent)
	if parsed, parseErr := url.Parse(address); parseErr == nil {
		request.Header.Set("Origin", providerMediaOrigin(parsed))
	}
	request.Header.Set("Referer", referer)
	// 站点要求先过入口年龄确认，POST 的搜索/播放接口同样带上门禁 Cookie。
	cookie = youzidjJoinCookies(youzidjAgeCookieText(d.youzidjAge(ctx, d.providerBaseURL(sourceYouzidj), referer)), cookie)
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return nil, nil, readErr
	}
	return response, body, nil
}

// youzidjAccount 取访客账户 Cookie。站点在每次播放前要求有一个访客会话，
// 这里缓存 12 小时，避免每次播放都新建一个访客账户。
func (d *Downloader) youzidjAccount(ctx context.Context, site, referer string) (string, error) {
	d.youzidjGuestMu.Lock()
	cached, cachedAt := d.youzidjGuest, d.youzidjGuestAt
	d.youzidjGuestMu.Unlock()
	if cached != "" && time.Since(cachedAt) < youzidjAccountTTL {
		return cached, nil
	}
	response, body, err := d.youzidjPost(ctx, site+"/api/account/bootstrap", referer, "")
	if err != nil {
		return "", fmt.Errorf("柚子短剧访客会话初始化失败：%w", err)
	}
	if response.StatusCode != http.StatusOK {
		var payload youzidjPlaybackPayload
		_ = json.Unmarshal(body, &payload)
		message := firstNonEmpty(payload.Error, "站点暂不可用")
		return "", fmt.Errorf("柚子短剧访客会话初始化失败：%s", message)
	}
	cookie := youzidjCookieValue(response.Cookies(), youzidjAccountCookie)
	if cookie == "" {
		return "", errors.New("柚子短剧未返回访客凭证，请稍后重试")
	}
	d.youzidjGuestMu.Lock()
	d.youzidjGuest, d.youzidjGuestAt = cookie, time.Now()
	d.youzidjGuestMu.Unlock()
	return cookie, nil
}

func (d *Downloader) youzidjForgetAccount() {
	d.youzidjGuestMu.Lock()
	d.youzidjGuest, d.youzidjGuestAt = "", time.Time{}
	d.youzidjGuestMu.Unlock()
}

// youzidjPlaybackOnce 走一次播放凭证接口，返回 m3u8 地址、需要带上的 Cookie 与过期时间。
func (d *Downloader) youzidjPlaybackOnce(ctx context.Context, site, videoID, referer, account string) (string, string, time.Time, int, error) {
	address := site + "/api/playback/" + url.PathEscape(videoID) + "?mode=mse"
	response, body, err := d.youzidjPost(ctx, address, referer, youzidjAccountCookie+"="+account)
	if err != nil {
		return "", "", time.Time{}, 0, err
	}
	status := response.StatusCode
	var payload youzidjPlaybackPayload
	_ = json.Unmarshal(body, &payload)
	if status != http.StatusOK {
		message := strings.TrimSpace(payload.Error)
		switch {
		case message != "":
			// 站点自己的说明更准确，例如「请先完成网站入口的年龄确认」。
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			message = "播放凭证已失效，请重新播放"
		default:
			message = fmt.Sprintf("站点返回 HTTP %d", status)
		}
		return "", "", time.Time{}, status, errors.New("柚子短剧播放凭证获取失败：" + message)
	}
	mediaPath := strings.TrimSpace(payload.URL)
	if mediaPath == "" {
		return "", "", time.Time{}, status, errors.New("柚子短剧未返回播放地址")
	}
	mediaURL := mediaPath
	if strings.HasPrefix(mediaPath, "/") {
		mediaURL = site + mediaPath
	} else if !strings.HasPrefix(mediaPath, "http") {
		mediaURL = site + "/" + mediaPath
	}
	expires := time.Unix(payload.ExpiresAt, 0)
	if payload.ExpiresAt <= 0 || expires.Before(time.Now()) {
		expires = time.Now().Add(8 * time.Minute)
	}
	// 主列表、子列表与分片都要带门禁 Cookie，缺了站点会回 403。
	cookie := youzidjJoinCookies(youzidjAgeCookieText(d.youzidjAge(ctx, site, referer)), youzidjAccountCookie+"="+account)
	if playCookie := youzidjCookieValue(response.Cookies(), "eden_play_"+videoID); playCookie != "" {
		cookie = youzidjJoinCookies(cookie, "eden_play_"+videoID+"="+playCookie)
	}
	return mediaURL, cookie, expires, status, nil
}

func (d *Downloader) resolveYouzidjMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.DramaID)
	if !valid || source != sourceYouzidj {
		return providerMedia{}, errors.New("柚子短剧播放信息无效，请刷新详情")
	}
	sourceID = strings.Trim(strings.TrimSpace(sourceID), "/")
	if !validYouzidjSourceID(sourceID) {
		return providerMedia{}, errors.New("柚子短剧播放信息无效，请刷新详情")
	}
	prefix := providerChapterID(sourceYouzidj, sourceID, "")
	chapterKey := strings.TrimSpace(strings.TrimPrefix(task.Chapter.ID, prefix))
	if chapterKey == task.Chapter.ID {
		chapterKey = ""
	}
	site := d.providerBaseURL(sourceYouzidj)
	pageURL := strings.TrimSpace(task.Chapter.PageURL)
	if pageURL == "" {
		pageURL = site + "/" + sourceID + "/"
		if chapterKey != "" && chapterKey != "single" {
			pageURL = site + "/" + sourceID + "/" + chapterKey + "/"
		}
	}
	body, err := d.youzidjGet(ctx, pageURL)
	if err != nil {
		return providerMedia{}, err
	}
	videoID := youzidjPlayerVideoID(body)
	if videoID == "" {
		return providerMedia{}, errors.New("柚子短剧该集暂无可播放地址，可能需要在站点解锁")
	}
	account, err := d.youzidjAccount(ctx, site, pageURL)
	if err != nil {
		return providerMedia{}, err
	}
	mediaURL, cookie, expires, status, err := d.youzidjPlaybackOnce(ctx, site, videoID, pageURL, account)
	if err != nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		// 访客会话或年龄确认过期时连同门禁凭证一起重新领取一次，仍然失败就把原错误交给上层。
		d.youzidjForgetAccount()
		d.youzidjForgetAge()
		if fresh, bootErr := d.youzidjAccount(ctx, site, pageURL); bootErr == nil {
			mediaURL, cookie, expires, _, err = d.youzidjPlaybackOnce(ctx, site, videoID, pageURL, fresh)
		}
	}
	if err != nil {
		return providerMedia{}, err
	}
	parsed, parseErr := url.Parse(mediaURL)
	if parseErr != nil || !isProviderHTTPMediaURL(mediaURL) {
		return providerMedia{}, errors.New("柚子短剧返回的播放地址无效")
	}
	media := providerMedia{
		URL: mediaURL, Referer: pageURL,
		credentials: &providerMediaCredentials{
			origin: providerMediaOrigin(parsed), cookie: cookie, referer: pageURL,
			userAgent: youzidjUserAgent, expires: expires,
		},
	}
	return d.prepareWebProviderMedia(ctx, media, youzidjChannelName)
}
