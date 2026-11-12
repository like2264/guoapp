package core

import (
	"testing"
)

// 列表卡片：真实页面上第一张卡片是推广 <aside>，后面才是剧集。
// 这里用简化过的结构做回归，覆盖标题、封面、热度、评分、标签与去重。
const huangtaoListFixture = `<div class="grid">
<article class="drama-card pm-ssp-card pm-ssp-standalone" data-ssp-shell><aside class="pm-ssp-slot"></aside></article>
<article class="drama-card"> <a class="poster-link" href="/drama/30551/" data-track-item-id="30551" data-track-item-type="video" data-track-item-name="样本短剧"> <div class="poster-art"> <img class="poster-img" alt="《样本短剧》封面" src="/static/web/images/drama-loading.png" data-src="https://pic.example.test/upload/sample.jpeg?auth_key=1-0-0-abc" data-preview-id="30551"> </div> <span class="card-badges"><em class="card-score">8.3<i>分</i></em></span> <span class="eps-flag">更新中 · 4集</span> </a> <div class="card-info"> <div> <a href="/drama/30551/"><h3>样本短剧</h3></a> <p>AI短剧 · 都市 <span>· 2.0万 热度</span></p> <p class="card-desc">这是样本剧集的简介。</p> </div> </div> <a class="sr-only" href="/video/30551/">直接播放第 1 集</a> </article>
<article class="drama-card"> <a class="poster-link" href="/drama/29472/" data-track-item-name="另一部短剧"> <img data-src="https://pic.example.test/upload/other.jpeg"> <span class="eps-flag">全 9 集</span> </a> <div class="card-info"><div><a href="/drama/29472/"><h3>另一部短剧</h3></a> <p>AI漫剧 · 古风 <span>· 8600 热度</span></p></div></div> </article>
<article class="drama-card"> <a class="poster-link" href="/drama/30551/" data-track-item-name="重复卡片"> <img data-src="https://pic.example.test/upload/dup.jpeg"> </a> <div class="card-info"><div><h3>重复卡片</h3></div></div> </article>
<article class="drama-card"> <a class="poster-link" href="/tag/anhei/"> <h3>没有剧集编号的推广位</h3> </a> </article>
</div>
<div class="pager"><span class="pager-status" data-pager-status>第 3/46 页</span></div>`

func TestParseHuangtaoCards(t *testing.T) {
	items := parseHuangtaoCards(huangtaoListFixture, "https://aihuangdou.com")
	if len(items) != 2 {
		t.Fatalf("expected 2 dramas, got %d: %+v", len(items), items)
	}
	first := items[0]
	if first.ID != "huangtao:30551" || first.SourceID != "30551" {
		t.Fatalf("unexpected id: %q / %q", first.ID, first.SourceID)
	}
	if first.Title != "样本短剧" || first.Name != "样本短剧" {
		t.Fatalf("unexpected title: %q / %q", first.Title, first.Name)
	}
	if first.Cover != "https://pic.example.test/upload/sample.jpeg?auth_key=1-0-0-abc" {
		t.Fatalf("unexpected cover: %q", first.Cover)
	}
	if first.Heat != "2.0万" {
		t.Fatalf("unexpected heat: %q", first.Heat)
	}
	if first.Score != "8.3分" {
		t.Fatalf("unexpected score: %q", first.Score)
	}
	if first.Remark != "更新中 · 4集" {
		t.Fatalf("unexpected remark: %q", first.Remark)
	}
	if first.CategoryName != "都市" {
		t.Fatalf("unexpected category: %q", first.CategoryName)
	}
	if first.Desc != "这是样本剧集的简介。" {
		t.Fatalf("unexpected desc: %q", first.Desc)
	}
	second := items[1]
	if second.SourceID != "29472" || second.Remark != "全 9 集" || second.Heat != "8600" {
		t.Fatalf("unexpected second card: %+v", second)
	}
	if total := huangtaoTotalPages(huangtaoListFixture); total != 46 {
		t.Fatalf("unexpected total pages: %d", total)
	}
}

func TestHuangtaoListPaths(t *testing.T) {
	cases := []struct {
		category string
		page     int
		want     string
	}{
		{"", 1, "/ai-duanju/"},
		{"", 3, "/ai-duanju/3/"},
		{"c:ai-manju", 1, "/ai-manju/"},
		{"c:ai-mogai", 2, "/ai-mogai/2/"},
		{"t:anhei", 1, "/tag/anhei/"},
		{"t:anhei", 12, "/tag/anhei/12/"},
	}
	for _, entry := range cases {
		if got := huangtaoListPath(entry.category, entry.page); got != entry.want {
			t.Fatalf("huangtaoListPath(%q, %d) = %q, want %q", entry.category, entry.page, got, entry.want)
		}
	}
	for _, invalid := range []string{"fengge", "c:unknown", "t:Anhei", "t:has space", "a/b"} {
		if validHuangtaoCategory(invalid) {
			t.Fatalf("category %q should be rejected", invalid)
		}
	}
	for _, valid := range []string{"", "c:ai-duanju", "t:anhei", "t:renqi-2"} {
		if !validHuangtaoCategory(valid) {
			t.Fatalf("category %q should be accepted", valid)
		}
	}
}

func TestHuangtaoCleanAndIDs(t *testing.T) {
	if got := huangtaoClean("  <b>样本</b>&amp;剧集&nbsp; "); got != "样本 &剧集" {
		t.Fatalf("unexpected cleaned text: %q", got)
	}
	for _, valid := range []string{"1", "30551", "9999999999"} {
		if !validHuangtaoID(valid) {
			t.Fatalf("id %q should be accepted", valid)
		}
	}
	for _, invalid := range []string{"", "0", "12a", "abc", "/video/12"} {
		if validHuangtaoID(invalid) {
			t.Fatalf("id %q should be rejected", invalid)
		}
	}
	if got := huangtaoHeat("AI短剧 · 都市 · 3.1万 热度"); got != "3.1万" {
		t.Fatalf("unexpected heat: %q", got)
	}
	if got := huangtaoHeat("AI短剧 · 都市"); got != "" {
		t.Fatalf("heat should be empty, got %q", got)
	}
}
