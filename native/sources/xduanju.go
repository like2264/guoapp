package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"duanjuapp/native/core"
)

// X短剧（x-duanju.com）：纯 SSR HTML 站，无接口。
//
//   - 列表：/zuixin、/zuixin/{page}、/fenlei/{slug}/{page}、/wanjie/{page}
//   - 分类：/fenlei 页内的 fl-tile 瓦片
//   - 搜索：/sousuo?q=关键词（单页，不出翻页）
//   - 详情：/ju/{slug}，分集在 li[data-n] 里
//   - 播放：/ju/{slug}/{n} 页内 .player 的 data-src
const (
	sourceXduanju      = "xduanju"
	xduanjuBaseURL     = "https://x-duanju.com"
	xduanjuName        = "X短剧"
	xduanjuUserAgent   = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	xduanjuPageSize    = 24
	xduanjuLatestPath  = "/zuixin"
	xduanjuSearchPath  = "/sousuo"
	xduanjuDonePath    = "/wanjie"
	xduanjuMaxPage     = 10000
	xduanjuDoneCat     = "wanjie"
	xduanjuMaxCards    = 200
	xduanjuMaxCategory = 96
)

var (
	reXduanjuCard       = regexp.MustCompile(`(?is)<li class="card"[^>]*>\s*<a href="/ju/([a-z0-9-]+)"[^>]*>(.*?)</li>`)
	reXduanjuCardCover  = regexp.MustCompile(`(?is)<img[^>]+src="([^"]+)"`)
	reXduanjuCardAlt    = regexp.MustCompile(`(?is)<img[^>]+alt="([^"]*)"`)
	reXduanjuCardName   = regexp.MustCompile(`(?is)<span class="name">(.*?)</span>`)
	reXduanjuCardRemark = regexp.MustCompile(`(?is)<em class="eps">(.*?)</em>`)
	reXduanjuTagStrip   = regexp.MustCompile(`(?is)<[^>]+>`)
	reXduanjuTile       = regexp.MustCompile(`(?is)<a class="fl-tile" href="/fenlei/([a-z0-9-]+)"[^>]*>(.*?)</a>`)
	reXduanjuTileName   = regexp.MustCompile(`(?is)<span class="fl-name">(.*?)</span>`)
	reXduanjuJSONLD     = regexp.MustCompile(`(?is)<script[^>]+application/ld\+json[^>]*>(.*?)</script>`)
	reXduanjuSynopsis   = regexp.MustCompile(`(?is)<div class="jd-desc"[^>]*>\s*<p>(.*?)</p>`)
	reXduanjuEpisode    = regexp.MustCompile(`(?is)<li data-n="(\d+)"[^>]*>\s*<a href="/ju/([a-z0-9-]+)/(\d+)"([^>]*)>`)
	reXduanjuTitleAttr  = regexp.MustCompile(`(?is)title="([^"]*)"`)
	reXduanjuPlayer     = regexp.MustCompile(`(?is)<div[^>]*class="[^"]*\bplayer\b[^"]*"[^>]*data-src="([^"]+)"`)
	reXduanjuOGTitle    = regexp.MustCompile(`(?is)<meta[^>]+property="og:title"[^>]+content="([^"]*)"`)
	reXduanjuOGDesc     = regexp.MustCompile(`(?is)<meta[^>]+property="og:description"[^>]+content="([^"]*)"`)
	reXduanjuOGImage    = regexp.MustCompile(`(?is)<meta[^>]+property="og:image"[^>]+content="([^"]*)"`)
	reXduanjuCoverAny   = regexp.MustCompile(`(?is)<img[^>]+src="(/tu/[^"]+)"`)
	reXduanjuSlug       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)
	reXduanjuEmptyState = regexp.MustCompile(`class="ss-empty"`)
)

var xduanjuDescriptor = core.SourceDescriptor{
	ID:            sourceXduanju,
	Name:          xduanjuName,
	Description:   "分类短剧 · 在线搜索",
	BaseURL:       xduanjuBaseURL,
	DefaultOff:    true,
	HostSuffixes:  []string{"x-duanju.com"},
	ValidCategory: validXduanjuCategory,
}

func init() {
	core.Register(xduanjuDescriptor, func(host core.Host) core.Provider {
		return &xduanjuProvider{host: host}
	})
}

type xduanjuProvider struct{ host core.Host }

func (p *xduanjuProvider) Descriptor() core.SourceDescriptor { return xduanjuDescriptor }

func (p *xduanjuProvider) Categories(ctx context.Context, force bool) ([]core.Category, error) {
	return p.fetchCategories(ctx)
}

func (p *xduanjuProvider) CatalogPage(ctx context.Context, page int, category string) ([]core.Drama, bool, error) {
	return p.fetchCatalogPage(ctx, page, category)
}

func (p *xduanjuProvider) Detail(ctx context.Context, sourceID string) (core.Drama, []core.Chapter, error) {
	return p.fetchDetail(ctx, sourceID)
}

func (p *xduanjuProvider) ResolveMedia(ctx context.Context, task core.Task) (core.ProviderMedia, error) {
	return p.resolveMedia(ctx, task)
}

func (p *xduanjuProvider) Search(ctx context.Context, page int, query string) ([]core.Drama, bool, error) {
	return p.search(ctx, page, query)
}

func validXduanjuSlug(slug string) bool {
	return reXduanjuSlug.MatchString(strings.TrimSpace(slug))
}

func validXduanjuCategory(category string) bool {
	category = strings.TrimSpace(category)
	return category == "" || validXduanjuSlug(category)
}

func xduanjuClean(text string) string {
	text = reXduanjuTagStrip.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\u3000", " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	return strings.Join(strings.Fields(text), " ")
}

func xduanjuTitle(raw string) string {
	raw = xduanjuClean(raw)
	if index := strings.Index(raw, " - "); index > 0 {
		raw = strings.TrimSpace(raw[:index])
	}
	return core.Truncate(raw, 512)
}

func xduanjuAbsURL(site, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	} else if strings.HasPrefix(raw, "/") {
		raw = site + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || !core.ValidNativeCoverURL(parsed) {
		return ""
	}
	return parsed.String()
}

func (p *xduanjuProvider) get(ctx context.Context, path string) (string, error) {
	site := p.host.BaseURL(sourceXduanju)
	address := path
	if !strings.HasPrefix(address, "http") {
		address = site + path
	}
	response, err := p.host.FetchText(ctx, core.TextRequest{
		URL:       address,
		Referer:   site + "/",
		UserAgent: xduanjuUserAgent,
	})
	if err != nil {
		return "", err
	}
	return response.Body, nil
}

func parseXduanjuCards(body, site string) []core.Drama {
	items := make([]core.Drama, 0, xduanjuPageSize)
	seen := map[string]bool{}
	for _, match := range reXduanjuCard.FindAllStringSubmatch(body, -1) {
		slug, chunk := match[1], match[2]
		if !validXduanjuSlug(slug) || seen[slug] {
			continue
		}
		title := ""
		if nameMatch := reXduanjuCardName.FindStringSubmatch(chunk); len(nameMatch) > 1 {
			title = xduanjuClean(nameMatch[1])
		}
		if title == "" {
			if altMatch := reXduanjuCardAlt.FindStringSubmatch(chunk); len(altMatch) > 1 {
				title = xduanjuClean(altMatch[1])
			}
		}
		if title == "" {
			continue
		}
		seen[slug] = true
		cover := ""
		if coverMatch := reXduanjuCardCover.FindStringSubmatch(chunk); len(coverMatch) > 1 {
			cover = xduanjuAbsURL(site, coverMatch[1])
		}
		remark := ""
		if remarkMatch := reXduanjuCardRemark.FindStringSubmatch(chunk); len(remarkMatch) > 1 {
			remark = core.Truncate(xduanjuClean(remarkMatch[1]), 64)
		}
		drama := core.Drama{
			ID: core.ProviderDramaID(sourceXduanju, slug), Source: sourceXduanju, SourceID: slug,
			Title: core.Truncate(title, 512), Name: core.Truncate(title, 512),
			Cover: cover, CoverURL: cover, ChannelName: xduanjuName, CategoryName: "短剧",
			Remark: remark,
		}
		if strings.Contains(chunk, "tag-vip") {
			vip := true
			drama.VIP = &vip
		}
		items = append(items, drama)
		if len(items) >= xduanjuMaxCards {
			break
		}
	}
	return items
}

func xduanjuListPath(category string, page int) string {
	base := xduanjuLatestPath
	switch {
	case category == xduanjuDoneCat:
		base = xduanjuDonePath
	case category != "":
		base = "/fenlei/" + category
	}
	if page > 1 {
		return fmt.Sprintf("%s/%d", base, page)
	}
	return base
}

// xduanjuHasNextPage 依据站点自身渲染的翻页链接判断是否还有下一页，
// 比按卡片数量推断更稳（末页可能恰好排满整页）。
func xduanjuHasNextPage(body, category string, page int) bool {
	base := strings.TrimSuffix(xduanjuListPath(category, 1), "/")
	needle := fmt.Sprintf(`%s/%d"`, base, page+1)
	return strings.Contains(body, needle)
}

func (p *xduanjuProvider) fetchCategories(ctx context.Context) ([]core.Category, error) {
	body, err := p.get(ctx, "/fenlei")
	if err != nil {
		return nil, err
	}
	categories := make([]core.Category, 0, 64)
	categories = append(categories, core.Category{ID: xduanjuDoneCat, Name: "完结"})
	seen := map[string]bool{xduanjuDoneCat: true}
	genres := 0
	for _, match := range reXduanjuTile.FindAllStringSubmatch(body, -1) {
		slug := match[1]
		if !validXduanjuSlug(slug) || seen[slug] {
			continue
		}
		name := ""
		if nameMatch := reXduanjuTileName.FindStringSubmatch(match[2]); len(nameMatch) > 1 {
			name = xduanjuClean(strings.Split(nameMatch[1], "<")[0])
		}
		if name == "" {
			name = slug
		}
		seen[slug] = true
		genres++
		categories = append(categories, core.Category{ID: slug, Name: core.Truncate(name, 32)})
		if len(categories) >= xduanjuMaxCategory {
			break
		}
	}
	if genres == 0 {
		return nil, errors.New("X短剧暂未返回内容分类")
	}
	return categories, nil
}

func (p *xduanjuProvider) fetchCatalogPage(ctx context.Context, page int, category string) ([]core.Drama, bool, error) {
	if page < 1 || page > xduanjuMaxPage {
		return nil, false, errors.New("X短剧目录页码无效")
	}
	if !validXduanjuCategory(category) {
		return nil, false, errors.New("X短剧内容分类无效")
	}
	body, err := p.get(ctx, xduanjuListPath(category, page))
	if err != nil {
		return nil, false, err
	}
	items := parseXduanjuCards(body, p.host.BaseURL(sourceXduanju))
	if len(items) == 0 {
		return []core.Drama{}, false, nil
	}
	return items, xduanjuHasNextPage(body, category, page), nil
}

func (p *xduanjuProvider) search(ctx context.Context, page int, query string) ([]core.Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, errors.New("X短剧搜索关键词无效")
	}
	if len([]rune(query)) > 40 {
		return nil, false, errors.New("X短剧搜索关键词无效")
	}
	if page > 1 {
		return []core.Drama{}, false, nil
	}
	body, err := p.get(ctx, xduanjuSearchPath+"?q="+url.QueryEscape(query))
	if err != nil {
		return nil, false, err
	}
	if reXduanjuEmptyState.MatchString(body) {
		return []core.Drama{}, false, nil
	}
	items := parseXduanjuCards(body, p.host.BaseURL(sourceXduanju))
	return items, false, nil
}

func xduanjuEpisodeMeta(body string) map[string]any {
	for _, match := range reXduanjuJSONLD.FindAllStringSubmatch(body, -1) {
		var value map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(match[1])), &value) != nil {
			continue
		}
		if kind, _ := value["@type"].(string); kind == "TVSeries" {
			return value
		}
	}
	return nil
}

func xduanjuChapters(body, sourceID, site string) []core.Chapter {
	chapters := make([]core.Chapter, 0, 32)
	seen := map[int]bool{}
	for _, match := range reXduanjuEpisode.FindAllStringSubmatch(body, -1) {
		number, err := strconv.Atoi(match[3])
		if err != nil || number < 1 || number > xduanjuMaxPage || seen[number] {
			continue
		}
		if match[2] != sourceID {
			continue
		}
		seen[number] = true
		title := ""
		if titleMatch := reXduanjuTitleAttr.FindStringSubmatch(match[4]); len(titleMatch) > 1 {
			title = xduanjuClean(titleMatch[1])
		}
		if title == "" {
			title = fmt.Sprintf("第 %d 集", number)
		}
		episode := strconv.Itoa(number)
		chapters = append(chapters, core.Chapter{
			ID: core.ProviderChapterID(sourceXduanju, sourceID, episode), Source: sourceXduanju,
			Title: core.Truncate(title, 128), CurrentEpisode: core.RawEpisode(number),
			VideoURL: "xduanju-play://" + episode,
			PageURL:  fmt.Sprintf("%s/ju/%s/%s", site, sourceID, episode),
			Referer:  site + "/",
		})
	}
	sort.SliceStable(chapters, func(i, j int) bool {
		left, _ := strconv.Atoi(chapters[i].EpisodeString(i + 1))
		right, _ := strconv.Atoi(chapters[j].EpisodeString(j + 1))
		return left < right
	})
	return chapters
}

func (p *xduanjuProvider) fetchDetail(ctx context.Context, sourceID string) (core.Drama, []core.Chapter, error) {
	if !validXduanjuSlug(sourceID) {
		return core.Drama{}, nil, errors.New("X短剧剧集标识无效")
	}
	site := p.host.BaseURL(sourceXduanju)
	body, err := p.get(ctx, "/ju/"+sourceID)
	if err != nil {
		return core.Drama{}, nil, err
	}
	chapters := xduanjuChapters(body, sourceID, site)
	if len(chapters) == 0 {
		return core.Drama{}, nil, errors.New("X短剧该剧暂无可播放分集")
	}
	meta := xduanjuEpisodeMeta(body)
	title := core.Truncate(xduanjuClean(core.NativeText(meta["name"])), 512)
	if title == "" {
		if match := reXduanjuOGTitle.FindStringSubmatch(body); len(match) > 1 {
			title = xduanjuTitle(match[1])
		}
	}
	if title == "" {
		return core.Drama{}, nil, errors.New("X短剧详情缺少剧集名称")
	}
	cover := xduanjuAbsURL(site, core.NativeText(meta["image"]))
	if cover == "" {
		if match := reXduanjuOGImage.FindStringSubmatch(body); len(match) > 1 {
			cover = xduanjuAbsURL(site, match[1])
		}
	}
	if cover == "" {
		if match := reXduanjuCoverAny.FindStringSubmatch(body); len(match) > 1 {
			cover = xduanjuAbsURL(site, match[1])
		}
	}
	// 详情页 .jd-desc 里的简介比 ld+json 的 description 更完整，优先取它。
	description := ""
	if match := reXduanjuSynopsis.FindStringSubmatch(body); len(match) > 1 {
		description = xduanjuClean(match[1])
	}
	if description == "" {
		description = core.NativeText(meta["description"])
	}
	if description == "" {
		if match := reXduanjuOGDesc.FindStringSubmatch(body); len(match) > 1 {
			description = match[1]
		}
	}
	description = xduanjuClean(description)
	drama := core.Drama{
		ID: core.ProviderDramaID(sourceXduanju, sourceID), Source: sourceXduanju, SourceID: sourceID,
		Title: title, Name: title,
		Desc: core.Truncate(description, 12000), Intro: core.Truncate(description, 12000),
		Cover: cover, CoverURL: cover, ChannelName: xduanjuName, CategoryName: "短剧",
	}
	if published := strings.TrimSpace(core.NativeText(meta["datePublished"])); len(published) >= 10 {
		drama.OnlineDate = published[:10]
	}
	tags := make([]string, 0, 8)
	seenTag := map[string]bool{}
	for _, genre := range core.MapStringSlice(meta, "genre") {
		genre = core.Truncate(xduanjuClean(genre), 24)
		if genre == "" || seenTag[genre] || len(tags) >= 8 {
			continue
		}
		seenTag[genre] = true
		tags = append(tags, genre)
	}
	if len(tags) > 0 {
		drama.Tags = tags
		drama.CategoryName = tags[0]
	}
	drama.EpisodeCount, drama.TotalEpisode = len(chapters), len(chapters)
	return drama, chapters, nil
}

func (p *xduanjuProvider) resolveMedia(ctx context.Context, task core.Task) (core.ProviderMedia, error) {
	source, sourceID, valid := core.SplitProviderDramaID(task.DramaID)
	prefix := core.ProviderChapterID(sourceXduanju, sourceID, "")
	if !valid || source != sourceXduanju || !validXduanjuSlug(sourceID) || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return core.ProviderMedia{}, errors.New("X短剧播放分集信息无效，请刷新详情")
	}
	number, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(task.Chapter.ID, prefix)))
	if err != nil || number < 1 || number > xduanjuMaxPage {
		return core.ProviderMedia{}, errors.New("X短剧播放分集信息无效，请刷新详情")
	}
	site := p.host.BaseURL(sourceXduanju)
	pageURL := fmt.Sprintf("%s/ju/%s/%d", site, sourceID, number)
	body, err := p.get(ctx, pageURL)
	if err != nil {
		return core.ProviderMedia{}, err
	}
	match := reXduanjuPlayer.FindStringSubmatch(body)
	if len(match) < 2 {
		return core.ProviderMedia{}, errors.New("X短剧该集暂无播放地址，可能需要在站点开通会员")
	}
	address := xduanjuAbsURL(site, match[1])
	if !core.IsProviderHTTPMediaURL(address) {
		return core.ProviderMedia{}, errors.New("X短剧播放地址无效")
	}
	return p.host.PrepareWebMedia(ctx, core.ProviderMedia{URL: address, Referer: pageURL}, xduanjuName)
}
