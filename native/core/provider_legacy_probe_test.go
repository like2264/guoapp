package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 只在 LEGACY_PROBE=1 时运行。用途：一次把「黄果旧版」目录被截断的所有可能原因试完。
// 输出只包含条数、count 数值、布尔值、参数名，不打印任何剧名、分类名、域名或真实 ID。
type probeList struct {
	List    []Drama `json:"list"`
	Count   any     `json:"count"`
	HasNext any     `json:"hasNext"`
}

func probeScalar(value any) string {
	switch typed := value.(type) {
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case string:
		return "str"
	case nil:
		return "null"
	}
	return "obj"
}

func probeFetch(ctx context.Context, d *Downloader, path string, params map[string]string) (probeList, error) {
	var page probeList
	err := d.fetchAPI(ctx, path, params, &page)
	return page, err
}

// probePagerParams 用不同参数名表达「第 page 页」，看站点认哪一个。
func probePagerParams(name string, page, size int) map[string]string {
	params := map[string]string{"pageSize": strconv.Itoa(size), "tabSortType": "1"}
	switch name {
	case "":
		return params
	case "offset":
		params["offset"] = strconv.Itoa((page - 1) * size)
	case "start":
		params["start"] = strconv.Itoa((page - 1) * size)
	default:
		params[name] = strconv.Itoa(page)
	}
	return params
}

func labelOf(name string) string {
	if name == "" {
		return "(none)"
	}
	return name
}

func TestLegacyPagingProbe(t *testing.T) {
	if os.Getenv("LEGACY_PROBE") != "1" {
		t.Skip("set LEGACY_PROBE=1 to run the live paging probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
	defer cancel()
	size := max(1, d.cfg.PageSize)

	var tabs legacyTabList
	if err := d.fetchAPI(ctx, "/api/app/playlet-tab/all", nil, &tabs); err != nil {
		t.Fatalf("tab list failed: %s", diagErr(err))
	}
	var valid []Tab
	for _, tab := range tabs {
		if tab.ID != "" && tab.Name != "" {
			valid = append(valid, tab)
		}
	}
	if len(valid) == 0 {
		t.Fatalf("no usable tab")
	}
	tab := valid[0]
	t.Logf("[probe] api_override=%v pageSize=%d tabs=%d", d.cfg.APIBase != "", size, len(valid))

	// 第一段：只用一个分类，试各种翻页参数名，看第 2 页是否真有数据。
	names := []string{"pageNumber", "pageNum", "page", "pageNo", "pageIndex", "current", "offset", "start", ""}
	var pageOneIDs []string
	for _, name := range names {
		if ctx.Err() != nil {
			break
		}
		p1, err1 := probeFetch(ctx, d, "/api/app/playlet/home/tab/"+tab.ID, probePagerParams(name, 1, size))
		p2, err2 := probeFetch(ctx, d, "/api/app/playlet/home/tab/"+tab.ID, probePagerParams(name, 2, size))
		if name == "pageNumber" {
			for _, drama := range p1.List {
				if id := firstNonEmpty(drama.SourceID, drama.ID); id != "" {
					pageOneIDs = append(pageOneIDs, id)
				}
			}
		}
		seen := map[string]bool{}
		for _, id := range pageOneIDs {
			seen[id] = true
		}
		overlap := 0
		for _, drama := range p2.List {
			if seen[firstNonEmpty(drama.SourceID, drama.ID)] {
				overlap++
			}
		}
		t.Logf("[probe] pager=%-10s p1 len=%d count=%s next=%s err=%q | p2 len=%d count=%s err=%q overlap=%d",
			labelOf(name), len(p1.List), probeScalar(p1.Count), probeScalar(p1.HasNext), diagErr(err1),
			len(p2.List), probeScalar(p2.Count), diagErr(err2), overlap)
	}

	// 第二段：pageSize 会不会被站点截断。
	for _, probeSize := range []int{10, 20, 30, 50, 100} {
		if ctx.Err() != nil {
			break
		}
		page, err := probeFetch(ctx, d, "/api/app/playlet/home/tab/"+tab.ID, probePagerParams("pageNumber", 1, probeSize))
		t.Logf("[probe] pageSize=%-4d len=%d count=%s err=%q", probeSize, len(page.List), probeScalar(page.Count), diagErr(err))
	}

	// 第三段：tabSortType 换排序会不会给出另一批剧。
	for _, sortType := range []string{"1", "2", "3", "4"} {
		if ctx.Err() != nil {
			break
		}
		page, err := probeFetch(ctx, d, "/api/app/playlet/home/tab/"+tab.ID, map[string]string{
			"pageNumber": "1", "pageSize": strconv.Itoa(size), "tabSortType": sortType,
		})
		t.Logf("[probe] sortType=%s len=%d count=%s err=%q", sortType, len(page.List), probeScalar(page.Count), diagErr(err))
	}

	// 第四段：每个分类的第 2 页，逐个确认是不是全都空。
	for index, item := range valid {
		if ctx.Err() != nil {
			break
		}
		page, err := probeFetch(ctx, d, "/api/app/playlet/home/tab/"+item.ID, probePagerParams("pageNumber", 2, size))
		t.Logf("[probe] tab[%d] p2 len=%d count=%s err=%q", index, len(page.List), probeScalar(page.Count), diagErr(err))
	}

	// 第五段：站点还有没有「整库」入口。命中任意一个就能把全量剧拉回来。
	candidates := []string{
		"/api/app/playlet-recommend/home/new",
		"/api/app/playlet-recommend/home/choose",
		"/api/app/playlet/home/list",
		"/api/app/playlet/all",
		"/api/app/playlet/list",
		"/api/app/playlet/page",
		"/api/app/playlet/classify/list",
		"/api/app/playlet-tag/all",
		"/api/app/playlet-category/all",
		"/api/app/playlet/rank/list",
		"/api/app/playlet/home/recommend",
		"/api/app/playlet/search",
		"/api/app/playlet-tab/list",
	}
	for index, path := range candidates {
		if ctx.Err() != nil {
			break
		}
		page, err := probeFetch(ctx, d, path, probePagerParams("pageNumber", 1, size))
		t.Logf("[probe] cand[%02d] len=%d count=%s err=%q", index, len(page.List), probeScalar(page.Count), diagErr(err))
	}

	// 第六段：关键词接口——整名、前 3 字、前 2 字，以及第 2 页和更大 pageSize。
	keyword, runes := "", 0
	for _, title := range collectProbeTitles(ctx, d, tab.ID, size) {
		if len([]rune(title)) >= 4 {
			keyword, runes = title, len([]rune(title))
			break
		}
	}
	if keyword == "" {
		t.Logf("[probe] search skipped: no usable title")
		return
	}
	t.Logf("[probe] keyword_runes=%d", runes)
	words := []struct {
		name string
		text string
	}{
		{"full", keyword},
		{"head3", string([]rune(keyword)[:3])},
		{"head2", string([]rune(keyword)[:2])},
	}
	for _, word := range words {
		if ctx.Err() != nil {
			break
		}
		page, err := probeFetch(ctx, d, "/api/app/playlet/keyword/search", map[string]string{
			"keyword": word.text, "pageNumber": "1", "pageSize": strconv.Itoa(size),
		})
		next, errNext := probeFetch(ctx, d, "/api/app/playlet/keyword/search", map[string]string{
			"keyword": word.text, "pageNumber": "2", "pageSize": strconv.Itoa(size),
		})
		t.Logf("[probe] search kw=%s p1 len=%d count=%s next=%s err=%q | p2 len=%d err=%q",
			word.name, len(page.List), probeScalar(page.Count), probeScalar(page.HasNext), diagErr(err),
			len(next.List), diagErr(errNext))
	}
	big, errBig := probeFetch(ctx, d, "/api/app/playlet/keyword/search", map[string]string{
		"keyword": keyword, "pageNumber": "1", "pageSize": "50",
	})
	t.Logf("[probe] search kw=full size=50 len=%d count=%s err=%q", len(big.List), probeScalar(big.Count), diagErr(errBig))

	// 第七段：搜索接口到底认哪个参数名，以及 GET/POST 有没有区别。
	// 用用户反馈搜不到的那部剧当关键词，只打印条数。
	const probeKeyword = "琼名神女录"
	for _, name := range []string{"keyword", "kw", "name", "searchKey", "title", "q", "wd"} {
		if ctx.Err() != nil {
			break
		}
		page, err := probeFetch(ctx, d, "/api/app/playlet/keyword/search", map[string]string{
			name: probeKeyword, "pageNumber": "1", "pageSize": strconv.Itoa(size),
		})
		t.Logf("[probe] searchparam=%-9s len=%d count=%s err=%q", name, len(page.List), probeScalar(page.Count), diagErr(err))
	}
	if access, accessErr := d.legacyCredentials(ctx); accessErr == nil {
		raw, rawErr := d.legacyRequest(ctx, http.MethodPost, "/api/app/playlet/keyword/search", map[string]string{
			"keyword": probeKeyword, "pageNumber": "1", "pageSize": strconv.Itoa(size),
		}, access)
		var post probeList
		unmarshalErr := json.Unmarshal(raw, &post)
		t.Logf("[probe] search_post len=%d count=%s err=%q unmarshal=%q",
			len(post.List), probeScalar(post.Count), diagErr(rawErr), diagErr(unmarshalErr))
	} else {
		t.Logf("[probe] search_post skipped credentials=%q", diagErr(accessErr))
	}

	// 第八段：用用户反馈搜不到的那部剧当关键词，只打印条数，复现「原来很多、现在只有一部」。
	for _, fixed := range []string{"琼名神女录", "琼名神"} {
		if ctx.Err() != nil {
			break
		}
		p1, err1 := probeFetch(ctx, d, "/api/app/playlet/keyword/search", map[string]string{
			"keyword": fixed, "pageNumber": "1", "pageSize": strconv.Itoa(size),
		})
		p2, err2 := probeFetch(ctx, d, "/api/app/playlet/keyword/search", map[string]string{
			"keyword": fixed, "pageNumber": "2", "pageSize": strconv.Itoa(size),
		})
		t.Logf("[probe] fixed kw_runes=%d p1 len=%d count=%s next=%s err=%q | p2 len=%d count=%s err=%q",
			len([]rune(fixed)), len(p1.List), probeScalar(p1.Count), probeScalar(p1.HasNext), diagErr(err1),
			len(p2.List), probeScalar(p2.Count), diagErr(err2))
	}

	// 第九段：扒前端 JS 的路由表，直接看站点自己用的是哪些接口和分页参数名。
	// 只打印接口路径（不是剧集内容），足够判断真假。
	scanFrontendRoutes(ctx, d, t)
}

var (
	probeScriptSrc = regexp.MustCompile(`(?i)<script[^>]+src="([^"]+)"`)
	probeAPIPath   = regexp.MustCompile(`/api/[A-Za-z0-9_\-/.]{2,90}`)
)

func probeGet(ctx context.Context, d *Downloader, address, referer string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := d.doCatalogRequestWithTimeout(req, providerTimeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func scanFrontendRoutes(ctx context.Context, d *Downloader, t *testing.T) {
	page, err := probeGet(ctx, d, legacyFrontendURL+"/", "", 4<<20)
	if err != nil {
		t.Logf("[probe] front_page failed err=%q", diagErr(err))
		return
	}
	var scripts []string
	for _, match := range probeScriptSrc.FindAllStringSubmatch(string(page), -1) {
		src := strings.TrimSpace(match[1])
		if src == "" || strings.HasPrefix(src, "data:") {
			continue
		}
		scripts = append(scripts, src)
	}
	t.Logf("[probe] front_page bytes=%d scripts=%d", len(page), len(scripts))
	if len(scripts) > 6 {
		scripts = scripts[:6]
	}

	routes := map[string]bool{}
	tokens := map[string]int{}
	scanned := 0
	for _, src := range scripts {
		address := src
		if strings.HasPrefix(src, "//") {
			address = "https:" + src
		} else if strings.HasPrefix(src, "/") {
			address = legacyFrontendURL + src
		} else if !strings.HasPrefix(src, "http") {
			address = legacyFrontendURL + "/" + src
		}
		body, err := probeGet(ctx, d, address, legacyFrontendURL+"/", 16<<20)
		if err != nil {
			t.Logf("[probe] bundle[%d] failed err=%q", scanned, diagErr(err))
			scanned++
			continue
		}
		text := string(body)
		t.Logf("[probe] bundle[%d] bytes=%d", scanned, len(body))
		for _, route := range probeAPIPath.FindAllString(text, -1) {
			routes[route] = true
		}
		for _, token := range []string{"pageNumber", "pageNum", "pageSize", "pageNo", "pageIndex", "tabSortType", "keyword", "offset"} {
			tokens[token] += strings.Count(text, token)
		}
		scanned++
		if ctx.Err() != nil {
			break
		}
	}

	ordered := make([]string, 0, len(routes))
	for route := range routes {
		ordered = append(ordered, route)
	}
	sort.Strings(ordered)
	if len(ordered) > 80 {
		ordered = ordered[:80]
	}
	for index, route := range ordered {
		t.Logf("[probe] route[%02d]=%s", index, route)
	}
	for _, token := range []string{"pageNumber", "pageNum", "pageNo", "pageIndex", "pageSize", "tabSortType", "keyword", "offset"} {
		t.Logf("[probe] token %s count=%d", token, tokens[token])
	}
}

// collectProbeTitles 取分类首页的标题，供关键词探测当输入；只在本进程内使用，不打印。
func collectProbeTitles(ctx context.Context, d *Downloader, tabID string, size int) []string {
	page, err := probeFetch(ctx, d, "/api/app/playlet/home/tab/"+tabID, probePagerParams("pageNumber", 1, size))
	if err != nil {
		return nil
	}
	titles := make([]string, 0, len(page.List))
	for _, drama := range page.List {
		if title := drama.DisplayTitle(); title != "" {
			titles = append(titles, title)
		}
	}
	return titles
}
