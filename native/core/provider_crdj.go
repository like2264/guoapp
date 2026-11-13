package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 91 短剧（91crdjai.com）适配。
//
// 站点是 SSR 页面 + 同源 JSON 接口的组合，没有加密、不需要 edge 桥
// （首页 __cloudfrontCompat.enabled = false，所有 /api/* 直接请求即可）：
//
//	全部：GET /api/videos?page={n}&size=20          → data.items + data.pagination
//	搜索：GET /api/search?q={kw}&page={n}&size=20   → 同上，pagination.pages 可直接判到底
//	分类：GET /{channel}/page/{n}/                   → SSR 卡片，每页 22 条
//	详情：GET /api/videos/{id}                      → 标题/封面/简介/标签
//	分集：GET /duanju/{id}/1/                       → 301 到真实分集页，
//	      页面里 <script id="playInitialData"> 给出完整 eps 数组与 playBase
//	播放：POST /api/videos/{id}/episodes/{n}/playback，body {}，
//	      → data.streams.main（m3u8），另有 hevc / preview
//
// sourceID 用站点的纯数字视频 id。详情路径里的类型前缀与 slug 不需要记住：
// /duanju/{id}/ 会自动 301 到 /{真实类型}/{id}-{slug}/，任意类型前缀都能正确跳转。
const (
	crdjBaseURL      = "https://91crdjai.com"
	crdjUserAgent    = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	crdjChannelName  = "91短剧"
	crdjPageSize     = 20
	crdjHTMLPageSize = 22
	crdjMaxPage      = 10000
	crdjMaxEpisodes  = 2000
)

// 分类取自站点顶部导航；只收可播放的视频类，漫画 / 小说 / 标签榜不在 App 的剧集模型里。
var crdjCategories = []nativeCategory{
	{ID: "duanju", Name: "短剧"},
	{ID: "manju", Name: "漫剧"},
	{ID: "zhenrenju", Name: "真人剧"},
	{ID: "shipin", Name: "视频"},
}

var (
	reCrdjNumericID = regexp.MustCompile(`^[1-9][0-9]{0,11}$`)
	reCrdjCard      = regexp.MustCompile(`(?is)<a\b([^>]*\bclass="[^"]*\bcard\b[^"]*"[^>]*)>(.*?)</a>`)
	reCrdjCardHref  = regexp.MustCompile(`(?is)href="[^"]*/(duanju|manju|zhenrenju|shipin)/(\d+)-[^"]*/"`)
	reCrdjCardCover = regexp.MustCompile(`(?is)data-src="([^"]+)"`)
	reCrdjCardTitle = regexp.MustCompile(`(?is)<h3[^>]*>(.*?)</h3>`)
	reCrdjCardFlag  = regexp.MustCompile(`(?is)<span class="eps-flag">(.*?)</span>`)
	reCrdjCardScore = regexp.MustCompile(`(?is)<span class="score">(.*?)</span>`)
	reCrdjEpCount   = regexp.MustCompile(`(\d+)\s*集`)
	reCrdjScore     = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)
	reCrdjPageLink  = regexp.MustCompile(`(?is)href="[^"]*/page/(\d+)/"`)
	reCrdjPlayData  = regexp.MustCompile(`(?is)<script[^>]*id="playInitialData"[^>]*>(.*?)</script>`)
	reCrdjEpGrid    = regexp.MustCompile(`(?is)<div class="ep-grid"[^>]*>(.*?)</div>`)
	reCrdjEpLink    = regexp.MustCompile(`(?is)href="([^"]*/(\d+)/)"`)
	reCrdjHTMLTag   = regexp.MustCompile(`(?is)<[^>]+>`)
	reCrdjSpace     = regexp.MustCompile(`\s+`)
)

func validCrdjID(id string) bool {
	return reCrdjNumericID.MatchString(strings.TrimSpace(id))
}

func validCrdjCategory(category string) bool {
	category = strings.TrimSpace(category)
	if category == "" {
		return true
	}
	for _, entry := range crdjCategories {
		if entry.ID == category {
			return true
		}
	}
	return false
}

func crdjClean(text string) string {
	text = reCrdjHTMLTag.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = strings.NewReplacer("\u3000", " ", "\u00a0", " ").Replace(text)
	return strings.TrimSpace(reCrdjSpace.ReplaceAllString(text, " "))
}

func crdjAbsURL(site, raw string) string {
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

func crdjTags(raw []string) []string {
	tags := make([]string, 0, 8)
	seen := map[string]bool{}
	for _, value := range raw {
		tag := truncate(crdjClean(value), 16)
		if tag == "" || seen[tag] || len(tags) >= 8 {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	return tags
}

func (d *Downloader) crdjGet(ctx context.Context, address string) (string, error) {
	site := d.providerBaseURL(sourceCrdj)
	if !strings.HasPrefix(address, "http") {
		address = site + address
	}
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, crdjUserAgent)
	return d.fetchProviderText(pageContext, address, site+"/")
}

func (d *Downloader) crdjPost(ctx context.Context, address string, payload []byte) (string, error) {
	site := d.providerBaseURL(sourceCrdj)
	if !strings.HasPrefix(address, "http") {
		address = site + address
	}
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, crdjUserAgent)
	return d.fetchProviderRequest(pageContext, address, site+"/", payload)
}

type crdjEnvelope struct {
	Status int             `json:"status"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

type crdjItem struct {
	ID    any      `json:"id"`
	Title string   `json:"title"`
	Cover string   `json:"cover"`
	Tags  []string `json:"tags"`
}

type crdjListData struct {
	Items      []crdjItem `json:"items"`
	Pagination struct {
		Page  int `json:"page"`
		Pages int `json:"pages"`
		Size  int `json:"size"`
		Total int `json:"total"`
	} `json:"pagination"`
}

type crdjDetailData struct {
	Title       string   `json:"title"`
	Cover       string   `json:"cover"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Breadcrumb  []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"breadcrumb"`
}

type crdjPlayData struct {
	Title       string `json:"title"`
	Poster      string `json:"poster"`
	Description string `json:"description"`
	PlayBase    string `json:"playBase"`
	DetailURL   string `json:"detailURL"`
	Eps         []struct {
		N     int    `json:"n"`
		Title string `json:"title"`
	} `json:"eps"`
}

func crdjListPath(category string, page int) string {
	if page > 1 {
		return fmt.Sprintf("/%s/page/%d/", category, page)
	}
	return "/" + category + "/"
}

func crdjAllPath(page int) string {
	return fmt.Sprintf("/api/videos?page=%d&size=%d", page, crdjPageSize)
}

func crdjSearchPath(query string, page int) string {
	return fmt.Sprintf("/api/search?q=%s&page=%d&size=%d", url.QueryEscape(strings.TrimSpace(query)), page, crdjPageSize)
}

func parseCrdjItems(raw json.RawMessage, site, category string) []Drama {
	var data crdjListData
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return nil
	}
	items := make([]Drama, 0, len(data.Items))
	seen := map[string]bool{}
	for _, entry := range data.Items {
		id := nativeText(entry.ID)
		title := truncate(crdjClean(entry.Title), 512)
		if !validCrdjID(id) || title == "" || seen[id] {
			continue
		}
		seen[id] = true
		cover := crdjAbsURL(site, entry.Cover)
		drama := Drama{
			ID: providerDramaID(sourceCrdj, id), Source: sourceCrdj, SourceID: id,
			Title: title, Name: title, Cover: cover, CoverURL: cover,
			ChannelName: crdjChannelName, CategoryName: firstNonEmpty(category, crdjChannelName),
		}
		if tags := crdjTags(entry.Tags); len(tags) > 0 {
			drama.Tags = tags
		}
		items = append(items, drama)
	}
	return items
}

func crdjLastPage(body string) int {
	last := 0
	for _, match := range reCrdjPageLink.FindAllStringSubmatch(body, -1) {
		if page, err := strconv.Atoi(match[1]); err == nil && page > last && page <= crdjMaxPage {
			last = page
		}
	}
	return last
}

func parseCrdjCards(body, site, category string) []Drama {
	items := make([]Drama, 0, crdjHTMLPageSize)
	seen := map[string]bool{}
	for _, card := range reCrdjCard.FindAllStringSubmatch(body, -1) {
		href := reCrdjCardHref.FindStringSubmatch(card[1])
		if len(href) < 3 || !validCrdjID(href[2]) || seen[href[2]] {
			continue
		}
		id := href[2]
		title := ""
		if match := reCrdjCardTitle.FindStringSubmatch(card[2]); len(match) > 1 {
			title = truncate(crdjClean(match[1]), 512)
		}
		if title == "" {
			continue
		}
		seen[id] = true
		cover := ""
		if match := reCrdjCardCover.FindStringSubmatch(card[2]); len(match) > 1 {
			cover = crdjAbsURL(site, match[1])
		}
		drama := Drama{
			ID: providerDramaID(sourceCrdj, id), Source: sourceCrdj, SourceID: id,
			Title: title, Name: title, Cover: cover, CoverURL: cover,
			ChannelName: crdjChannelName, CategoryName: firstNonEmpty(category, crdjChannelName),
		}
		if match := reCrdjCardFlag.FindStringSubmatch(card[2]); len(match) > 1 {
			if count := reCrdjEpCount.FindStringSubmatch(crdjClean(match[1])); len(count) > 1 {
				if value, err := strconv.Atoi(count[1]); err == nil && value > 0 {
					drama.EpisodeCount, drama.TotalEpisode = value, value
				}
			}
		}
		if match := reCrdjCardScore.FindStringSubmatch(card[2]); len(match) > 1 {
			drama.Score = firstNonEmpty(reCrdjScore.FindString(crdjClean(match[1])), "")
		}
		items = append(items, drama)
	}
	return items
}

func (d *Downloader) fetchCrdjCategories(ctx context.Context) ([]nativeCategory, error) {
	return append([]nativeCategory{}, crdjCategories...), nil
}

func (d *Downloader) fetchCrdjCatalogPage(ctx context.Context, page int, category, query string) ([]Drama, bool, error) {
	if page < 1 || page > crdjMaxPage {
		return nil, false, errors.New("91短剧目录页码无效")
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) > 40 {
		return nil, false, errors.New("91短剧搜索关键词无效")
	}
	category = strings.TrimSpace(category)
	if !validCrdjCategory(category) {
		return nil, false, errors.New("91短剧内容分类无效")
	}
	site := d.providerBaseURL(sourceCrdj)

	// 「全部」与「搜索」都走站点自己的 JSON 接口，分页信息由服务端给出。
	if query != "" || category == "" {
		path := crdjAllPath(page)
		if query != "" {
			path = crdjSearchPath(query, page)
		}
		body, err := d.crdjGet(ctx, path)
		if err != nil {
			return nil, false, err
		}
		var envelope crdjEnvelope
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			return nil, false, fmt.Errorf("91短剧接口返回格式无效：%w", err)
		}
		if envelope.Status != 1 && envelope.Status != 0 {
			return nil, false, fmt.Errorf("91短剧接口返回失败：%s", firstNonEmpty(crdjClean(envelope.Msg), "未知错误"))
		}
		var data crdjListData
		if len(envelope.Data) > 0 {
			_ = json.Unmarshal(envelope.Data, &data)
		}
		items := parseCrdjItems(envelope.Data, site, crdjCategoryName(category))
		if len(items) == 0 {
			return []Drama{}, false, nil
		}
		more := len(items) >= crdjPageSize
		if data.Pagination.Pages > 0 && page >= data.Pagination.Pages {
			more = false
		}
		return items, more, nil
	}

	// 分类列表是 SSR 卡片；/api/videos 会忽略 category 参数，必须走 HTML。
	body, err := d.crdjGet(ctx, crdjListPath(category, page))
	if err != nil {
		return nil, false, err
	}
	items := parseCrdjCards(body, site, crdjCategoryName(category))
	if len(items) == 0 {
		return []Drama{}, false, nil
	}
	more := len(items) >= crdjHTMLPageSize
	if last := crdjLastPage(body); last > 0 && page >= last {
		more = false
	}
	return items, more, nil
}

func crdjCategoryName(category string) string {
	for _, entry := range crdjCategories {
		if entry.ID == category {
			return entry.Name
		}
	}
	return crdjChannelName
}

func crdjChapter(sourceID, site, playBase string, number int) Chapter {
	episode := strconv.Itoa(number)
	base := strings.Trim(strings.TrimSpace(playBase), "/")
	if base == "" || !strings.Contains(base, "/") {
		base = "duanju/" + sourceID
	}
	return Chapter{
		ID: providerChapterID(sourceCrdj, sourceID, episode), Source: sourceCrdj,
		Title: fmt.Sprintf("第%d集", number), CurrentEpisode: rawEpisode(number),
		VideoURL: "crdj-play://" + episode,
		PageURL:  fmt.Sprintf("%s/%s/%s/", site, base, episode),
		Referer:  site + "/",
	}
}

func crdjSortChapters(chapters []Chapter) []Chapter {
	sort.SliceStable(chapters, func(i, j int) bool {
		left, _ := strconv.Atoi(chapters[i].EpisodeString(i + 1))
		right, _ := strconv.Atoi(chapters[j].EpisodeString(j + 1))
		return left < right
	})
	return chapters
}

// crdjPlayChapters 从分集页内嵌的 playInitialData 读完整分集数组，返回分集与真实路径前缀。
func crdjPlayChapters(body, sourceID, site string) ([]Chapter, string, *crdjPlayData) {
	match := reCrdjPlayData.FindStringSubmatch(body)
	if len(match) < 2 {
		return nil, "", nil
	}
	var data crdjPlayData
	if json.Unmarshal([]byte(strings.TrimSpace(match[1])), &data) != nil {
		return nil, "", nil
	}
	chapters := make([]Chapter, 0, len(data.Eps))
	seen := map[int]bool{}
	for index, episode := range data.Eps {
		number := episode.N
		if number < 1 || number > crdjMaxEpisodes {
			number = index + 1
		}
		if seen[number] {
			continue
		}
		seen[number] = true
		chapter := crdjChapter(sourceID, site, data.PlayBase, number)
		if title := truncate(crdjClean(episode.Title), 128); title != "" && !strings.HasPrefix(title, "第") {
			chapter.Title = title
		}
		chapters = append(chapters, chapter)
	}
	return crdjSortChapters(chapters), data.PlayBase, &data
}

// crdjDetailChapters 兜底：详情页 ep-grid 里是 .../N/ 形式的分集链接。
func crdjDetailChapters(body, sourceID, site string) []Chapter {
	chapters := make([]Chapter, 0, 32)
	seen := map[int]bool{}
	for _, grid := range reCrdjEpGrid.FindAllStringSubmatch(body, -1) {
		for _, link := range reCrdjEpLink.FindAllStringSubmatch(grid[1], -1) {
			number, err := strconv.Atoi(link[2])
			if err != nil || number < 1 || number > crdjMaxEpisodes || seen[number] {
				continue
			}
			seen[number] = true
			chapters = append(chapters, crdjChapter(sourceID, site, "", number))
		}
	}
	return crdjSortChapters(chapters)
}

func (d *Downloader) fetchCrdjDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	if !validCrdjID(sourceID) {
		return Drama{}, nil, errors.New("91短剧剧集标识无效")
	}
	site := d.providerBaseURL(sourceCrdj)

	var meta crdjDetailData
	if body, err := d.crdjGet(ctx, "/api/videos/"+url.PathEscape(sourceID)); err == nil {
		var envelope crdjEnvelope
		if json.Unmarshal([]byte(body), &envelope) == nil && len(envelope.Data) > 0 {
			_ = json.Unmarshal(envelope.Data, &meta)
		}
	}

	var play *crdjPlayData
	var chapters []Chapter
	if body, err := d.crdjGet(ctx, "/duanju/"+url.PathEscape(sourceID)+"/1/"); err == nil {
		chapters, _, play = crdjPlayChapters(body, sourceID, site)
	}
	if len(chapters) == 0 {
		if body, err := d.crdjGet(ctx, "/duanju/"+url.PathEscape(sourceID)+"/"); err == nil {
			chapters = crdjDetailChapters(body, sourceID, site)
		}
	}
	if len(chapters) == 0 {
		// 单片作品：站点偶尔不写分集清单，仍可按第 1 集取流。
		chapters = []Chapter{crdjChapter(sourceID, site, "", 1)}
	}

	title := truncate(crdjClean(meta.Title), 512)
	if title == "" && play != nil {
		title = truncate(crdjClean(play.Title), 512)
	}
	if title == "" {
		return Drama{}, nil, errors.New("91短剧详情缺少剧集名称")
	}
	cover := crdjAbsURL(site, meta.Cover)
	if cover == "" && play != nil {
		cover = crdjAbsURL(site, play.Poster)
	}
	description := crdjClean(meta.Description)
	if description == "" && play != nil {
		description = crdjClean(play.Description)
	}
	categoryName := crdjChannelName
	for _, entry := range meta.Breadcrumb {
		if name := truncate(crdjClean(entry.Name), 16); name != "" {
			categoryName = name
			break
		}
	}
	drama := Drama{
		ID: providerDramaID(sourceCrdj, sourceID), Source: sourceCrdj, SourceID: sourceID,
		Title: title, Name: title,
		Desc: truncate(description, 12000), Intro: truncate(description, 12000),
		Cover: cover, CoverURL: cover,
		ChannelName: crdjChannelName, CategoryName: categoryName,
		EpisodeCount: len(chapters), TotalEpisode: len(chapters),
	}
	if tags := crdjTags(meta.Tags); len(tags) > 0 {
		drama.Tags = tags
	}
	return drama, chapters, nil
}

func (d *Downloader) resolveCrdjMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.DramaID)
	prefix := providerChapterID(sourceCrdj, sourceID, "")
	if !valid || source != sourceCrdj || !validCrdjID(sourceID) || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return providerMedia{}, errors.New("91短剧播放分集信息无效，请刷新详情")
	}
	number, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(task.Chapter.ID, prefix)))
	if err != nil || number < 1 || number > crdjMaxEpisodes {
		return providerMedia{}, errors.New("91短剧播放分集信息无效，请刷新详情")
	}
	var reply struct {
		ManifestMethod string            `json:"manifest_method"`
		Streams        map[string]string `json:"streams"`
		Src            string            `json:"src"`
	}
	path := fmt.Sprintf("/api/videos/%s/episodes/%d/playback", url.PathEscape(sourceID), number)
	body, err := d.crdjPost(ctx, path, []byte("{}"))
	if err != nil {
		return providerMedia{}, err
	}
	var envelope crdjEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return providerMedia{}, fmt.Errorf("91短剧播放接口返回格式无效：%w", err)
	}
	if envelope.Status != 1 {
		return providerMedia{}, fmt.Errorf("91短剧该集暂无播放地址：%s", firstNonEmpty(crdjClean(envelope.Msg), "请稍后重试"))
	}
	if len(envelope.Data) > 0 {
		_ = json.Unmarshal(envelope.Data, &reply)
	}
	if method := strings.TrimSpace(reply.ManifestMethod); method != "" && !strings.EqualFold(method, "GET") {
		return providerMedia{}, errors.New("91短剧播放接口返回了不支持的播放方式")
	}
	site := d.providerBaseURL(sourceCrdj)
	pageURL := task.Chapter.PageURL
	if pageURL == "" {
		pageURL = fmt.Sprintf("%s/duanju/%s/%d/", site, sourceID, number)
	}
	address := crdjAbsURL(site, firstNonEmpty(reply.Streams["main"], reply.Src, reply.Streams["preview"]))
	if !isProviderHTTPMediaURL(address) {
		return providerMedia{}, errors.New("91短剧该集暂无播放地址")
	}
	media := providerMedia{URL: address, Referer: pageURL}
	if hevc := crdjAbsURL(site, reply.Streams["hevc"]); isProviderHTTPMediaURL(hevc) && hevc != address {
		media.Variants = append(media.Variants, providerMedia{URL: hevc, Referer: pageURL, Quality: 1080})
	}
	return d.prepareWebProviderMedia(ctx, media, crdjChannelName)
}
