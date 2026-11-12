package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// maskPathShape 只保留路径形状：域名、scheme 去掉，长串字母数字（ID/签名）打码。
func maskPathShape(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "empty"
	}
	kind := "path"
	if strings.HasPrefix(strings.ToLower(raw), "http") {
		kind = "url"
	}
	if index := strings.Index(raw, "://"); index >= 0 {
		raw = raw[index+3:]
	}
	if index := strings.Index(raw, "/"); index >= 0 {
		raw = raw[index:]
	} else {
		raw = "/"
	}
	raw = reProbeAlnumRun.ReplaceAllString(raw, "X")
	if len(raw) > 110 {
		raw = raw[:110]
	}
	return kind + ":" + raw
}

// 只在 HD_PROBE=1 时运行：确认黄桃的播放字段（playback_endpoint）能不能直接取到 m3u8，
// 以及它的分类入口、搜索翻页是否可用。只打印形状、字节数、布尔和 ASCII 错误。
func TestHuangtaoPlaybackProbe(t *testing.T) {
	if os.Getenv("HD_PROBE") != "1" {
		t.Skip("set HD_PROBE=1 to run the huangtao probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	base := "https://aihuangdou.com"
	d.cfg.HuangguoAIURL = base

	var page1 struct {
		Items json.RawMessage `json:"items"`
	}
	if err := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=1&size=20", &page1); err != nil {
		t.Fatalf("search: %v", probeErrText(err))
	}
	items := parseHuangguoAIJSONCards(page1.Items, base+"/", "")
	if len(items) == 0 {
		t.Fatalf("search returned no items")
	}
	sourceID := items[0].SourceID
	t.Logf("[ht] sample id=%s titleRunes=%d cover=%v", sourceID, len([]rune(items[0].Title)), items[0].Cover != "")

	// 第一段：详情里的播放字段。
	var detail json.RawMessage
	if err := d.huangguoAIAPI(ctx, "/api/videos/"+url.PathEscape(sourceID), &detail); err != nil {
		t.Fatalf("detail: %v", probeErrText(err))
	}
	var row map[string]any
	if err := json.Unmarshal(detail, &row); err != nil {
		t.Fatalf("detail unmarshal: %v", probeErrText(err))
	}
	endpoint := mapString(row, "playback_endpoint")
	preview := mapString(row, "preview_url", "preview_id")
	t.Logf("[ht] playback_endpoint shape=%s hasM3U8=%v len=%d",
		maskPathShape(endpoint), strings.Contains(strings.ToLower(endpoint), ".m3u8"), len(endpoint))
	t.Logf("[ht] preview shape=%s hasM3U8=%v len=%d", maskPathShape(preview),
		strings.Contains(strings.ToLower(preview), ".m3u8"), len(preview))
	t.Logf("[ht] duration=%s counts=%s", mapString(row, "duration"), probeShape([]byte(mustJSON(row["counts"]))))

	// 第二段：直接抓 playback_endpoint，看是不是可播的 m3u8。
	for index, candidate := range []string{endpoint, preview} {
		if strings.TrimSpace(candidate) == "" {
			t.Logf("[ht] media[%d] skipped empty", index)
			continue
		}
		address := resolveProviderURL(base+"/", candidate)
		body, mediaErr := d.fetchProviderText(ctx, address, base+"/")
		t.Logf("[ht] media[%d] bytes=%d extm3u=%v segments=%d err=%q",
			index, len(body), strings.Contains(body, "#EXTM3U"),
			strings.Count(body, "#EXTINF"), probeErrText(mediaErr))
		time.Sleep(500 * time.Millisecond)
	}

	// 第三段：搜索能不能翻到第 2 页（全站搜索要靠这个）。
	var page2 struct {
		Items json.RawMessage `json:"items"`
	}
	secondErr := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=2&size=20", &page2)
	second := parseHuangguoAIJSONCards(page2.Items, base+"/", "")
	same := len(second) > 0 && len(items) > 0 && second[0].SourceID == items[0].SourceID
	t.Logf("[ht] search p2 items=%d sameAsP1=%v err=%q", len(second), same, probeErrText(secondErr))
	time.Sleep(500 * time.Millisecond)

	// 第四段：分类/目录入口候选。
	paths := []string{
		"/api/videos?page=1&size=24",
		"/api/videos/recommend",
		"/api/videos/hot?page=1",
		"/api/videos/latest?page=1",
		"/api/feed?page=1",
		"/api/tags",
		"/api/tags/all",
		"/api/videos/categories?page=1",
		"/api/categories?page=1",
		"/api/home/videos?page=1",
	}
	for index, path := range paths {
		var raw json.RawMessage
		callErr := d.huangguoAIAPI(ctx, path, &raw)
		t.Logf("[ht] cat[%02d] %s %s err=%q", index, path, probeShape(raw), probeErrText(callErr))
		time.Sleep(400 * time.Millisecond)
	}
}

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func probeJoinKeys(row map[string]any) string {
	if len(row) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(row))
	for key := range row {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 24 {
		keys = keys[:24]
	}
	return strings.Join(keys, ",")
}

// 只在 HD_PROBE=1 时运行：黄桃的播放地址到底怎么取（走 edge 桥还是直连）、
// 分类表在哪、列表接口要什么参数。只打印字段名、条数、布尔和 ASCII 错误。
func TestHuangtaoMediaProbe(t *testing.T) {
	if os.Getenv("HD_PROBE") != "1" {
		t.Skip("set HD_PROBE=1 to run the huangtao probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	base := "https://aihuangdou.com"
	d.cfg.HuangguoAIURL = base

	var found struct {
		Items json.RawMessage `json:"items"`
	}
	if err := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=1&size=20", &found); err != nil {
		t.Fatalf("search: %v", probeErrText(err))
	}
	cards := parseHuangguoAIJSONCards(found.Items, base+"/", "")
	if len(cards) == 0 {
		t.Fatalf("search returned no items")
	}
	sourceID := cards[0].SourceID
	time.Sleep(400 * time.Millisecond)

	// A 段：播放候选路径，走 edge 桥。
	for index, path := range []string{
		"/api/videos/" + url.PathEscape(sourceID) + "/playback",
		"/api/videos/" + url.PathEscape(sourceID) + "/play?ep=1&page_texts=0",
	} {
		var raw json.RawMessage
		callErr := d.huangguoAIAPI(ctx, path, &raw)
		masked := strings.ReplaceAll(path, sourceID, "{id}")
		t.Logf("[ht2] play[%d] %s %s err=%q", index, masked, probeShape(raw), probeErrText(callErr))
		var row map[string]any
		if json.Unmarshal(raw, &row) == nil {
			t.Logf("[ht2] play[%d] keys=%s", index, probeJoinKeys(row))
			for key, value := range row {
				text, ok := value.(string)
				if !ok || text == "" {
					continue
				}
				if !strings.Contains(text, "/") && !strings.Contains(text, ".") {
					continue
				}
				t.Logf("[ht2] play[%d] field %s shape=%s hasM3U8=%v", index, key,
					maskPathShape(text), strings.Contains(strings.ToLower(text), ".m3u8"))
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// B 段：标签表（/api/tags 返回 categories + hot）。
	var tags map[string]any
	tagsErr := d.huangguoAIAPI(ctx, "/api/tags", &tags)
	categories, _ := tags["categories"].([]any)
	hot, _ := tags["hot"].([]any)
	t.Logf("[ht2] tags categories=%d hot=%d err=%q", len(categories), len(hot), probeErrText(tagsErr))
	var firstKey string
	if len(categories) > 0 {
		row, _ := categories[0].(map[string]any)
		firstKey = mapString(row, "id", "slug", "tag", "code", "name")
		t.Logf("[ht2] cat0 keys=%s idLen=%d", probeJoinKeys(row), len(firstKey))
	}
	if len(hot) > 0 {
		row, _ := hot[0].(map[string]any)
		t.Logf("[ht2] hot0 keys=%s", probeJoinKeys(row))
	}
	time.Sleep(500 * time.Millisecond)

	// C 段：带分类的列表候选。
	if firstKey != "" {
		escaped := url.QueryEscape(firstKey)
		for index, path := range []string{
			"/api/videos?tag=" + escaped + "&page=1&size=24",
			"/api/videos?category=" + escaped + "&page=1&size=24",
			"/api/tags/" + escaped + "/videos?page=1&size=24",
			"/api/videos?tags=" + escaped + "&page=1&size=24",
		} {
			var raw json.RawMessage
			callErr := d.huangguoAIAPI(ctx, path, &raw)
			masked := strings.ReplaceAll(path, escaped, "{cat}")
			t.Logf("[ht2] catlist[%d] %s %s err=%q", index, masked, probeShape(raw), probeErrText(callErr))
			var envelope struct {
				Items json.RawMessage `json:"items"`
			}
			if json.Unmarshal(raw, &envelope) == nil && len(envelope.Items) > 0 {
				t.Logf("[ht2] catlist[%d] cards=%d", index, len(parseHuangguoAIJSONCards(envelope.Items, base+"/", "")))
			}
			time.Sleep(400 * time.Millisecond)
		}
	}

	// D 段：首页/列表接口的形状。
	for index, path := range []string{
		"/api/videos?page=1&size=24",
		"/api/feed?page=1",
		"/api/home/videos?page=1",
		"/api/categories?page=1",
	} {
		var raw json.RawMessage
		callErr := d.huangguoAIAPI(ctx, path, &raw)
		t.Logf("[ht2] list[%d] %s %s err=%q", index, path, probeShape(raw), probeErrText(callErr))
		var row map[string]any
		if json.Unmarshal(raw, &row) == nil {
			t.Logf("[ht2] list[%d] keys=%s", index, probeJoinKeys(row))
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// probeM3U8Shapes 从 HTML/JSON 里捞出所有 m3u8 候选，返回打码后的形状。
func probeM3U8Shapes(body string) []string {
	var shapes []string
	seen := map[string]bool{}
	for _, token := range strings.Split(body, ".m3u8") {
		start := len(token)
		for start > 0 {
			ch := token[start-1]
			if ch == '"' || ch == '\'' || ch == '\\' || ch == ' ' || ch == '\n' || ch == '\t' || ch == '(' || ch == ')' || ch == '<' || ch == '>' || ch == ',' {
				break
			}
			start--
		}
		if start >= len(token) {
			continue
		}
		shape := maskPathShape(token[start:] + ".m3u8")
		if seen[shape] {
			continue
		}
		seen[shape] = true
		shapes = append(shapes, shape)
		if len(shapes) >= 6 {
			break
		}
	}
	return shapes
}

// 只在 HD_PROBE=1 时运行：黄桃的播放地址最后一探——先看详情页 HTML 是否直接嵌 m3u8，
// 再用 edge 桥的 POST 变体打 playback 接口。只打印字节数、布尔、字段名和打码路径。
func TestHuangtaoPageProbe(t *testing.T) {
	if os.Getenv("HD_PROBE") != "1" {
		t.Skip("set HD_PROBE=1 to run the huangtao probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	base := "https://aihuangdou.com"
	d.cfg.HuangguoAIURL = base

	var found struct {
		Items json.RawMessage `json:"items"`
	}
	if err := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=1&size=20", &found); err != nil {
		t.Fatalf("search: %v", probeErrText(err))
	}
	cards := parseHuangguoAIJSONCards(found.Items, base+"/", "")
	if len(cards) == 0 {
		t.Fatalf("search returned no items")
	}
	sourceID := cards[0].SourceID
	time.Sleep(400 * time.Millisecond)

	// A 段：详情里的 breadcrumb 会暴露前端详情页的路径形状。
	var detail map[string]any
	if err := d.huangguoAIAPI(ctx, "/api/videos/"+url.PathEscape(sourceID), &detail); err != nil {
		t.Fatalf("detail: %v", probeErrText(err))
	}
	t.Logf("[ht3] detail keys=%s", probeJoinKeys(detail))
	t.Logf("[ht3] playback_endpoint=%s", maskPathShape(mapString(detail, "playback_endpoint")))
	switch crumbs := detail["breadcrumb"].(type) {
	case []any:
		t.Logf("[ht3] breadcrumb items=%d", len(crumbs))
		for index, value := range crumbs {
			if index > 2 {
				break
			}
			row, _ := value.(map[string]any)
			t.Logf("[ht3] crumb[%d] keys=%s url=%s", index, probeJoinKeys(row), maskPathShape(mapString(row, "url", "href", "link")))
		}
	case map[string]any:
		t.Logf("[ht3] breadcrumb keys=%s", probeJoinKeys(crumbs))
	default:
		t.Logf("[ht3] breadcrumb type=%T", detail["breadcrumb"])
	}
	time.Sleep(400 * time.Millisecond)

	// B 段：详情页 HTML 里有没有直接嵌 m3u8。
	for index, path := range []string{
		"/video/" + url.PathEscape(sourceID),
		"/videos/" + url.PathEscape(sourceID),
		"/v/" + url.PathEscape(sourceID),
		"/watch/" + url.PathEscape(sourceID),
	} {
		body, pageErr := d.fetchProviderText(ctx, base+path, base+"/")
		lower := strings.ToLower(body)
		m3u8 := strings.Count(lower, ".m3u8")
		t.Logf("[ht3] page[%d] %s bytes=%d videoTag=%v m3u8=%d contentUrl=%v ldjson=%v err=%q",
			index, strings.ReplaceAll(path, sourceID, "{id}"), len(body),
			strings.Contains(lower, "<video"), m3u8,
			strings.Contains(lower, "contenturl"), strings.Contains(lower, "application/ld+json"),
			probeErrText(pageErr))
		for _, shape := range probeM3U8Shapes(body) {
			t.Logf("[ht3] page[%d] m3u8shape=%s", index, shape)
		}
		time.Sleep(400 * time.Millisecond)
	}

	// C 段：edge 桥的 POST 变体打 playback。
	for index, envelope := range []map[string]string{
		{"method": "POST", "path": "/api/videos/" + sourceID + "/playback", "body": "{}", "content_type": "application/json"},
		{"method": "GET", "path": "/api/videos/" + sourceID + "/playback?ep=1", "body": "", "content_type": ""},
		{"method": "POST", "path": "/api/videos/" + sourceID + "/play", "body": "{}", "content_type": "application/json"},
	} {
		payload, _ := json.Marshal(envelope)
		raw, callErr := d.fetchProviderRequest(ctx, base+"/api/edge/request", base+"/", payload)
		t.Logf("[ht3] bridge[%d] %s bytes=%d %s err=%q", index, maskPathShape(envelope["path"]),
			len(raw), probeShape([]byte(raw)), probeErrText(callErr))
		for _, shape := range probeM3U8Shapes(raw) {
			t.Logf("[ht3] bridge[%d] m3u8shape=%s", index, shape)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// D 段：分类→标签→视频列表，找黄桃自己的目录接口。
	var tags map[string]any
	if err := d.huangguoAIAPI(ctx, "/api/tags", &tags); err != nil {
		t.Logf("[ht3] tags err=%q", probeErrText(err))
	}
	categories, _ := tags["categories"].([]any)
	var tagID string
	if len(categories) > 0 {
		row, _ := categories[0].(map[string]any)
		t.Logf("[ht3] cat0 code=%s id=%s nameRunes=%d sort=%s",
			mapString(row, "code"), mapString(row, "id"), len([]rune(mapString(row, "name"))), mapString(row, "sort"))
		children, _ := row["tags"].([]any)
		t.Logf("[ht3] cat0 tags=%d", len(children))
		if len(children) > 0 {
			child, _ := children[0].(map[string]any)
			t.Logf("[ht3] tag0 keys=%s", probeJoinKeys(child))
			tagID = mapString(child, "id", "code", "slug")
			t.Logf("[ht3] tag0 idLen=%d nameRunes=%d", len(tagID), len([]rune(mapString(child, "name"))))
		}
	}
	time.Sleep(500 * time.Millisecond)

	listPaths := []string{
		"/api/videos",
		"/api/videos?page=1",
		"/api/videos?page=1&size=20",
		"/api/videos?page=1&page_size=20",
		"/api/videos?limit=20&offset=0",
	}
	if tagID != "" {
		escaped := url.QueryEscape(tagID)
		listPaths = append(listPaths,
			"/api/videos?tag="+escaped+"&page=1&size=20",
			"/api/videos?tag_id="+escaped+"&page=1&size=20",
			"/api/tags/"+escaped+"/videos?page=1&size=20",
			"/api/tags/"+escaped,
		)
	}
	for index, path := range listPaths {
		var raw json.RawMessage
		callErr := d.huangguoAIAPI(ctx, path, &raw)
		masked := path
		if tagID != "" {
			masked = strings.ReplaceAll(path, tagID, "{tag}")
		}
		t.Logf("[ht3] list[%d] %s %s err=%q", index, masked, probeShape(raw), probeErrText(callErr))
		var envelope struct {
			Items json.RawMessage `json:"items"`
			List  json.RawMessage `json:"list"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			items := envelope.Items
			if len(items) == 0 {
				items = envelope.List
			}
			if len(items) > 0 {
				t.Logf("[ht3] list[%d] cards=%d", index, len(parseHuangguoAIJSONCards(items, base+"/", "")))
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// probeDecode 尝试把文本按 hex / base64 / 原样解成字节。
func probeDecode(text string) [][]byte {
	var out [][]byte
	if raw, err := hex.DecodeString(text); err == nil && len(raw) > 0 {
		out = append(out, raw)
	}
	if raw, err := base64.StdEncoding.DecodeString(text); err == nil && len(raw) > 0 {
		out = append(out, raw)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(text); err == nil && len(raw) > 0 {
		out = append(out, raw)
	}
	if len(text) > 0 {
		out = append(out, []byte(text))
	}
	return out
}

// probeAESOpen 用 AES-CBC 解一次，成功返回明文。
func probeAESOpen(cipherText, key, iv string) (string, bool) {
	for _, raw := range probeDecode(cipherText) {
		if len(raw) == 0 || len(raw)%16 != 0 {
			continue
		}
		for _, keyBytes := range probeDecode(key) {
			if len(keyBytes) != 16 && len(keyBytes) != 24 && len(keyBytes) != 32 {
				continue
			}
			ivs := probeDecode(iv)
			if len(ivs) == 0 {
				ivs = [][]byte{make([]byte, 16)}
			}
			for _, ivBytes := range ivs {
				if len(ivBytes) != 16 {
					continue
				}
				block, err := aes.NewCipher(keyBytes)
				if err != nil {
					continue
				}
				plain := make([]byte, len(raw))
				cipher.NewCBCDecrypter(block, ivBytes).CryptBlocks(plain, raw)
				if len(plain) == 0 {
					continue
				}
				pad := int(plain[len(plain)-1])
				if pad > 0 && pad <= 16 && pad <= len(plain) {
					plain = plain[:len(plain)-pad]
				}
				text := strings.TrimSpace(string(plain))
				if strings.Contains(text, "/") && !strings.ContainsRune(text, '\x00') {
					return text, true
				}
			}
		}
	}
	return "", false
}

// 只在 HD_PROBE=1 时运行：黄桃的 video_url/video_key/video_iv 是不是 AES 加密的 m3u8，
// 以及按 slug/cate_id 取列表的正确参数。只打印字段形状、长度、布尔。
func TestHuangtaoEncryptedMediaProbe(t *testing.T) {
	if os.Getenv("HD_PROBE") != "1" {
		t.Skip("set HD_PROBE=1 to run the huangtao probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	base := "https://aihuangdou.com"
	d.cfg.HuangguoAIURL = base

	var found struct {
		Items json.RawMessage `json:"items"`
	}
	if err := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=1&size=20", &found); err != nil {
		t.Fatalf("search: %v", probeErrText(err))
	}
	cards := parseHuangguoAIJSONCards(found.Items, base+"/", "")
	if len(cards) == 0 {
		t.Fatalf("search returned no items")
	}
	sourceID := cards[0].SourceID
	time.Sleep(400 * time.Millisecond)

	var detail map[string]any
	if err := d.huangguoAIAPI(ctx, "/api/videos/"+url.PathEscape(sourceID), &detail); err != nil {
		t.Fatalf("detail: %v", probeErrText(err))
	}
	t.Logf("[ht4] keys=%s", probeJoinKeys(detail))
	videoURL := mapString(detail, "video_url")
	videoKey := mapString(detail, "video_key")
	videoIV := mapString(detail, "video_iv")
	t.Logf("[ht4] video_type=%s video_api=%s", mapString(detail, "video_type"), maskPathShape(mapString(detail, "video_api")))
	t.Logf("[ht4] video_url len=%d shape=%s hasM3U8=%v", len(videoURL), maskPathShape(videoURL),
		strings.Contains(strings.ToLower(videoURL), ".m3u8"))
	t.Logf("[ht4] video_key len=%d hex=%v video_iv len=%d hex=%v",
		len(videoKey), probeIsHex(videoKey), len(videoIV), probeIsHex(videoIV))
	t.Logf("[ht4] duration=%s titleRunes=%d", mapString(detail, "duration"), len([]rune(mapString(detail, "title"))))

	if plain, ok := probeAESOpen(videoURL, videoKey, videoIV); ok {
		t.Logf("[ht4] aes-open ok shape=%s hasM3U8=%v len=%d",
			maskPathShape(plain), strings.Contains(strings.ToLower(plain), ".m3u8"), len(plain))
		address := resolveProviderURL(base+"/", plain)
		body, mediaErr := d.fetchProviderText(ctx, address, base+"/")
		t.Logf("[ht4] media bytes=%d extm3u=%v segments=%d err=%q",
			len(body), strings.Contains(body, "#EXTM3U"), strings.Count(body, "#EXTINF"), probeErrText(mediaErr))
	} else {
		t.Logf("[ht4] aes-open failed")
		// 退一步：直接抓 video_url（可能是明文或另一层跳转）。
		address := resolveProviderURL(base+"/", videoURL)
		body, mediaErr := d.fetchProviderText(ctx, address, base+"/")
		t.Logf("[ht4] raw video_url bytes=%d extm3u=%v err=%q",
			len(body), strings.Contains(body, "#EXTM3U"), probeErrText(mediaErr))
	}
	time.Sleep(600 * time.Millisecond)

	// 详情页 HTML 里这些字段是不是也内联着（内联的话就不用再请求接口）。
	page, pageErr := d.fetchProviderText(ctx, base+"/video/"+url.PathEscape(sourceID), base+"/")
	lower := strings.ToLower(page)
	t.Logf("[ht4] page bytes=%d videoUrl=%d videoKey=%d m3u8=%d err=%q", len(page),
		strings.Count(lower, "video_url"), strings.Count(lower, "video_key"),
		strings.Count(lower, ".m3u8"), probeErrText(pageErr))
	for _, shape := range probeM3U8Shapes(page) {
		t.Logf("[ht4] page m3u8shape=%s", shape)
	}
	time.Sleep(500 * time.Millisecond)

	// 列表：用 slug / cate_id 再试一轮。
	var tags map[string]any
	if err := d.huangguoAIAPI(ctx, "/api/tags", &tags); err != nil {
		t.Logf("[ht4] tags err=%q", probeErrText(err))
	}
	categories, _ := tags["categories"].([]any)
	var slug, cateID string
	if len(categories) > 0 {
		row, _ := categories[0].(map[string]any)
		cateID = mapString(row, "id")
		children, _ := row["tags"].([]any)
		if len(children) > 0 {
			child, _ := children[0].(map[string]any)
			slug = mapString(child, "slug")
			t.Logf("[ht4] tag0 slugLen=%d idLen=%d cate_id=%s", len(slug), len(mapString(child, "id")), mapString(child, "cate_id"))
		}
	}
	listPaths := []string{}
	if slug != "" {
		esc := url.QueryEscape(slug)
		listPaths = append(listPaths,
			"/api/videos?slug="+esc+"&page=1&size=20",
			"/api/tag/"+esc+"/videos?page=1&size=20",
			"/api/tags/"+esc+"/videos?page=1&size=20",
			"/api/videos?tag_slug="+esc+"&page=1&size=20",
			"/api/videos?tags="+esc+"&page=1&size=20",
		)
	}
	if cateID != "" {
		esc := url.QueryEscape(cateID)
		listPaths = append(listPaths,
			"/api/videos?cate_id="+esc+"&page=1&size=20",
			"/api/categories/"+esc+"/videos?page=1&size=20",
			"/api/videos?category_id="+esc+"&page=1&size=20",
		)
	}
	for index, path := range listPaths {
		var raw json.RawMessage
		callErr := d.huangguoAIAPI(ctx, path, &raw)
		masked := path
		if slug != "" {
			masked = strings.ReplaceAll(masked, url.QueryEscape(slug), "{slug}")
		}
		if cateID != "" {
			masked = strings.ReplaceAll(masked, url.QueryEscape(cateID), "{cate}")
		}
		t.Logf("[ht4] list[%d] %s %s err=%q", index, masked, probeShape(raw), probeErrText(callErr))
		var envelope struct {
			Items json.RawMessage `json:"items"`
			List  json.RawMessage `json:"list"`
			Data  json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			for _, items := range []json.RawMessage{envelope.Items, envelope.List} {
				if len(items) > 0 {
					t.Logf("[ht4] list[%d] cards=%d", index, len(parseHuangguoAIJSONCards(items, base+"/", "")))
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func probeIsHex(text string) bool {
	if text == "" {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}

// 只在 HD_PROBE=1 时运行：黄桃的播放字段是不是「登录才有」（多取几个样本统计），
// 以及首页 HTML 里有没有可直接用的剧集列表。只打印计数与布尔。
func TestHuangtaoGateProbe(t *testing.T) {
	if os.Getenv("HD_PROBE") != "1" {
		t.Skip("set HD_PROBE=1 to run the huangtao probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	base := "https://aihuangdou.com"
	d.cfg.HuangguoAIURL = base

	var found struct {
		Items json.RawMessage `json:"items"`
	}
	if err := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=1&size=20", &found); err != nil {
		t.Fatalf("search: %v", probeErrText(err))
	}
	cards := parseHuangguoAIJSONCards(found.Items, base+"/", "")
	limit := len(cards)
	if limit > 6 {
		limit = 6
	}
	withVideo, withPreview, withEndpoint := 0, 0, 0
	for index := 0; index < limit; index++ {
		var detail map[string]any
		if err := d.huangguoAIAPI(ctx, "/api/videos/"+url.PathEscape(cards[index].SourceID), &detail); err != nil {
			continue
		}
		if mapString(detail, "video_url") != "" {
			withVideo++
		}
		if mapString(detail, "preview_url") != "" {
			withPreview++
		}
		if mapString(detail, "playback_endpoint") != "" {
			withEndpoint++
		}
		time.Sleep(350 * time.Millisecond)
	}
	t.Logf("[ht5] samples=%d withVideo=%d withPreview=%d withEndpoint=%d", limit, withVideo, withPreview, withEndpoint)

	// 首页 HTML 里有没有现成的剧集列表。
	page, pageErr := d.fetchProviderText(ctx, base+"/", base+"/")
	lower := strings.ToLower(page)
	t.Logf("[ht5] home bytes=%d videoLinks=%d nextData=%v nextFlight=%d ldjson=%d err=%q",
		len(page), strings.Count(lower, `href="/video/`),
		strings.Contains(lower, "__next_data__"), strings.Count(lower, "__next_f"),
		strings.Count(lower, "application/ld+json"), probeErrText(pageErr))
	if ids := reProbeVideoPath.FindAllStringSubmatch(page, -1); len(ids) > 0 {
		t.Logf("[ht5] home videoIDs=%d sampleLen=%d", len(ids), len(ids[0][1]))
	}
	time.Sleep(400 * time.Millisecond)

	// 首页/推荐类接口再试一轮。
	for index, path := range []string{
		"/api/feed?page=1&size=20",
		"/api/recommend?page=1&size=20",
		"/api/home?page=1",
		"/api/index?page=1",
		"/api/discover?page=1&size=20",
		"/api/videos/feed?page=1&size=20",
		"/api/videos/list?page=1&size=20",
	} {
		var raw json.RawMessage
		callErr := d.huangguoAIAPI(ctx, path, &raw)
		t.Logf("[ht5] feed[%d] %s %s err=%q", index, path, probeShape(raw), probeErrText(callErr))
		time.Sleep(350 * time.Millisecond)
	}
}

var reProbeVideoPath = regexp.MustCompile(`/video/([A-Za-z0-9]{2,12})`)
