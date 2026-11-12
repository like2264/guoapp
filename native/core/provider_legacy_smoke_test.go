package core

import (
	"context"
	"os"
	"testing"
	"time"
)

// 只在 LEGACY_PROBE=1 时运行。用真实的 fetchLegacyCatalogCategoryPage 走一遍「全部」和「推荐」，
// 确认改写后的爬取器在线上能翻到底、有上限、不重复。只打印条数和布尔值。
func TestLegacyCatalogLiveSmoke(t *testing.T) {
	if os.Getenv("LEGACY_PROBE") != "1" {
		t.Skip("set LEGACY_PROBE=1 to run the live smoke test")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 540*time.Second)
	defer cancel()

	unique := map[string]bool{}
	start := time.Now()
	for page := 1; page <= 14; page++ {
		items, more, err := d.fetchLegacyCatalogCategoryPage(ctx, page, "")
		if err != nil {
			t.Logf("[smoke] all p%d err=%q", page, diagErr(err))
			break
		}
		fresh := 0
		for _, drama := range items {
			if !unique[drama.ID] {
				unique[drama.ID] = true
				fresh++
			}
		}
		t.Logf("[smoke] all p%d len=%d fresh=%d total=%d more=%v", page, len(items), fresh, len(unique), more)
		if !more {
			break
		}
	}
	t.Logf("[smoke] all total=%d elapsed=%s", len(unique), time.Since(start).Truncate(time.Second))

	recommend := map[string]bool{}
	for page := 1; page <= 4; page++ {
		items, more, err := d.fetchLegacyCatalogCategoryPage(ctx, page, legacyRecommendID)
		if err != nil {
			t.Logf("[smoke] recommend p%d err=%q", page, diagErr(err))
			break
		}
		fresh := 0
		for _, drama := range items {
			if !recommend[drama.ID] {
				recommend[drama.ID] = true
				fresh++
			}
		}
		t.Logf("[smoke] recommend p%d len=%d fresh=%d total=%d more=%v", page, len(items), fresh, len(recommend), more)
		if !more {
			break
		}
	}

	// 第二次请求同一页必须命中缓存，不应再打站点（用耗时粗判）。
	cacheStart := time.Now()
	if items, more, err := d.fetchLegacyCatalogCategoryPage(ctx, 2, ""); err != nil {
		t.Logf("[smoke] cache probe err=%q", diagErr(err))
	} else {
		t.Logf("[smoke] cache p2 len=%d more=%v elapsed=%s", len(items), more, time.Since(cacheStart).Truncate(time.Millisecond))
	}

	// 搜索：用正确写法应当有多条命中。
	if items, more, err := d.searchLegacyCatalog(ctx, 1, "琼明神女录"); err != nil {
		t.Logf("[smoke] search err=%q", diagErr(err))
	} else {
		t.Logf("[smoke] search len=%d more=%v", len(items), more)
	}
}
