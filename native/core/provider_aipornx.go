package core

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	sourceAipornx = "aipornx"
	aipornxBaseURL = "https://aipornx.cc"
)

var (
	reAipornxCard = regexp.MustCompile(`(?is)<article\b[^>]*class=["'][^"']*\bvod-card\b[^"']*["'][^>]*>(.*?)</article>`)
	reAipornxDetailID = regexp.MustCompile(`(?i)/detail/(\d+)/?`)
	reAipornxPlay = regexp.MustCompile(`(?i)/play/(\d+)-(\d+)-(\d+)/?`)
	reAipornxEpisode = regexp.MustCompile(`(?is)<a\b([^>]*)href=["']([^"']*/play/\d+-\d+-\d+/?)["'][^>]*>(.*?)</a>`)
	reAipornxNext = regexp.MustCompile(`(?i)href=["']([^"']*/page/(\d+)/?)["']`)
)

func aipornxTextByClass(block, class string) string {
	re := regexp.MustCompile(`(?is)<[^>]+class=["'][^"']*\b` + regexp.QuoteMeta(class) + `\b[^"']*["'][^>]*>(.*?)</[^>]+>`)
	match := re.FindStringSubmatch(block)
	if len(match) < 2 {
		return ""
	}
	return cleanText(match[1])
}

func aipornxAttr(block, attr string) string {
	re := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(attr) + `=["']([^"']+)["']`)
	match := re.FindStringSubmatch(block)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(htmlUnescape(match[1]))
}

func htmlUnescape(value string) string {
	return html.UnescapeString(value)
}

func parseAipornxCards(body, pageURL, category string) []Drama {
	seen := map[string]bool{}
	var items []Drama
	for _, card := range reAipornxCard.FindAllStringSubmatch(body, -1) {
		block := card[1]
		link := reAipornxDetailID.FindStringSubmatch(block)
		if len(link) < 2 || seen[link[1]] {
			continue
		}
		id := link[1]
		seen[id] = true
		title := aipornxTextByClass(block, "vod-name")
		if title == "" {
			title = aipornxAttr(block, "alt")
		}
		if title == "" {
			title = aipornxAttr(block, "title")
		}
		if title == "" {
			continue
		}
		cover := aipornxAttr(block, "data-original")
		if cover == "" {
			cover = aipornxAttr(block, "data-src")
		}
		if cover == "" {
			cover = aipornxAttr(block, "src")
		}
		cover = resolveProviderURL(pageURL, cover)
		remark := aipornxTextByClass(block, "vod-remarks")
		items = append(items, Drama{
			ID: providerDramaID(sourceAipornx, id), Source: sourceAipornx, SourceID: id,
			Title: title, Name: title, Cover: cover, CoverURL: cover,
			Remark: remark, CategoryName: category, ChannelName: "aipornx.cc",
		})
	}
	return items
}

func (d *Downloader) fetchAipornxCatalogPage(ctx context.Context, page int, category string) ([]Drama, bool, error) {
	if page < 1 {
		page = 1
	}
	var path string
	switch category {
	case "", "all":
		if page == 1 {
			path = "/home/"
		} else {
			path = fmt.Sprintf("/home/page/%d/", page)
		}
	case "anthurium", "camelia":
		if page == 1 {
			path = "/aidj/" + category + "/"
		} else {
			path = fmt.Sprintf("/aidj/%s/page/%d/", category, page)
		}
	default:
		return nil, false, fmt.Errorf("未知的 AiPorn 分类")
	}
	base := d.providerBaseURL(sourceAipornx)
	address := base + path
	body, err := d.fetchProviderText(ctx, address, base+"/")
	if err != nil {
		return nil, false, err
	}
	name := "首页"
	if category == "anthurium" {
		name = "AI成人短剧"
	} else if category == "camelia" {
		name = "AI成人漫剧"
	}
	items := parseAipornxCards(body, address, name)
	more := false
	for _, match := range reAipornxNext.FindAllStringSubmatch(body, -1) {
		if len(match) > 2 {
			n, _ := strconv.Atoi(match[2])
			if n > page {
				more = true
				break
			}
		}
	}
	return items, more, nil
}

func (d *Downloader) searchAipornx(ctx context.Context, page int, query string) ([]Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 64 {
		return nil, false, fmt.Errorf("搜索关键词无效")
	}
	base := d.providerBaseURL(sourceAipornx)
	address := base + "/search/-------------/?wd=" + url.QueryEscape(query)
	body, err := d.fetchProviderText(ctx, address, base+"/")
	if err != nil {
		return nil, false, err
	}
	return parseAipornxCards(body, address, "搜索"), false, nil
}

func (d *Downloader) fetchAipornxDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" || !regexp.MustCompile(`^\d+$`).MatchString(sourceID) {
		return Drama{}, nil, fmt.Errorf("影片编号无效")
	}
	base := d.providerBaseURL(sourceAipornx)
	address := base + "/detail/" + sourceID + "/"
	body, err := d.fetchProviderText(ctx, address, base+"/")
	if err != nil {
		return Drama{}, nil, err
	}
	title := ""
	if match := reH1Tag.FindStringSubmatch(body); len(match) > 1 {
		title = cleanText(match[1])
	}
	if title == "" {
		if match := reTitleTag.FindStringSubmatch(body); len(match) > 1 {
			title = cleanText(match[1])
		}
	}
	cover := ""
	if match := regexp.MustCompile(`(?is)<[^>]*class=["'][^"']*detail-cover[^"']*["'][^>]*>.*?<img[^>]+(?:src|data-src)=["']([^"']+)["']`).FindStringSubmatch(body); len(match) > 1 {
		cover = resolveProviderURL(address, match[1])
	}
	desc := ""
	if match := regexp.MustCompile(`(?is)<[^>]*class=["'][^"']*detail-summary[^"']*["'][^>]*>(.*?)</[^>]+>`).FindStringSubmatch(body); len(match) > 1 {
		desc = cleanText(match[1])
	}
	drama := Drama{
		ID: providerDramaID(sourceAipornx, sourceID), Source: sourceAipornx, SourceID: sourceID,
		Title: title, Name: title, Desc: desc, Intro: desc, Cover: cover, CoverURL: cover,
		ChannelName: "aipornx.cc",
	}
	seen := map[string]bool{}
	var chapters []Chapter
	for _, match := range reAipornxPlay.FindAllStringSubmatch(body, -1) {
		if len(match) < 4 {
			continue
		}
		videoID, season, episode := match[1], match[2], match[3]
		key := season + "-" + episode
		if seen[key] {
			continue
		}
		seen[key] = true
		n, _ := strconv.Atoi(episode)
		chapterURL := base + "/play/" + videoID + "-" + season + "-" + episode + "/"
		chapters = append(chapters, Chapter{
			ID: providerChapterID(sourceAipornx, sourceID, key), Source: sourceAipornx,
			Title: fmt.Sprintf("第%d集", n), PageURL: chapterURL, Referer: address,
			CurrentEpisode: rawEpisode(n),
		})
	}
	sortProviderChapters(chapters)
	return drama, chapters, nil
}

func (d *Downloader) fetchAipornxPlayURL(ctx context.Context, chapterURL string) (string, error) {
	match := reAipornxPlay.FindStringSubmatch(chapterURL)
	if len(match) < 4 {
		return "", fmt.Errorf("播放页面地址无效")
	}
	base := d.providerBaseURL(sourceAipornx)
	endpoint := fmt.Sprintf("%s/huangguo.php?ac=play&id=%s&ep=%s", base, url.QueryEscape(match[1]), url.QueryEscape(match[3]))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", chapterURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	resp, err := d.doCatalogRequestWithTimeout(req, providerTimeout)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("播放接口返回 HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("解析播放接口失败: %w", err)
	}
	if payload.Code != 1 || payload.URL == "" {
		message := strings.TrimSpace(payload.Msg)
		if len(message) > 100 {
			message = message[:100]
		}
		return "", fmt.Errorf("播放接口未返回有效地址: %s", message)
	}
	mediaURL := resolveProviderURL(base+"/", payload.URL)
	parsed, err := url.Parse(mediaURL)
	if err != nil || parsed.Hostname() != "aipornx.cc" || parsed.Path != "/huangguo.php" || parsed.Query().Get("ac") != "media" {
		return "", fmt.Errorf("播放接口返回了不受支持的媒体地址")
	}
	return mediaURL, nil
}
