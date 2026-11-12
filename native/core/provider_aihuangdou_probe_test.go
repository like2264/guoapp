package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

var reProbeScriptSrc = regexp.MustCompile(`(?i)<script[^>]+src\s*=\s*["']([^"']+)["']`)

// 只在 HD_PROBE=1 时运行：判断 aihuangdou.com 是否就是现有「黄豆」那套加密接口，
// 以及它的搜索入口在哪。只打印字节数、条数、路径和错误摘要，不打印剧名和域名。
func TestAiHuangdouProbe(t *testing.T) {
	if os.Getenv("HD_PROBE") != "1" {
		t.Skip("set HD_PROBE=1 to run the aihuangdou probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	host := "https://aihuangdou.com"

	// 第一段：当普通网页抓一次，看是 SPA 还是服务端渲染，以及有没有可扒的前端脚本。
	home, homeErr := d.fetchProviderText(ctx, host+"/", host+"/")
	scripts := reProbeScriptSrc.FindAllStringSubmatch(home, -1)
	t.Logf("[hd] home bytes=%d scripts=%d err=%q", len(home), len(scripts), diagErr(homeErr))

	// 第二段：把这套域名直接喂给现有的黄豆加密客户端，看 /drama/list 通不通。
	// 通了就说明 aihuangdou.com 只是同一套接口的另一个域名。
	d.cfg.HuangdouURL = host
	d.cfg.Retries = 1
	client := newHuangdouAPIClient(d)
	client.host = host
	var decoded any
	listErr := client.call(ctx, "/drama/list", map[string]any{"page": "1", "page_size": "30"}, &decoded)
	rows := huangdouList(decoded)
	t.Logf("[hd] list len=%d err=%q", len(rows), diagErr(listErr))

	// 第三段：搜索入口候选。加密接口用的是 POST 加密包，参数名只能一个个试。
	keyword := "总裁"
	candidates := []struct {
		path string
		key  string
	}{
		{"/drama/search", "keyword"},
		{"/drama/list", "keyword"},
		{"/drama/list", "name"},
		{"/drama/keyword", "keyword"},
		{"/search/drama", "keyword"},
		{"/drama/search/keyword", "keyword"},
		{"/search", "keyword"},
		{"/drama/list", "search"},
	}
	for index, candidate := range candidates {
		var out any
		started := time.Now()
		callErr := client.call(ctx, candidate.path, map[string]any{
			candidate.key: keyword, "page": "1", "page_size": "30",
		}, &out)
		found := len(huangdouList(out))
		t.Logf("[hd] cand[%02d] path=%s key=%s len=%d cost=%s err=%q",
			index, candidate.path, candidate.key, found, time.Since(started).Round(time.Millisecond), diagErr(callErr))
		time.Sleep(600 * time.Millisecond)
	}

	// 第四段：扒前端脚本里的接口路由表，比逐个猜准。
	for index, match := range scripts {
		if index >= 3 {
			break
		}
		src := resolveProviderURL(host+"/", match[1])
		body, scriptErr := d.fetchProviderText(ctx, src, host+"/")
		if scriptErr != nil {
			t.Logf("[hd] bundle[%d] bytes=0 err=%q", index, diagErr(scriptErr))
			continue
		}
		routes := reProbeAPIRoute.FindAllString(body, -1)
		seen := make(map[string]bool, len(routes))
		unique := make([]string, 0, len(routes))
		for _, route := range routes {
			route = strings.TrimRight(route, "/`\"'")
			if route == "" || seen[route] {
				continue
			}
			seen[route] = true
			unique = append(unique, route)
		}
		t.Logf("[hd] bundle[%d] bytes=%d routes=%d", index, len(body), len(unique))
		for routeIndex, route := range unique {
			if routeIndex >= 25 {
				break
			}
			t.Logf("[hd] route[%02d]=%s", routeIndex, route)
		}
		t.Logf("[hd] bundle[%d] tokens keyword=%d search=%d page_size=%d",
			index,
			strings.Count(strings.ToLower(body), "keyword"),
			strings.Count(strings.ToLower(body), "search"),
			strings.Count(strings.ToLower(body), "page_size"))
		time.Sleep(300 * time.Millisecond)
	}
}

var reProbeAPIRoute = regexp.MustCompile(`/api/[A-Za-z0-9_/{}$.-]{2,60}`)

// 第二段：aihuangdou.com 首页里那个 /api/edge/request 桥，看起来和黄果AI 的接口层是同一套。
// 直接把黄果AI 的调用方式套上去，看能不能拿到数据；同时用黄果AI 自己的域名做对照。
func TestAiHuangdouEdgeProbe(t *testing.T) {
	if os.Getenv("HD_PROBE") != "1" {
		t.Skip("set HD_PROBE=1 to run the aihuangdou probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	bases := []string{"https://aihuangdou.com", "https://huangguoai.com"}
	for index, base := range bases {
		d.cfg.HuangguoAIURL = base

		var search struct {
			Items      json.RawMessage `json:"items"`
			Pagination struct {
				Pages int `json:"pages"`
			} `json:"pagination"`
		}
		started := time.Now()
		searchErr := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=1&size=20", &search)
		items := 0
		if searchErr == nil {
			items = len(parseHuangguoAIJSONCards(search.Items, base+"/", ""))
		}
		t.Logf("[hd2] host[%d] search items=%d pages=%d cost=%s err=%q",
			index, items, search.Pagination.Pages, time.Since(started).Round(time.Millisecond), diagErr(searchErr))

		if len(nativeAICategories) > 0 {
			var body json.RawMessage
			path := "/api/videos/category/" + url.PathEscape(nativeAICategories[0].ID) + "?sort=hot&page=1&size=24"
			catalogErr := d.huangguoAIAPI(ctx, path, &body)
			cards := 0
			if catalogErr == nil {
				cards = len(parseHuangguoAIJSONCards(body, base+"/", ""))
			}
			t.Logf("[hd2] host[%d] catalog cards=%d err=%q", index, cards, diagErr(catalogErr))
		}

		envelope, marshalErr := json.Marshal(map[string]string{
			"method": http.MethodGet, "path": "/api/account/me", "body": "", "content_type": "",
		})
		status := -1
		rawLen := 0
		var bridgeErr error = marshalErr
		if marshalErr == nil {
			var raw string
			raw, bridgeErr = d.fetchProviderRequest(ctx, base+"/api/edge/request", base+"/", envelope)
			rawLen = len(raw)
			if bridgeErr == nil {
				var reply struct {
					Status int    `json:"status"`
					Msg    string `json:"msg"`
				}
				if json.Unmarshal([]byte(raw), &reply) == nil {
					status = reply.Status
				}
			}
		}
		t.Logf("[hd2] host[%d] bridge bytes=%d status=%d err=%q", index, rawLen, status, diagErr(bridgeErr))
		time.Sleep(700 * time.Millisecond)
	}
	d.cfg.HuangguoAIURL = ""
}

// 第三段：aihuangdou.com 的内容接口是否齐全（详情、播放、分类表），只打印条数和字段名。
func TestAiHuangdouContentProbe(t *testing.T) {
	if os.Getenv("HD_PROBE") != "1" {
		t.Skip("set HD_PROBE=1 to run the aihuangdou probe")
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

	var search struct {
		Items json.RawMessage `json:"items"`
	}
	if err := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=1&size=20", &search); err != nil {
		t.Fatalf("search: %v", diagErr(err))
	}
	items := parseHuangguoAIJSONCards(search.Items, base+"/", "")
	t.Logf("[hd3] search items=%d", len(items))
	if len(items) == 0 {
		return
	}
	sourceID := items[0].SourceID
	t.Logf("[hd3] first cover=%v totalEpisode=%d", items[0].Cover != "", items[0].TotalEpisode)

	var detail map[string]any
	detailErr := d.huangguoAIAPI(ctx, "/api/videos/"+url.PathEscape(sourceID), &detail)
	episodes, _ := detail["episodes"].([]any)
	t.Logf("[hd3] detail eps=%d titleRunes=%d keys=%d err=%q",
		len(episodes), len([]rune(mapString(detail, "title", "name"))), len(detail), diagErr(detailErr))
	time.Sleep(500 * time.Millisecond)

	var play struct {
		VideoURL string `json:"video_url"`
	}
	playErr := d.huangguoAIAPI(ctx, "/api/videos/"+url.PathEscape(sourceID)+"/play?ep=1&page_texts=0", &play)
	t.Logf("[hd3] play urlLen=%d err=%q", len(play.VideoURL), diagErr(playErr))
	time.Sleep(500 * time.Millisecond)

	paths := []string{
		"/api/categories", "/api/videos/categories", "/api/category", "/api/category/list",
		"/api/home", "/api/videos?page=1&size=24", "/api/navs", "/api/tabs",
	}
	for index, path := range paths {
		var raw json.RawMessage
		callErr := d.huangguoAIAPI(ctx, path, &raw)
		shape := "-"
		if callErr == nil {
			var value any
			if json.Unmarshal(raw, &value) == nil {
				switch typed := value.(type) {
				case []any:
					shape = fmt.Sprintf("array len=%d", len(typed))
				case map[string]any:
					keys := make([]string, 0, len(typed))
					for key := range typed {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					if len(keys) > 8 {
						keys = keys[:8]
					}
					shape = "keys=" + strings.Join(keys, ",")
				default:
					shape = "scalar"
				}
			}
		}
		t.Logf("[hd3] cat[%02d] %s bytes=%d %s err=%q", index, path, len(raw), shape, diagErr(callErr))
		time.Sleep(400 * time.Millisecond)
	}
}
