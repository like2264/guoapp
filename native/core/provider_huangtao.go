package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 黄桃（aihuangdou.com）：站点是 SSR 列表页 + 同源 JSON 接口的组合。
//
//   - 列表/分类：服务端渲染的 HTML，翻页走路径（/ai-duanju/2/、/tag/anhei/3/），
//     每页 22 部，页脚直接给「第 X/Y 页」。
//   - 搜索：/api/search?q=&page=&size=20，返回 items + pagination.pages（可以一路翻到底）。
//   - 详情/分集：/api/videos/{id} 与 /videos/{id}/episodes（后者不带 /api 前缀）。
//   - 播放：POST /api/videos/{id}/episodes/{n}/playback，body 为 {}，
//     返回 streams.main（m3u8）+ cdn 头信息；实测匿名即可取流，AES-128 标准 HLS。
//
// 所有 /api/* 请求都要经过站点的 edge 桥（POST /api/edge/request，
// 信封里 method/path/body(base64)/content_type），与黄果 AI 同一套机制。
const (
	huangtaoBaseURL        = "https://aihuangdou.com"
	huangtaoDefaultChannel = "ai-duanju"
	huangtaoSearchPageSize = 20
	huangtaoMaxPage        = 500
	huangtaoMaxEpisodes    = 2000
	huangtaoChannelPrefix  = "c:"
	huangtaoTagPrefix      = "t:"
)

var (
	huangtaoChannels = []nativeCategory{
		{ID: huangtaoChannelPrefix + "ai-duanju", Name: "AI 短剧"},
		{ID: huangtaoChannelPrefix + "ai-manju", Name: "AI 漫剧"},
		{ID: huangtaoChannelPrefix + "ai-mogai", Name: "AI 魔改"},
		{ID: huangtaoChannelPrefix + "ai-meinv", Name: "AI 美女"},
	}

	reHuangtaoCard       = regexp.MustCompile(`(?is)<article class="drama-card"[^>]*>(.*?)</article>`)
	reHuangtaoCardID     = regexp.MustCompile(`(?is)href="/drama/(\d+)/"`)
	reHuangtaoCardName   = regexp.MustCompile(`(?is)data-track-item-name="([^"]*)"`)
	reHuangtaoCardAlt    = regexp.MustCompile(`(?is)<img[^>]+alt="《([^》]*)》封面"`)
	reHuangtaoCardCover  = regexp.MustCompile(`(?is)data-src="([^"]+)"`)
	reHuangtaoCardScore  = regexp.MustCompile(`(?is)<em class="card-score">(.*?)</em>`)
	reHuangtaoCardFlag   = regexp.MustCompile(`(?is)<span class="eps-flag">(.*?)</span>`)
	reHuangtaoCardTitle  = regexp.MustCompile(`(?is)<h3[^>]*>(.*?)</h3>`)
	reHuangtaoCardDesc   = regexp.MustCompile(`(?is)<p class="card-desc">(.*?)</p>`)
	reHuangtaoCardMeta   = regexp.MustCompile(`(?is)</h3></a>\s*<p>(.*?)</p>`)
	reHuangtaoPageTotal  = regexp.MustCompile(`第\s*\d+\s*/\s*(\d+)\s*页`)
	reHuangtaoPlayData   = regexp.MustCompile(`(?is)<script id="ninePlayData" type="application/json">(.*?)</script>`)
	reHuangtaoStripTags  = regexp.MustCompile(`(?is)<[^>]+>`)
	reHuangtaoNumericID  = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
	reHuangtaoTextSlug   = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,63}$`)
	reHuangtaoHeatNumber = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?\s*[wW万]?`)
)

func huangtaoClean(raw string) string {
	if raw == "" {
		return ""
	}
	raw = reHuangtaoStripTags.ReplaceAllString(raw, " ")
	raw = html.UnescapeString(raw)
	raw = strings.ReplaceAll(raw, "\u3000", " ")
	return strings.Join(strings.Fields(raw), " ")
}

func validHuangtaoID(sourceID string) bool {
	return reHuangtaoNumericID.MatchString(strings.TrimSpace(sourceID))
}

// huangtaoCategoryParts 把分类标识拆成「频道 / 标签 / 默认」三类，
// 频道与标签都映射到各自的 SSR 列表路径。
func huangtaoCategoryParts(category string) (kind, value string, ok bool) {
	category = strings.TrimSpace(category)
	switch {
	case category == "":
		return "default", "", true
	case strings.HasPrefix(category, huangtaoChannelPrefix):
		value = strings.TrimPrefix(category, huangtaoChannelPrefix)
		for _, channel := range huangtaoChannels {
			if channel.ID == category {
				return "channel", value, true
			}
		}
		return "", "", false
	case strings.HasPrefix(category, huangtaoTagPrefix):
		value = strings.TrimPrefix(category, huangtaoTagPrefix)
		if reHuangtaoTextSlug.MatchString(value) {
			return "tag", value, true
		}
		return "", "", false
	default:
		return "", "", false
	}
}

func validHuangtaoCategory(category string) bool {
	_, _, ok := huangtaoCategoryParts(category)
	return ok
}

func huangtaoListPath(category string, page int) string {
	kind, value, _ := huangtaoCategoryParts(category)
	base := "/" + huangtaoDefaultChannel + "/"
	switch kind {
	case "channel":
		base = "/" + value + "/"
	case "tag":
		base = "/tag/" + value + "/"
	}
	if page > 1 {
		return fmt.Sprintf("%s%d/", base, page)
	}
	return base
}

type huangtaoEnvelope struct {
	Status int             `json:"status"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

// huangtaoAPI 走站点的 edge 桥。payload 非空时按 POST 发送（body 需要 base64）。
func (d *Downloader) huangtaoAPI(ctx context.Context, method, path string, payload any, out any) error {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	envelope := map[string]string{"method": method, "path": path, "body": "", "content_type": ""}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		envelope["body"] = base64.StdEncoding.EncodeToString(raw)
		envelope["content_type"] = "application/json"
	}
	request, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	base := d.providerBaseURL(sourceHuangtao)
	body, err := d.fetchProviderRequest(ctx, base+"/api/edge/request", base+"/", request)
	if err != nil {
		return err
	}
	var reply huangtaoEnvelope
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		return fmt.Errorf("黄桃接口返回格式无效：%w", err)
	}
	if reply.Status != 1 {
		return fmt.Errorf("黄桃接口返回失败：%s", firstNonEmpty(huangtaoClean(reply.Msg), "未知错误"))
	}
	if out == nil || len(reply.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(reply.Data, out); err != nil {
		return fmt.Errorf("黄桃接口数据格式已变化：%w", err)
	}
	return nil
}

func huangtaoCoverURL(site, raw string) string {
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

// huangtaoHeat 从「· 2.0万 热度」里抠出热度数值文本。
func huangtaoHeat(meta string) string {
	meta = strings.TrimSpace(meta)
	if meta == "" {
		return ""
	}
	index := strings.Index(meta, "热度")
	if index < 0 {
		return ""
	}
	segment := meta[:index]
	if cut := strings.LastIndex(segment, "·"); cut >= 0 {
		segment = segment[cut+1:]
	}
	return strings.TrimSpace(reHuangtaoHeatNumber.FindString(segment))
}

func parseHuangtaoCards(body, site string) []Drama {
	items := make([]Drama, 0, 24)
	seen := map[string]bool{}
	for _, match := range reHuangtaoCard.FindAllStringSubmatch(body, -1) {
		chunk := match[1]
		idMatch := reHuangtaoCardID.FindStringSubmatch(chunk)
		if len(idMatch) < 2 || !validHuangtaoID(idMatch[1]) || seen[idMatch[1]] {
			continue
		}
		sourceID := idMatch[1]
		title := ""
		if nameMatch := reHuangtaoCardName.FindStringSubmatch(chunk); len(nameMatch) > 1 {
			title = huangtaoClean(nameMatch[1])
		}
		if title == "" {
			if titleMatch := reHuangtaoCardTitle.FindStringSubmatch(chunk); len(titleMatch) > 1 {
				title = huangtaoClean(titleMatch[1])
			}
		}
		if title == "" {
			if altMatch := reHuangtaoCardAlt.FindStringSubmatch(chunk); len(altMatch) > 1 {
				title = huangtaoClean(altMatch[1])
			}
		}
		if title == "" {
			continue
		}
		seen[sourceID] = true
		cover := ""
		if coverMatch := reHuangtaoCardCover.FindStringSubmatch(chunk); len(coverMatch) > 1 {
			cover = huangtaoCoverURL(site, coverMatch[1])
		}
		remark := ""
		if flagMatch := reHuangtaoCardFlag.FindStringSubmatch(chunk); len(flagMatch) > 1 {
			remark = truncate(huangtaoClean(flagMatch[1]), 64)
		}
		score := ""
		if scoreMatch := reHuangtaoCardScore.FindStringSubmatch(chunk); len(scoreMatch) > 1 {
			// 卡片里是 `<em>8.3<i>分</i></em>`，去标签后会多出一个空格。
			score = truncate(strings.ReplaceAll(huangtaoClean(scoreMatch[1]), " 分", "分"), 16)
		}
		meta := ""
		if metaMatch := reHuangtaoCardMeta.FindStringSubmatch(chunk); len(metaMatch) > 1 {
			meta = huangtaoClean(metaMatch[1])
		}
		description := ""
		if descMatch := reHuangtaoCardDesc.FindStringSubmatch(chunk); len(descMatch) > 1 {
			description = truncate(huangtaoClean(descMatch[1]), 4000)
		}
		category := "AI 短剧"
		// 卡片那段是「AI短剧 · 都市 · 2.0万 热度」：取最后一个既不是频道也不是热度的段。
		if meta != "" {
			segments := strings.Split(meta, "·")
			for index := len(segments) - 1; index >= 0; index-- {
				segment := strings.TrimSpace(segments[index])
				if segment == "" || strings.Contains(segment, "热度") || strings.HasPrefix(segment, "AI") {
					continue
				}
				category = truncate(segment, 24)
				break
			}
		}
		drama := Drama{
			ID: providerDramaID(sourceHuangtao, sourceID), Source: sourceHuangtao, SourceID: sourceID,
			Title: truncate(title, 512), Name: truncate(title, 512),
			Desc: description, Intro: description,
			Cover: cover, CoverURL: cover, ChannelName: "黄桃", CategoryName: category,
			Remark: remark, Score: score, Heat: huangtaoHeat(meta),
		}
		items = append(items, drama)
		if len(items) >= 200 {
			break
		}
	}
	return items
}

// huangtaoTotalPages 读页脚「第 X/Y 页」，比按卡片数推断更准（末页可能恰好排满）。
func huangtaoTotalPages(body string) int {
	match := reHuangtaoPageTotal.FindStringSubmatch(body)
	if len(match) < 2 {
		return 0
	}
	total, err := strconv.Atoi(match[1])
	if err != nil || total < 0 || total > huangtaoMaxPage {
		return 0
	}
	return total
}

func (d *Downloader) fetchHuangtaoCategories(ctx context.Context) ([]nativeCategory, error) {
	var reply struct {
		Categories []struct {
			Name string `json:"name"`
			Tags []struct {
				Slug string `json:"slug"`
				Name string `json:"name"`
			} `json:"tags"`
		} `json:"categories"`
	}
	if err := d.huangtaoAPI(ctx, http.MethodGet, "/api/tags", nil, &reply); err != nil {
		return nil, err
	}
	categories := make([]nativeCategory, 0, 48)
	categories = append(categories, huangtaoChannels...)
	tags := 0
	for _, group := range reply.Categories {
		for _, tag := range group.Tags {
			slug := strings.TrimSpace(tag.Slug)
			if !reHuangtaoTextSlug.MatchString(slug) {
				continue
			}
			name := strings.TrimSpace(tag.Name)
			if group.Name != "" && name != "" {
				name = group.Name + " · " + name
			}
			if name == "" {
				name = slug
			}
			categories = append(categories, nativeCategory{ID: huangtaoTagPrefix + slug, Name: truncate(name, 24)})
			tags++
			if len(categories) >= 96 {
				break
			}
		}
	}
	if tags == 0 && len(reply.Categories) == 0 {
		return nil, errors.New("黄桃暂未返回内容分类")
	}
	return categories, nil
}

func (d *Downloader) fetchHuangtaoCatalogPage(ctx context.Context, page int, category, query string) ([]Drama, bool, error) {
	if page < 1 || page > huangtaoMaxPage {
		return nil, false, errors.New("黄桃目录页码无效")
	}
	if strings.TrimSpace(query) != "" {
		return d.searchHuangtao(ctx, page, query)
	}
	if !validHuangtaoCategory(category) {
		return nil, false, errors.New("黄桃内容分类无效")
	}
	site := d.providerBaseURL(sourceHuangtao)
	address := site + huangtaoListPath(category, page)
	body, err := d.fetchProviderText(ctx, address, site+"/")
	if err != nil {
		return nil, false, err
	}
	items := parseHuangtaoCards(body, site)
	if len(items) == 0 {
		return []Drama{}, false, nil
	}
	totalPages := huangtaoTotalPages(body)
	if totalPages > 0 {
		return items, page < totalPages, nil
	}
	return items, len(items) >= 20, nil
}

func (d *Downloader) searchHuangtao(ctx context.Context, page int, query string) ([]Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 64 {
		return nil, false, errors.New("黄桃搜索关键词无效")
	}
	var reply struct {
		Items      json.RawMessage `json:"items"`
		Pagination struct {
			Pages int `json:"pages"`
			Total int `json:"total"`
		} `json:"pagination"`
	}
	path := "/api/search?" + url.Values{
		"q":    {query},
		"page": {strconv.Itoa(page)},
		"size": {strconv.Itoa(huangtaoSearchPageSize)},
	}.Encode()
	if err := d.huangtaoAPI(ctx, http.MethodGet, path, nil, &reply); err != nil {
		return nil, false, err
	}
	var rows []map[string]any
	if len(reply.Items) > 0 {
		if err := json.Unmarshal(reply.Items, &rows); err != nil {
			return nil, false, fmt.Errorf("黄桃搜索结果格式已变化：%w", err)
		}
	}
	site := d.providerBaseURL(sourceHuangtao)
	items := make([]Drama, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		sourceID := strings.TrimSpace(mapString(row, "id", "video_id"))
		if !validHuangtaoID(sourceID) || seen[sourceID] {
			continue
		}
		title := truncate(huangtaoClean(mapString(row, "title", "name")), 512)
		if title == "" {
			continue
		}
		seen[sourceID] = true
		cover := huangtaoCoverURL(site, mapString(row, "cover", "poster"))
		description := truncate(huangtaoClean(mapString(row, "description", "intro")), 4000)
		onlineDate := ""
		if created := strings.TrimSpace(mapString(row, "created_at")); len(created) >= 10 {
			onlineDate = created[:10]
		}
		items = append(items, Drama{
			ID: providerDramaID(sourceHuangtao, sourceID), Source: sourceHuangtao, SourceID: sourceID,
			Title: title, Name: title, Desc: description, Intro: description,
			Cover: cover, CoverURL: cover, ChannelName: "黄桃", CategoryName: "AI 短剧",
			Heat: mapString(row, "hot"), Views: normalizeViews(mapString(row, "play_count")),
			Tags: mapStringSlice(row, "tags"), OnlineDate: onlineDate,
		})
	}
	hasMore := page < reply.Pagination.Pages
	return items, hasMore, nil
}

// huangtaoEpisodes 取分集清单：优先用 /videos/{id}/episodes（JSON），
// 失败再退回详情页内嵌的 #ninePlayData（同一份数据的 SSR 副本）。
func (d *Downloader) huangtaoEpisodes(ctx context.Context, sourceID string) (int, []Chapter, error) {
	site := d.providerBaseURL(sourceHuangtao)
	var reply struct {
		EpTotal int `json:"epTotal"`
		Eps     []struct {
			N       int    `json:"n"`
			Title   string `json:"title"`
			Heading string `json:"heading"`
			Fact    string `json:"fact"`
		} `json:"eps"`
	}
	body, err := d.fetchProviderText(ctx, site+"/videos/"+url.PathEscape(sourceID)+"/episodes", site+"/")
	if err == nil {
		var envelope huangtaoEnvelope
		if json.Unmarshal([]byte(body), &envelope) == nil && len(envelope.Data) > 0 {
			_ = json.Unmarshal(envelope.Data, &reply)
		}
	}
	total := reply.EpTotal
	chapters := make([]Chapter, 0, max(total, len(reply.Eps)))
	for index, episode := range reply.Eps {
		number := episode.N
		if number < 1 || number > huangtaoMaxEpisodes {
			number = index + 1
		}
		title := truncate(huangtaoClean(firstNonEmpty(episode.Title, episode.Fact, episode.Heading)), 128)
		if title == "" {
			title = fmt.Sprintf("第%d集", number)
		}
		chapters = append(chapters, Chapter{
			ID: providerChapterID(sourceHuangtao, sourceID, strconv.Itoa(number)), Source: sourceHuangtao,
			Title: title, CurrentEpisode: rawEpisode(number),
			VideoURL: "huangtao-play://" + strconv.Itoa(number),
			PageURL:  fmt.Sprintf("%s/video/%s/%d/", site, sourceID, number),
			Referer:  site + "/",
		})
	}
	if len(chapters) == 0 && total > 0 && total <= huangtaoMaxEpisodes {
		for number := 1; number <= total; number++ {
			chapters = append(chapters, Chapter{
				ID: providerChapterID(sourceHuangtao, sourceID, strconv.Itoa(number)), Source: sourceHuangtao,
				Title: fmt.Sprintf("第%d集", number), CurrentEpisode: rawEpisode(number),
				VideoURL: "huangtao-play://" + strconv.Itoa(number),
				PageURL:  fmt.Sprintf("%s/video/%s/%d/", site, sourceID, number),
				Referer:  site + "/",
			})
		}
	}
	if len(chapters) == 0 {
		if total = d.huangtaoPageEpisodes(ctx, sourceID, site, &chapters); total < 0 {
			return 0, nil, errors.New("黄桃该剧暂无可播放分集")
		}
	}
	sort.SliceStable(chapters, func(i, j int) bool {
		left, _ := strconv.Atoi(chapters[i].EpisodeString(i + 1))
		right, _ := strconv.Atoi(chapters[j].EpisodeString(j + 1))
		return left < right
	})
	return max(total, len(chapters)), chapters, nil
}

// huangtaoPageEpisodes 兜底：从播放页 #ninePlayData 里读 epTotal / eps。
func (d *Downloader) huangtaoPageEpisodes(ctx context.Context, sourceID, site string, chapters *[]Chapter) int {
	body, err := d.fetchProviderText(ctx, site+"/video/"+url.PathEscape(sourceID)+"/", site+"/")
	if err != nil {
		return -1
	}
	match := reHuangtaoPlayData.FindStringSubmatch(body)
	if len(match) < 2 {
		return -1
	}
	var data struct {
		EpTotal int `json:"epTotal"`
		Eps     []struct {
			N     int    `json:"n"`
			Title string `json:"title"`
			Fact  string `json:"fact"`
		} `json:"eps"`
	}
	if json.Unmarshal([]byte(html.UnescapeString(match[1])), &data) != nil {
		return -1
	}
	collected := make([]Chapter, 0, len(data.Eps))
	for index, episode := range data.Eps {
		number := episode.N
		if number < 1 || number > huangtaoMaxEpisodes {
			number = index + 1
		}
		title := truncate(huangtaoClean(firstNonEmpty(episode.Title, episode.Fact)), 128)
		if title == "" {
			title = fmt.Sprintf("第%d集", number)
		}
		collected = append(collected, Chapter{
			ID: providerChapterID(sourceHuangtao, sourceID, strconv.Itoa(number)), Source: sourceHuangtao,
			Title: title, CurrentEpisode: rawEpisode(number),
			VideoURL: "huangtao-play://" + strconv.Itoa(number),
			PageURL:  fmt.Sprintf("%s/video/%s/%d/", site, sourceID, number),
			Referer:  site + "/",
		})
	}
	if len(collected) == 0 {
		return -1
	}
	*chapters = collected
	return max(data.EpTotal, len(collected))
}

func (d *Downloader) fetchHuangtaoDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	sourceID = strings.TrimSpace(sourceID)
	if !validHuangtaoID(sourceID) {
		return Drama{}, nil, errors.New("黄桃剧集标识无效")
	}
	var row map[string]any
	if err := d.huangtaoAPI(ctx, http.MethodGet, "/api/videos/"+url.PathEscape(sourceID), nil, &row); err != nil {
		return Drama{}, nil, err
	}
	if id := strings.TrimSpace(mapString(row, "id")); id != "" && id != sourceID {
		return Drama{}, nil, errors.New("黄桃详情与请求剧集不符")
	}
	site := d.providerBaseURL(sourceHuangtao)
	title := truncate(huangtaoClean(mapString(row, "title", "name")), 512)
	if title == "" {
		return Drama{}, nil, errors.New("黄桃详情缺少剧集名称")
	}
	description := truncate(huangtaoClean(mapString(row, "description", "intro")), 12000)
	cover := huangtaoCoverURL(site, mapString(row, "cover", "poster"))
	category := "AI 短剧"
	if crumbs, ok := row["breadcrumb"].([]any); ok && len(crumbs) > 0 {
		if first, ok := crumbs[0].(map[string]any); ok {
			if name := strings.TrimSpace(mapString(first, "name")); name != "" {
				category = truncate(name, 32)
			}
		}
	}
	total, chapters, err := d.huangtaoEpisodes(ctx, sourceID)
	if err != nil {
		return Drama{}, nil, err
	}
	drama := Drama{
		ID: providerDramaID(sourceHuangtao, sourceID), Source: sourceHuangtao, SourceID: sourceID,
		Title: title, Name: title, Desc: description, Intro: description,
		Cover: cover, CoverURL: cover, ChannelName: "黄桃", CategoryName: category,
		Tags: mapStringSlice(row, "tags"),
	}
	if created := strings.TrimSpace(mapString(row, "created_at")); len(created) >= 10 {
		drama.OnlineDate = created[:10]
	}
	if counts, ok := row["counts"].(map[string]any); ok {
		drama.Heat = mapString(counts, "hot")
		drama.Views = normalizeViews(firstNonEmpty(mapString(counts, "play", "view"), mapString(counts, "hot")))
	}
	drama.EpisodeCount, drama.TotalEpisode = total, total
	if total > 0 {
		drama.Remark = "全 " + strconv.Itoa(total) + " 集"
	}
	return drama, uniqueChapters(chapters), nil
}

func (d *Downloader) resolveHuangtaoMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.DramaID)
	prefix := providerChapterID(sourceHuangtao, sourceID, "")
	if !valid || source != sourceHuangtao || !validHuangtaoID(sourceID) || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return providerMedia{}, errors.New("黄桃播放分集信息无效，请刷新详情")
	}
	number, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(task.Chapter.ID, prefix)))
	if err != nil || number < 1 || number > huangtaoMaxEpisodes {
		return providerMedia{}, errors.New("黄桃播放分集信息无效，请刷新详情")
	}
	var reply struct {
		ManifestMethod string            `json:"manifest_method"`
		Streams        map[string]string `json:"streams"`
		Src            string            `json:"src"`
	}
	path := fmt.Sprintf("/api/videos/%s/episodes/%d/playback", url.PathEscape(sourceID), number)
	if err := d.huangtaoAPI(ctx, http.MethodPost, path, map[string]any{}, &reply); err != nil {
		return providerMedia{}, err
	}
	if method := strings.TrimSpace(reply.ManifestMethod); method != "" && !strings.EqualFold(method, http.MethodGet) {
		return providerMedia{}, errors.New("黄桃播放接口返回了不支持的播放方式")
	}
	site := d.providerBaseURL(sourceHuangtao)
	pageURL := fmt.Sprintf("%s/video/%s/%d/", site, sourceID, number)
	address := resolveProviderURL(site+"/", strings.TrimSpace(firstNonEmpty(reply.Streams["main"], reply.Src)))
	if !isProviderHTTPMediaURL(address) {
		return providerMedia{}, errors.New("黄桃该集暂无播放地址")
	}
	media := providerMedia{URL: address, Referer: pageURL}
	if hevc := resolveProviderURL(site+"/", strings.TrimSpace(reply.Streams["hevc"])); isProviderHTTPMediaURL(hevc) && hevc != address {
		media.Variants = append(media.Variants, providerMedia{URL: hevc, Referer: pageURL, Quality: 1080})
	}
	return d.prepareWebProviderMedia(ctx, media, "黄桃")
}
