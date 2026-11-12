package core

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"
)

// 只在 LEGACY_PROBE=1 时运行。第二轮：定量确认真实剧库规模、推荐接口能否深翻、
// 标签接口有没有配套列表。输出只有条数、页码、布尔值和长度，不打印剧名/分类名/域名。
func probe2Wait(ctx context.Context) {
	select {
	case <-time.After(650 * time.Millisecond):
	case <-ctx.Done():
	}
}

// probe2Fetch 在 4010（频率限制）时退避重试，避免把「限流」误读成「没数据」。
func probe2Fetch(ctx context.Context, d *Downloader, path string, params map[string]string) (probeList, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		page, err := probeFetch(ctx, d, path, params)
		if err == nil {
			return page, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(time.Duration(attempt+1) * 1500 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return probeList{}, lastErr
}

func TestLegacyDeepProbe(t *testing.T) {
	if os.Getenv("LEGACY_PROBE") != "1" {
		t.Skip("set LEGACY_PROBE=1 to run the live deep probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
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

	// 第一段：每个分类一路往下翻，看到底有多少页，以及全集有多少部。
	catalog := map[string]bool{}
	for index, tab := range valid {
		seen := map[string]bool{}
		for page := 1; page <= 8; page++ {
			if ctx.Err() != nil {
				break
			}
			result, err := probe2Fetch(ctx, d, "/api/app/playlet/home/tab/"+url.PathEscape(tab.ID), map[string]string{
				"pageNumber": strconv.Itoa(page), "pageSize": strconv.Itoa(size), "tabSortType": "1",
			})
			if err != nil {
				t.Logf("[probe2] tab[%d] p%d err=%q", index, page, diagErr(err))
				break
			}
			fresh := 0
			for _, drama := range result.List {
				key := firstNonEmpty(drama.SourceID, drama.ID)
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				catalog[key] = true
				fresh++
			}
			t.Logf("[probe2] tab[%d] p%d len=%d fresh=%d total=%d", index, page, len(result.List), fresh, len(seen))
			if len(result.List) == 0 {
				break
			}
			probe2Wait(ctx)
		}
	}
	t.Logf("[probe2] catalog_unique=%d", len(catalog))

	// 第二段：推荐位能不能深翻——能翻就是「整库」入口。
	recommend := map[string]bool{}
	for page := 1; page <= 5; page++ {
		if ctx.Err() != nil {
			break
		}
		result, err := probe2Fetch(ctx, d, "/api/app/playlet-recommend/home/choose", map[string]string{
			"pageNumber": strconv.Itoa(page), "pageSize": strconv.Itoa(size),
		})
		if err != nil {
			t.Logf("[probe2] choose p%d err=%q", page, diagErr(err))
			break
		}
		fresh := 0
		for _, drama := range result.List {
			key := firstNonEmpty(drama.SourceID, drama.ID)
			if key != "" && !recommend[key] {
				recommend[key] = true
				fresh++
			}
		}
		t.Logf("[probe2] choose p%d len=%d fresh=%d total=%d", page, len(result.List), fresh, len(recommend))
		if len(result.List) == 0 {
			break
		}
		probe2Wait(ctx)
	}
	union := 0
	for key := range recommend {
		if !catalog[key] {
			union++
		}
	}
	t.Logf("[probe2] choose_unique=%d recommend_only=%d with_catalog=%d", len(recommend), union, len(catalog)+union)

	// 第三段：388 条标签有没有配套的列表接口，有就能按题材把整库拉出来。
	var raw json.RawMessage
	if err := d.fetchAPI(ctx, "/api/app/playlet-tag/all", nil, &raw); err != nil {
		t.Logf("[probe2] tag_all err=%q", diagErr(err))
	} else {
		var parsed struct {
			List []map[string]any `json:"list"`
		}
		if json.Unmarshal(raw, &parsed) == nil && len(parsed.List) > 0 {
			tagID := mapString(parsed.List[0], "id", "tagId", "tag_id")
			t.Logf("[probe2] tag_all len=%d id_len=%d", len(parsed.List), len(tagID))
			if tagID != "" {
				candidates := []string{
					"/api/app/playlet/home/tag/" + url.PathEscape(tagID),
					"/api/app/playlet-tag/" + url.PathEscape(tagID) + "/list",
					"/api/app/playlet/tag/" + url.PathEscape(tagID),
					"/api/app/playlet-recommend/home/tag/" + url.PathEscape(tagID),
				}
				for index, path := range candidates {
					if ctx.Err() != nil {
						break
					}
					result, err := probe2Fetch(ctx, d, path, map[string]string{
						"pageNumber": "1", "pageSize": strconv.Itoa(size),
					})
					t.Logf("[probe2] tagcand[%d] len=%d count=%s err=%q", index, len(result.List), probeScalar(result.Count), diagErr(err))
					probe2Wait(ctx)
				}
			}
		}
	}

	// 第四段：用正确的片名（琼明，不是琼名）验证关键词命中数。
	for index, keyword := range []string{"琼明神女录", "琼名神女录", "神女录"} {
		if ctx.Err() != nil {
			break
		}
		result, err := probe2Fetch(ctx, d, "/api/app/playlet/keyword/search", map[string]string{
			"keyword": keyword, "pageNumber": "1", "pageSize": strconv.Itoa(size),
		})
		t.Logf("[probe2] search[%d] runes=%d len=%d count=%s err=%q", index, len([]rune(keyword)), len(result.List), probeScalar(result.Count), diagErr(err))
		probe2Wait(ctx)
	}
}
