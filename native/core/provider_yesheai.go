package core

import (
	"context"
	"encoding/base64"
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

// 夜社短剧（yesheai.com）适配。
//
// 站点是标准的苹果 CMS 风格站点，没有加密接口：
//
//	列表：/filter/{type}.html，翻页 /filter/{type}/page/{n}.html，每页 30 条
//	搜索：/vod/search/wd/{关键词}.html，翻页 /vod/search/wd/{关键词}/page/{n}.html
//	详情：/detail/{id}.html，集数在 .ys-episode 上，href 形如 /play/{id}/1/{n}.html
//	播放：/play/{id}/1/{ep}.html 里内联 var player_aaaa={...}，encrypt=0 时 url 就是 m3u8 直链
//
// 站点在 /filter/ 下的分类是纯数字 type id，站点导航里给出的顶层分类见
// yesheaiFallbackCategories；首页能抓到导航时以线上为准。
const (
	yesheaiBaseURL   = "https://yesheai.com"
	yesheaiUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	yesheaiPageSize  = 30
	yesheaiMaxPage   = 10000
	// yesheaiDefaultType 是「全部」落到的分类：站点没有跨分类的聚合列表，
	// 这里用体量最大的「短剧」(type 6)，其余分类由用户在分类栏自选。
	yesheaiDefaultType = "6"
	yesheaiChannelName = "夜社"
)

var (
	reYesheaiAnchor     = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	reYesheaiCardCover  = regexp.MustCompile(`(?is)<img[^>]+src="([^"]+)"`)
	reYesheaiCardType   = regexp.MustCompile(`(?is)<div\s+class="type">(.*?)</div>`)
	reYesheaiCardIntro  = regexp.MustCompile(`(?is)<div\s+class="module-poster-item-intro">(.*?)</div>`)
	reYesheaiCardEpText = regexp.MustCompile(`(\d+)\s*集`)
	reYesheaiDetailH1   = regexp.MustCompile(`(?is)class="ys-web-detail__info".*?<h1[^>]*>(.*?)</h1>`)
	reYesheaiDetailPic  = regexp.MustCompile(`(?is)class="ys-web-detail__poster".*?<img[^>]+src="([^"]+)"`)
	reYesheaiDetailCate = regexp.MustCompile(`(?is)class="ys-web-detail__meta ys-web-detail__cate">(.*?)</div>`)
	reYesheaiDetailDesc = regexp.MustCompile(`(?is)<p\s+class="ys-web-detail__desc"[^>]*>(.*?)</p>`)
	reYesheaiEpisode    = regexp.MustCompile(`(?is)data-episode="(\d+)"[^>]*href="/play/(\d+)/1/(\d+)\.html"`)
	reYesheaiEpisodeAlt = regexp.MustCompile(`(?is)href="/play/(\d+)/1/(\d+)\.html"[^>]*data-episode="(\d+)"`)
	reYesheaiPlayer     = regexp.MustCompile(`(?is)var\s+player_aaaa\s*=\s*(\{.*?\})\s*;?\s*</script>`)
	reYesheaiHTMLTag    = regexp.MustCompile(`(?is)<[^>]+>`)
	reYesheaiSpace      = regexp.MustCompile(`\s+`)
	reYesheaiNumericID  = regexp.MustCompile(`^[1-9][0-9]{0,11}$`)
	reYesheaiTypeID     = regexp.MustCompile(`^[1-9][0-9]{0,5}$`)
	reYesheaiNavType    = regexp.MustCompile(`(?is)<a\b[^>]*href="/type/(\d+)\.html"[^>]*>(.*?)</a>`)
	reYesheaiCateTag    = regexp.MustCompile(`(?is)#([^<#]{1,16})`)
)

// yesheaiFallbackCategories 是首页抓不到导航时的兜底分类（取自站点导航）。
var yesheaiFallbackCategories = []nativeCategory{
	{ID: "6", Name: "短剧"},
	{ID: "1", Name: "成人短剧"},
	{ID: "3", Name: "国产短剧"},
	{ID: "4", Name: "海外短剧"},
	{ID: "7", Name: "动漫"},
	{ID: "16", Name: "AI 漫剧"},
	{ID: "20", Name: "网剧"},
	{ID: "21", Name: "电影"},
	{ID: "12", Name: "综艺"},
}

func validYesheaiID(id string) bool {
	return reYesheaiNumericID.MatchString(strings.TrimSpace(id))
}

func validYesheaiCategory(category string) bool {
	category = strings.TrimSpace(category)
	if category == "" {
		return true
	}
	return reYesheaiTypeID.MatchString(category)
}

func yesheaiClean(text string) string {
	text = reYesheaiHTMLTag.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = strings.NewReplacer("\u3000", " ", "\u00a0", " ").Replace(text)
	text = reYesheaiSpace.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

func yesheaiAbsURL(site, raw string) string {
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

func (d *Downloader) yesheaiGet(ctx context.Context, address string) (string, error) {
	site := d.providerBaseURL(sourceYesheai)
	if !strings.HasPrefix(address, "http") {
		address = site + address
	}
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, yesheaiUserAgent)
	return d.fetchProviderText(pageContext, address, site+"/")
}

func yesheaiListPath(category string, page int) string {
	category = strings.TrimSpace(category)
	if !validYesheaiCategory(category) || category == "" {
		category = yesheaiDefaultType
	}
	if page > 1 {
		return fmt.Sprintf("/filter/%s/page/%d.html", category, page)
	}
	return "/filter/" + category + ".html"
}

func yesheaiSearchPath(query string, page int) string {
	escaped := url.PathEscape(strings.TrimSpace(query))
	if page > 1 {
		return fmt.Sprintf("/vod/search/wd/%s/page/%d.html", escaped, page)
	}
	return "/vod/search/wd/" + escaped + ".html"
}

func parseYesheaiCards(body, site string) []Drama {
	items := make([]Drama, 0, yesheaiPageSize)
	seen := map[string]bool{}
	for _, match := range reYesheaiAnchor.FindAllStringSubmatch(body, -1) {
		attrs, chunk := match[1], match[2]
		if !strings.Contains(attrs, "module-poster-item") {
			continue
		}
		href := extractAttr("<a "+attrs+">", "href")
		if !strings.HasPrefix(href, "/detail/") || !strings.HasSuffix(href, ".html") {
			continue
		}
		sourceID := strings.TrimSuffix(strings.TrimPrefix(href, "/detail/"), ".html")
		if !validYesheaiID(sourceID) || seen[sourceID] {
			continue
		}
		title := yesheaiClean(extractAttr("<a "+attrs+">", "title"))
		if title == "" {
			title = yesheaiClean(extractByClassText(chunk, "module-poster-item-title"))
		}
		if title == "" {
			if m := reYesheaiCardCover.FindStringSubmatch(chunk); len(m) > 1 {
				title = yesheaiClean(extractAttr(m[0], "alt"))
			}
		}
		if title == "" {
			continue
		}
		seen[sourceID] = true
		cover := ""
		if m := reYesheaiCardCover.FindStringSubmatch(chunk); len(m) > 1 {
			cover = yesheaiAbsURL(site, m[1])
		}
		categoryName := ""
		if m := reYesheaiCardType.FindStringSubmatch(chunk); len(m) > 1 {
			categoryName = truncate(yesheaiClean(m[1]), 24)
		}
		remark := ""
		if m := reYesheaiCardEpText.FindStringSubmatch(chunk); len(m) > 1 {
			remark = "更新至" + m[1] + "集"
		}
		if remark == "" {
			if m := reYesheaiCardIntro.FindStringSubmatch(chunk); len(m) > 1 {
				remark = truncate(yesheaiClean(m[1]), 60)
			}
		}
		if categoryName == "" {
			categoryName = yesheaiChannelName
		}
		drama := Drama{
			ID: providerDramaID(sourceYesheai, sourceID), Source: sourceYesheai, SourceID: sourceID,
			Title: truncate(title, 512), Name: truncate(title, 512),
			Cover: cover, CoverURL: cover,
			ChannelName: yesheaiChannelName, CategoryName: categoryName,
			Remark: remark,
		}
		items = append(items, drama)
		if len(items) >= 240 {
			break
		}
	}
	return items
}

func parseYesheaiNavCategories(body string) []nativeCategory {
	categories := make([]nativeCategory, 0, 12)
	seen := map[string]bool{}
	for _, match := range reYesheaiNavType.FindAllStringSubmatch(body, -1) {
		id, name := match[1], truncate(yesheaiClean(match[2]), 16)
		if !validYesheaiCategory(id) || name == "" || seen[id] || len([]rune(name)) > 12 {
			continue
		}
		if name == "更多" || strings.Contains(name, "更多") {
			continue
		}
		seen[id] = true
		categories = append(categories, nativeCategory{ID: id, Name: name})
	}
	return categories
}

func (d *Downloader) fetchYesheaiCategories(ctx context.Context) ([]nativeCategory, error) {
	body, err := d.yesheaiGet(ctx, "/")
	if err != nil {
		return append([]nativeCategory{}, yesheaiFallbackCategories...), nil
	}
	categories := parseYesheaiNavCategories(body)
	if len(categories) == 0 {
		return append([]nativeCategory{}, yesheaiFallbackCategories...), nil
	}
	return categories, nil
}

func (d *Downloader) fetchYesheaiCatalogPage(ctx context.Context, page int, category, query string) ([]Drama, bool, error) {
	if page < 1 || page > yesheaiMaxPage {
		return nil, false, errors.New("夜社目录页码无效")
	}
	if len([]rune(query)) > 40 {
		return nil, false, errors.New("夜社搜索关键词无效")
	}
	if !validYesheaiCategory(category) {
		return nil, false, errors.New("夜社内容分类无效")
	}
	site := d.providerBaseURL(sourceYesheai)
	query = strings.TrimSpace(query)
	path := ""
	if query != "" {
		path = yesheaiSearchPath(query, page)
	} else {
		path = yesheaiListPath(category, page)
	}
	body, err := d.yesheaiGet(ctx, path)
	if err != nil {
		return nil, false, err
	}
	items := parseYesheaiCards(body, site)
	if len(items) == 0 {
		return []Drama{}, false, nil
	}
	// 站点是固定 30 条一页，末页会不足 30 条；直接按条数判断比解析翻页链接更稳。
	return items, len(items) >= yesheaiPageSize, nil
}

func yesheaiChapters(body, sourceID, site string) []Chapter {
	chapters := make([]Chapter, 0, 32)
	seen := map[int]bool{}
	add := func(dramaID, episode string) {
		number, err := strconv.Atoi(episode)
		if err != nil || number < 1 || number > yesheaiMaxPage || seen[number] {
			return
		}
		if dramaID != sourceID {
			return
		}
		seen[number] = true
		chapters = append(chapters, Chapter{
			ID: providerChapterID(sourceYesheai, sourceID, episode), Source: sourceYesheai,
			Title: truncate(fmt.Sprintf("第%d集", number), 128), CurrentEpisode: rawEpisode(number),
			VideoURL: "yesheai-play://" + episode,
			PageURL:  fmt.Sprintf("%s/play/%s/1/%s.html", site, sourceID, episode),
			Referer:  site + "/",
		})
	}
	for _, match := range reYesheaiEpisode.FindAllStringSubmatch(body, -1) {
		add(match[2], match[3])
	}
	for _, match := range reYesheaiEpisodeAlt.FindAllStringSubmatch(body, -1) {
		add(match[1], match[2])
	}
	sort.SliceStable(chapters, func(i, j int) bool {
		left, _ := strconv.Atoi(strings.TrimSpace(chapters[i].EpisodeString(i + 1)))
		right, _ := strconv.Atoi(strings.TrimSpace(chapters[j].EpisodeString(j + 1)))
		return left < right
	})
	return chapters
}

func (d *Downloader) fetchYesheaiDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	if !validYesheaiID(sourceID) {
		return Drama{}, nil, errors.New("夜社剧集标识无效")
	}
	site := d.providerBaseURL(sourceYesheai)
	body, err := d.yesheaiGet(ctx, "/detail/"+sourceID+".html")
	if err != nil {
		return Drama{}, nil, err
	}
	chapters := yesheaiChapters(body, sourceID, site)
	if len(chapters) == 0 {
		return Drama{}, nil, errors.New("夜社该剧暂无可播放分集")
	}
	title := ""
	if m := reYesheaiDetailH1.FindStringSubmatch(body); len(m) > 1 {
		title = truncate(yesheaiClean(m[1]), 512)
	}
	if title == "" {
		title = truncate(yesheaiClean(extractByClassText(body, "ys-web-detail__ep-title")), 512)
	}
	if title == "" {
		return Drama{}, nil, errors.New("夜社详情缺少剧集名称")
	}
	cover := ""
	if m := reYesheaiDetailPic.FindStringSubmatch(body); len(m) > 1 {
		cover = yesheaiAbsURL(site, m[1])
	}
	description := ""
	if m := reYesheaiDetailDesc.FindStringSubmatch(body); len(m) > 1 {
		description = yesheaiClean(m[1])
	}
	categoryName := ""
	tags := make([]string, 0, 8)
	seenTag := map[string]bool{}
	if m := reYesheaiDetailCate.FindStringSubmatch(body); len(m) > 1 {
		for _, tagMatch := range reYesheaiCateTag.FindAllStringSubmatch(yesheaiClean(m[1]), -1) {
			tag := truncate(strings.TrimSpace(tagMatch[1]), 16)
			if tag == "" || seenTag[tag] || len(tags) >= 8 {
				continue
			}
			seenTag[tag] = true
			tags = append(tags, tag)
			if categoryName == "" {
				categoryName = tag
			}
		}
	}
	if categoryName == "" {
		categoryName = yesheaiChannelName
	}
	drama := Drama{
		ID: providerDramaID(sourceYesheai, sourceID), Source: sourceYesheai, SourceID: sourceID,
		Title: title, Name: title,
		Desc: truncate(description, 12000), Intro: truncate(description, 12000),
		Cover: cover, CoverURL: cover,
		ChannelName: yesheaiChannelName, CategoryName: categoryName,
	}
	if len(tags) > 0 {
		drama.Tags = tags
	}
	drama.EpisodeCount, drama.TotalEpisode = len(chapters), len(chapters)
	drama.Remark = fmt.Sprintf("全%d集", len(chapters))
	return drama, chapters, nil
}

// yesheaiDecodePlayerURL 兼容苹果 CMS 播放器的三种 encrypt 形态：
// 0 直接是地址；1 是 base64；2 是 base64 后再整体倒序。
func yesheaiDecodePlayerURL(raw string, encrypt int) string {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" || encrypt == 0 {
		return raw
	}
	if encrypt == 2 {
		runes := []rune(raw)
		for left, right := 0, len(runes)-1; left < right; left, right = left+1, right-1 {
			runes[left], runes[right] = runes[right], runes[left]
		}
		raw = string(runes)
	}
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		text := strings.TrimSpace(string(decoded))
		if unescaped, err := url.QueryUnescape(text); err == nil {
			text = unescaped
		}
		return text
	}
	return raw
}

func (d *Downloader) resolveYesheaiMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.DramaID)
	prefix := providerChapterID(sourceYesheai, sourceID, "")
	if !valid || source != sourceYesheai || !validYesheaiID(sourceID) || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return providerMedia{}, errors.New("夜社播放分集信息无效，请刷新详情")
	}
	episode := strings.TrimSpace(strings.TrimPrefix(task.Chapter.ID, prefix))
	number, err := strconv.Atoi(episode)
	if err != nil || number < 1 || number > yesheaiMaxPage {
		return providerMedia{}, errors.New("夜社播放分集信息无效，请刷新详情")
	}
	site := d.providerBaseURL(sourceYesheai)
	pageURL := fmt.Sprintf("%s/play/%s/1/%d.html", site, sourceID, number)
	body, err := d.yesheaiGet(ctx, pageURL)
	if err != nil {
		return providerMedia{}, err
	}
	match := reYesheaiPlayer.FindStringSubmatch(body)
	if len(match) < 2 {
		return providerMedia{}, errors.New("夜社该集暂无可播放地址，可能需要在站点开通会员")
	}
	var player struct {
		URL     string `json:"url"`
		URLNext string `json:"url_next"`
		Encrypt int    `json:"encrypt"`
		From    string `json:"from"`
	}
	if err := json.Unmarshal([]byte(match[1]), &player); err != nil {
		return providerMedia{}, errors.New("夜社播放信息解析失败，请刷新后重试")
	}
	address := yesheaiDecodePlayerURL(player.URL, player.Encrypt)
	if address == "" || address == "null" {
		address = yesheaiDecodePlayerURL(player.URLNext, player.Encrypt)
	}
	address = yesheaiAbsURL(site, address)
	if !isProviderHTTPMediaURL(address) {
		return providerMedia{}, errors.New("夜社该集暂无可播放地址，可能需要在站点开通会员")
	}
	return d.prepareWebProviderMedia(ctx, providerMedia{URL: address, Referer: pageURL}, yesheaiChannelName)
}
