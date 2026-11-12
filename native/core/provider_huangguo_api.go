package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const huangguoAISearchPageSize = 20

// 黄果站点自 2026-11 改版后取流改为 POST /api/videos/{id}/playback，
// 旧的 GET /play 仍在，但返回的 video_url / video_key / video_iv 全为空。
// 下面按常见请求体逐个探测，命中即用。
var huangguoAIPlaybackBodies = []string{
	`{"ep":%d}`,
	`{"ep_num":%d}`,
	`{"episode":%d}`,
	`{"ep":%d,"page_texts":0}`,
}

var huangguoAIPlaybackURLKeys = []string{"video_url", "play_url", "playback_url", "video_api", "m3u8", "url", "src"}

func (d *Downloader) huangguoAIAPI(ctx context.Context, path string, out any) error {
	return d.huangguoAIRequest(ctx, http.MethodGet, path, "", "", out)
}

// huangguoAIRequest 通过站点自带的 edge 通道转发请求；method 与 body 都写在信封里。
func (d *Downloader) huangguoAIRequest(ctx context.Context, method, path, body, contentType string, out any) error {
	envelope, err := json.Marshal(map[string]string{
		"method":       method,
		"path":         path,
		"body":         body,
		"content_type": contentType,
	})
	if err != nil {
		return err
	}
	base := d.providerBaseURL(sourceHuangguoAI)
	raw, err := d.fetchProviderRequest(ctx, base+"/api/edge/request", base+"/", envelope)
	if err != nil {
		return err
	}
	var reply struct {
		Status int             `json:"status"`
		Msg    string          `json:"msg"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		return fmt.Errorf("黄果接口返回格式无效：%w", err)
	}
	if reply.Status != 1 {
		return fmt.Errorf("黄果接口返回失败：%s", firstNonEmpty(reply.Msg, "未知错误"))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(reply.Data, out); err != nil {
		return fmt.Errorf("黄果接口数据格式已变化：%w", err)
	}
	return nil
}

func (d *Downloader) fetchHuangguoAIDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	sourceID = strings.TrimPrefix(strings.Trim(strings.TrimSpace(sourceID), "/"), "detail/")
	if !rankingSourceID.MatchString(sourceID) {
		return Drama{}, nil, errors.New("无效的黄果剧集 ID")
	}
	var detail map[string]any
	if err := d.huangguoAIAPI(ctx, "/api/videos/"+url.PathEscape(sourceID), &detail); err != nil {
		return Drama{}, nil, err
	}
	base := d.providerBaseURL(sourceHuangguoAI)
	drama, ok := dramaFromAIMap(detail, base+"/", "")
	if !ok || drama.SourceID != sourceID {
		return Drama{}, nil, errors.New("黄果详情与请求剧集不符")
	}
	rows, _ := detail["episodes"].([]any)
	var chapters []Chapter
	for _, value := range rows {
		row, _ := value.(map[string]any)
		episode, err := strconv.Atoi(mapString(row, "ep_num"))
		if err != nil || episode < 1 {
			continue
		}
		chapters = append(chapters, huangguoAIChapter(base, sourceID, episode, mapString(row, "title")))
	}
	if len(rows) == 0 {
		chapters = append(chapters, huangguoAIChapter(base, sourceID, 1, drama.DisplayTitle()))
	}
	sortProviderChapters(chapters)
	return drama, uniqueChapters(chapters), nil
}

func huangguoAIChapter(base, sourceID string, episode int, title string) Chapter {
	key, pageURL := "auto-1", base+"/video/"+sourceID+"/"
	if episode > 1 {
		key = "ep-" + strconv.Itoa(episode)
		pageURL += key + "/"
	}
	return Chapter{ID: providerChapterID(sourceHuangguoAI, sourceID, key), Source: sourceHuangguoAI,
		Title: firstNonEmpty(title, fmt.Sprintf("第%d集", episode)), PageURL: pageURL, CurrentEpisode: rawEpisode(episode)}
}

func (d *Downloader) fetchHuangguoAIPlayURL(ctx context.Context, sourceID string, episode int) (string, error) {
	if !rankingSourceID.MatchString(sourceID) || episode < 1 {
		return "", errors.New("黄果集数无效")
	}
	base := d.providerBaseURL(sourceHuangguoAI)
	escaped := url.PathEscape(sourceID)

	// 1. 旧接口仍可用时优先使用，保持向后兼容。
	var legacy struct {
		VideoURL string `json:"video_url"`
	}
	legacyPath := fmt.Sprintf("/api/videos/%s/play?ep=%d&page_texts=0", escaped, episode)
	if err := d.huangguoAIAPI(ctx, legacyPath, &legacy); err == nil {
		if media := huangguoAIPlaybackMedia(base, legacy.VideoURL); media != "" {
			return media, nil
		}
	}

	// 2. 改版后的 POST 取流，按候选请求体逐个探测。
	endpoint := "/api/videos/" + escaped + "/playback"
	var lastErr error
	for _, pattern := range huangguoAIPlaybackBodies {
		var payload map[string]any
		if err := d.huangguoAIRequest(ctx, http.MethodPost, endpoint, fmt.Sprintf(pattern, episode), "application/json", &payload); err != nil {
			lastErr = err
			continue
		}
		if media := huangguoAIPlaybackMedia(base, huangguoAIPlaybackURL(payload)); media != "" {
			return media, nil
		}
	}
	message := "黄果站点未返回播放地址（站点已改为 POST 取流）"
	if lastErr != nil {
		message += "：" + lastErr.Error()
	}
	return "", errors.New(message)
}

// huangguoAIPlaybackMedia 把接口给出的原始地址归一化成可播放地址；
// 新版接口可能返回不带扩展名的地址，因此允许放宽到任意 http(s) 媒体地址。
func huangguoAIPlaybackMedia(base, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if media := normalizeProviderMediaURL(base+"/", raw); media != "" {
		return media
	}
	if resolved := resolveProviderURL(base+"/", raw); isProviderHTTPMediaURL(resolved) {
		return resolved
	}
	return ""
}

// huangguoAIPlaybackURL 在返回体里递归找出播放地址，先认 .m3u8/.mp4，再放宽到已知字段名。
func huangguoAIPlaybackURL(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	if media := huangguoAIPlaybackFromMap(payload, 0); media != "" {
		return media
	}
	return ""
}

func huangguoAIPlaybackFromMap(row map[string]any, depth int) string {
	if depth > 3 {
		return ""
	}
	for _, key := range huangguoAIPlaybackURLKeys {
		value := strings.TrimSpace(mapString(row, key))
		lower := strings.ToLower(value)
		if strings.Contains(lower, ".m3u8") || strings.Contains(lower, ".mp4") {
			return value
		}
	}
	if streams, ok := row["streams"].(map[string]any); ok {
		for _, key := range []string{"main", "hd", "sd", "url"} {
			if value := strings.TrimSpace(mapString(streams, key)); value != "" {
				return value
			}
		}
	}
	for _, key := range huangguoAIPlaybackURLKeys {
		if value := strings.TrimSpace(mapString(row, key)); isProviderHTTPMediaURL(value) {
			return value
		}
	}
	for _, value := range row {
		if child, ok := value.(map[string]any); ok {
			if media := huangguoAIPlaybackFromMap(child, depth+1); media != "" {
				return media
			}
		}
	}
	return ""
}

func (d *Downloader) searchHuangguoAI(ctx context.Context, page int, query string) ([]Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 256 {
		return nil, false, errors.New("黄果搜索关键词无效")
	}
	var result struct {
		Items      json.RawMessage `json:"items"`
		Pagination struct {
			Pages int `json:"pages"`
		} `json:"pagination"`
	}
	path := "/api/search?" + url.Values{"q": {query}, "page": {strconv.Itoa(page)}, "size": {strconv.Itoa(huangguoAISearchPageSize)}}.Encode()
	if err := d.huangguoAIAPI(ctx, path, &result); err != nil {
		return nil, false, err
	}
	items := parseHuangguoAIJSONCards(result.Items, d.providerBaseURL(sourceHuangguoAI)+"/", "")
	return items, page < result.Pagination.Pages, nil
}

func parseHuangguoRanking(rows []map[string]any, pageURL string) (rankingPage, error) {
	failure := errors.New("黄果未返回有效榜单")
	if len(rows) == 0 || len(rows) > 100 {
		return rankingPage{}, failure
	}
	result := rankingPage{Items: make([]rankingItem, 0, len(rows)), TotalPages: 1}
	seen := map[string]bool{}
	for index, row := range rows {
		row["id"] = row["video_id"]
		drama, ok := dramaFromAIMap(row, pageURL, mapString(row, "cate_name"))
		if !ok || !rankingSourceID.MatchString(drama.SourceID) || seen[drama.SourceID] {
			return rankingPage{}, failure
		}
		seen[drama.SourceID] = true
		rank, err := strconv.Atoi(mapString(row, "rank"))
		if err != nil || rank < 1 {
			rank = index + 1
		}
		result.Items = append(result.Items, rankingItem{Rank: rank, Drama: drama, Metric: huangguoRankingMetric(row)})
	}
	return result, nil
}

func huangguoRankingMetric(row map[string]any) string {
	label := mapString(row, "metric_label")
	value, err := strconv.ParseFloat(mapString(row, "metric_value"), 64)
	if label == "" || err != nil {
		return ""
	}
	if value >= 10000 {
		return label + " " + strconv.FormatFloat(value/10000, 'f', 1, 64) + "万"
	}
	return label + " " + strconv.FormatFloat(value, 'f', -1, 64)
}
