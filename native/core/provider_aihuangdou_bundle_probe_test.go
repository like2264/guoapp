package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func asciiOnly(text string) string {
	var out []rune
	for _, r := range text {
		if r < 128 {
			out = append(out, r)
		}
	}
	if len(out) > 160 {
		out = out[:160]
	}
	return strings.TrimSpace(string(out))
}

func probeErrText(err error) string {
	if err == nil {
		return ""
	}
	return asciiOnly(err.Error())
}

func probeShape(raw []byte) string {
	if len(raw) == 0 {
		return "-"
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "raw"
	}
	switch typed := value.(type) {
	case []any:
		return fmt.Sprintf("array len=%d", len(typed))
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 14 {
			keys = keys[:14]
		}
		return "keys=" + strings.Join(keys, ",")
	default:
		return "scalar"
	}
}

// 只在 HD_PROBE=1 时运行：把 aihuangdou.com 全部前端脚本扒一遍，
// 取出它自己调用的 /api/... 路由表；同时把一部剧的详情字段名和分集/播放候选接口试完。
// 只打印路径、字段名、条数和 ASCII 错误，不打印剧名。
func TestAiHuangdouBundleProbe(t *testing.T) {
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

	home, homeErr := d.fetchProviderText(ctx, base+"/", base+"/")
	scripts := reProbeScriptSrc.FindAllStringSubmatch(home, -1)
	t.Logf("[hd4] home bytes=%d scripts=%d err=%q", len(home), len(scripts), diagErr(homeErr))

	seenScript := map[string]bool{}
	for index, match := range scripts {
		src := resolveProviderURL(base+"/", match[1])
		if seenScript[src] {
			continue
		}
		seenScript[src] = true
		body, scriptErr := d.fetchProviderText(ctx, src, base+"/")
		lower := strings.ToLower(body)
		routes := reProbeAPIRoute.FindAllString(body, -1)
		unique := make([]string, 0, len(routes))
		seenRoute := map[string]bool{}
		for _, route := range routes {
			route = strings.TrimRight(route, "`\"'.,);")
			if len(route) < 8 || seenRoute[route] {
				continue
			}
			seenRoute[route] = true
			unique = append(unique, route)
		}
		t.Logf("[hd4] script[%02d] bytes=%d routes=%d ep=%d play=%d cat=%d search=%d err=%q",
			index, len(body), len(unique),
			strings.Count(lower, "episode"), strings.Count(lower, "play"),
			strings.Count(lower, "categor"), strings.Count(lower, "search"),
			probeErrText(scriptErr))
		for routeIndex, route := range unique {
			if routeIndex >= 20 {
				break
			}
			t.Logf("[hd4] route[%02d/%02d]=%s", index, routeIndex, route)
		}
		time.Sleep(250 * time.Millisecond)
	}

	var search struct {
		Items json.RawMessage `json:"items"`
	}
	if err := d.huangguoAIAPI(ctx, "/api/search?q="+url.QueryEscape("总裁")+"&page=1&size=20", &search); err != nil {
		t.Logf("[hd4] search failed err=%q", probeErrText(err))
		return
	}
	items := parseHuangguoAIJSONCards(search.Items, base+"/", "")
	if len(items) == 0 {
		t.Logf("[hd4] search returned no items")
		return
	}
	sourceID := url.PathEscape(items[0].SourceID)
	t.Logf("[hd4] sample id len=%d", len(items[0].SourceID))

	var detail json.RawMessage
	detailErr := d.huangguoAIAPI(ctx, "/api/videos/"+sourceID, &detail)
	t.Logf("[hd4] detail %s err=%q", probeShape(detail), probeErrText(detailErr))
	time.Sleep(400 * time.Millisecond)

	paths := []string{
		"/api/videos/" + sourceID + "/episodes",
		"/api/videos/" + sourceID + "/episode",
		"/api/episodes?video_id=" + sourceID,
		"/api/videos/" + sourceID + "/chapters",
		"/api/videos/" + sourceID + "/play?ep=1",
		"/api/videos/" + sourceID + "/play/1",
		"/api/play/" + sourceID + "?ep=1",
		"/api/videos/" + sourceID + "/url?ep=1",
	}
	for index, path := range paths {
		var raw json.RawMessage
		callErr := d.huangguoAIAPI(ctx, path, &raw)
		t.Logf("[hd4] ep cand[%02d] %s %s err=%q", index, path, probeShape(raw), probeErrText(callErr))
		time.Sleep(400 * time.Millisecond)
	}
}
