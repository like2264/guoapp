package sources

import (
	"encoding/json"
	"testing"
)

func TestParseXduanjuCards(t *testing.T) {
	body := `
<ul>
<li class="card"><a href="/ju/abc-1" title="测试剧">
  <img src="/tu/a.jpg" alt="备用标题"><span class="name">总裁的替身新娘</span><em class="eps">全 80 集</em>
</a></li>
<li class="card"><a href="/ju/abc-2"><img src="/tu/b.jpg" alt="仅 alt 标题"><span class="name"></span></a></li>
<li class="card"><a href="/ju/bad slug"><span class="name">非法标识</span></a></li>
</ul>`
	items := parseXduanjuCards(body, "https://x-duanju.com")
	if len(items) != 2 {
		t.Fatalf("expected 2 cards, got %d", len(items))
	}
	first := items[0]
	if first.SourceID != "abc-1" || first.Title != "总裁的替身新娘" {
		t.Fatalf("unexpected first card: %+v", first)
	}
	if first.Cover != "https://x-duanju.com/tu/a.jpg" {
		t.Fatalf("cover not resolved: %q", first.Cover)
	}
	if first.Remark != "全 80 集" {
		t.Fatalf("remark not parsed: %q", first.Remark)
	}
	if items[1].Title != "仅 alt 标题" {
		t.Fatalf("alt fallback failed: %q", items[1].Title)
	}
}

func TestParseHuangguoVideoCardsFallback(t *testing.T) {
	// 搜索页没有 video-card class，走 article 兜底。
	body := `
<article><a href="/series/ab12" title="搜索命中的剧"><img data-src="/c.jpg"></a></article>
<article><a href="/video/cd34" title="单片视频"></a></article>
<article><a href="/about">关于我们</a></article>`
	items := parseHuangguoVideoCards(body, "https://huangguo.video/search?q=x")
	if len(items) != 2 {
		t.Fatalf("expected 2 cards, got %d: %+v", len(items), items)
	}
	if items[0].SourceID != "series/ab12" || items[0].CategoryName != "series" {
		t.Fatalf("unexpected series card: %+v", items[0])
	}
	if items[1].SourceID != "video/cd34" || items[1].CategoryName != "video" {
		t.Fatalf("unexpected video card: %+v", items[1])
	}
	if items[0].Cover != "https://huangguo.video/c.jpg" {
		t.Fatalf("cover not resolved: %q", items[0].Cover)
	}
}

func TestHuangguoDramaFromMap(t *testing.T) {
	row := map[string]any{
		"id":            "6979",
		"title":         "总裁的替身新娘",
		"cover":         "/upload/cover.jpg",
		"episode_count": json.Number("80"),
		"is_finished":   "true",
		"views":         "12000",
	}
	drama, ok := huangguoDramaFromMap(row, "https://huangguoai.com/", "AI 短剧")
	if !ok {
		t.Fatal("expected drama parsed")
	}
	if drama.Source != sourceHuangguoAI || drama.SourceID != "6979" {
		t.Fatalf("unexpected identity: %+v", drama)
	}
	if drama.Remark != "全80集" {
		t.Fatalf("remark mismatch: %q", drama.Remark)
	}
	if drama.ReleaseStatus != "finished" {
		t.Fatalf("status mismatch: %q", drama.ReleaseStatus)
	}
	if drama.Cover == "" {
		t.Fatal("cover should be resolved")
	}
}

func TestHuangjuDramaFromMap(t *testing.T) {
	row := map[string]any{
		"id":            "12",
		"slug":          "zongcai",
		"title":         "总裁的替身新娘",
		"totalEpisodes": json.Number("60"),
		"status":        "completed",
		"category":      map[string]any{"name": "都市"},
	}
	drama, err := huangjuDramaFromMap(row, "https://huangju.net", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drama.SourceID != "zongcai-12" {
		t.Fatalf("source id mismatch: %q", drama.SourceID)
	}
	if drama.ReleaseStatus != "finished" || drama.Category != "都市" {
		t.Fatalf("metadata mismatch: %+v", drama)
	}
	if _, err := huangjuDramaFromMap(row, "https://huangju.net", "other-99"); err == nil {
		t.Fatal("mismatched source id should fail")
	}
}
