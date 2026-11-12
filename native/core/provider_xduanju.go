package core

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
)

const (
	xduanjuBaseURL    = "https://x-duanju.com"
	xduanjuUserAgent  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	xduanjuPageSize   = 24
	xduanjuLatestPath = "/zuixin"
	xduanjuSearchPath = "/sousuo"
	xduanjuDonePath   = "/wanjie"
	xduanjuMaxPage    = 10000
	// xduanjuDoneCategory 是站点「完结」板块的伪分类，与 /fenlei 下的题材分类走不同路径。
	xduanjuDoneCategory = "wanjie"
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
	return truncate(raw, 512)
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
	if err != nil || !validNativeCoverURL(parsed) {
		return ""
	}
	return parsed.String()
}

func (d *Downloader) xduanjuGet(ctx context.Context, path string) (string, error) {
	site := d.providerBaseURL(sourceXduanju)
	address := path
	if !strings.HasPrefix(address, "http") {
		address = site + path
	}
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, xduanjuUserAgent)
	return d.fetchProviderText(pageContext, address, site+"/")
}

func parseXduanjuCards(body, site string) []Drama {
	items := make([]Drama, 0, xduanjuPageSize)
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
			remark = truncate(xduanjuClean(remarkMatch[1]), 64)
		}
		drama := Drama{
			ID: providerDramaID(sourceXduanju, slug), Source: sourceXduanju, SourceID: slug,
			Title: truncate(title, 512), Name: truncate(title, 512),
			Cover: cover, CoverURL: cover, ChannelName: "X短剧", CategoryName: "短剧",
			Remark: remark,
		}
		if strings.Contains(chunk, "tag-vip") {
			vip := true
			drama.VIP = &vip
		}
		items = append(items, drama)
		if len(items) >= 200 {
			break
		}
	}
	return items
}

func xduanjuListPath(category string, page int) string {
	base := xduanjuLatestPath
	switch {
	case category == xduanjuDoneCategory:
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

func (d *Downloader) fetchXduanjuCategories(ctx context.Context) ([]nativeCategory, error) {
	body, err := d.xduanjuGet(ctx, "/fenlei")
	if err != nil {
		return nil, err
	}
	categories := make([]nativeCategory, 0, 64)
	categories = append(categories, nativeCategory{ID: xduanjuDoneCategory, Name: "完结"})
	seen := map[string]bool{xduanjuDoneCategory: true}
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
		categories = append(categories, nativeCategory{ID: slug, Name: truncate(name, 32)})
		if len(categories) >= 96 {
			break
		}
	}
	if genres == 0 {
		return nil, errors.New("X短剧暂未返回内容分类")
	}
	return categories, nil
}

func (d *Downloader) fetchXduanjuCatalogPage(ctx context.Context, page int, category, query string) ([]Drama, bool, error) {
	if page < 1 || page > xduanjuMaxPage {
		return nil, false, errors.New("X短剧目录页码无效")
	}
	if len([]rune(query)) > 40 {
		return nil, false, errors.New("X短剧搜索关键词无效")
	}
	if strings.TrimSpace(query) != "" {
		return d.searchXduanju(ctx, page, query)
	}
	if !validXduanjuCategory(category) {
		return nil, false, errors.New("X短剧内容分类无效")
	}
	body, err := d.xduanjuGet(ctx, xduanjuListPath(category, page))
	if err != nil {
		return nil, false, err
	}
	items := parseXduanjuCards(body, d.providerBaseURL(sourceXduanju))
	if len(items) == 0 {
		return []Drama{}, false, nil
	}
	return items, xduanjuHasNextPage(body, category, page), nil
}

func (d *Downloader) searchXduanju(ctx context.Context, page int, query string) ([]Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, errors.New("X短剧搜索关键词无效")
	}
	if page > 1 {
		return []Drama{}, false, nil
	}
	body, err := d.xduanjuGet(ctx, xduanjuSearchPath+"?q="+url.QueryEscape(query))
	if err != nil {
		return nil, false, err
	}
	if reXduanjuEmptyState.MatchString(body) {
		return []Drama{}, false, nil
	}
	items := parseXduanjuCards(body, d.providerBaseURL(sourceXduanju))
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

func xduanjuChapters(body, sourceID, site string) []Chapter {
	chapters := make([]Chapter, 0, 32)
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
		chapters = append(chapters, Chapter{
			ID: providerChapterID(sourceXduanju, sourceID, episode), Source: sourceXduanju,
			Title: truncate(title, 128), CurrentEpisode: rawEpisode(number),
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

func (d *Downloader) fetchXduanjuDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	if !validXduanjuSlug(sourceID) {
		return Drama{}, nil, errors.New("X短剧剧集标识无效")
	}
	site := d.providerBaseURL(sourceXduanju)
	body, err := d.xduanjuGet(ctx, "/ju/"+sourceID)
	if err != nil {
		return Drama{}, nil, err
	}
	chapters := xduanjuChapters(body, sourceID, site)
	if len(chapters) == 0 {
		return Drama{}, nil, errors.New("X短剧该剧暂无可播放分集")
	}
	meta := xduanjuEpisodeMeta(body)
	title := truncate(xduanjuClean(nativeText(meta["name"])), 512)
	if title == "" {
		if match := reXduanjuOGTitle.FindStringSubmatch(body); len(match) > 1 {
			title = xduanjuTitle(match[1])
		}
	}
	if title == "" {
		return Drama{}, nil, errors.New("X短剧详情缺少剧集名称")
	}
	cover := xduanjuAbsURL(site, nativeText(meta["image"]))
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
		description = nativeText(meta["description"])
	}
	if description == "" {
		if match := reXduanjuOGDesc.FindStringSubmatch(body); len(match) > 1 {
			description = match[1]
		}
	}
	description = xduanjuClean(description)
	drama := Drama{
		ID: providerDramaID(sourceXduanju, sourceID), Source: sourceXduanju, SourceID: sourceID,
		Title: title, Name: title,
		Desc: truncate(description, 12000), Intro: truncate(description, 12000),
		Cover: cover, CoverURL: cover, ChannelName: "X短剧", CategoryName: "短剧",
	}
	if published := strings.TrimSpace(nativeText(meta["datePublished"])); len(published) >= 10 {
		drama.OnlineDate = published[:10]
	}
	tags := make([]string, 0, 8)
	seenTag := map[string]bool{}
	for _, genre := range mapStringSlice(meta, "genre") {
		genre = truncate(xduanjuClean(genre), 24)
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

func (d *Downloader) resolveXduanjuMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.DramaID)
	prefix := providerChapterID(sourceXduanju, sourceID, "")
	if !valid || source != sourceXduanju || !validXduanjuSlug(sourceID) || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return providerMedia{}, errors.New("X短剧播放分集信息无效，请刷新详情")
	}
	number, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(task.Chapter.ID, prefix)))
	if err != nil || number < 1 || number > xduanjuMaxPage {
		return providerMedia{}, errors.New("X短剧播放分集信息无效，请刷新详情")
	}
	site := d.providerBaseURL(sourceXduanju)
	pageURL := fmt.Sprintf("%s/ju/%s/%d", site, sourceID, number)
	body, err := d.xduanjuGet(ctx, pageURL)
	if err != nil {
		return providerMedia{}, err
	}
	match := reXduanjuPlayer.FindStringSubmatch(body)
	if len(match) < 2 {
		return providerMedia{}, errors.New("X短剧该集暂无播放地址，可能需要在站点开通会员")
	}
	address := xduanjuAbsURL(site, match[1])
	if !isProviderHTTPMediaURL(address) {
		return providerMedia{}, errors.New("X短剧播放地址无效")
	}
	return d.prepareWebProviderMedia(ctx, providerMedia{URL: address, Referer: pageURL}, "X短剧")
}
