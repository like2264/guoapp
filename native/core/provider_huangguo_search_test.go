package core

import "testing"

// 黄果视频的搜索结果页把卡片放在普通 <article> 里，没有 video-card class。
// 解析器必须能退到「带剧集链接的任意 article」，否则搜索永远返回 0 条。
func TestHuangguoVideoCardsParseSearchLayout(t *testing.T) {
	page := `<article class="group"><a href="/video/v9" class="focus-ring"><img alt="示例剧甲" src="/a.jpg"><div><h2>示例剧甲</h2></div></a></article>` +
		`<article class="group"><a href="/series/s8"><img alt="示例剧乙" src="/b.jpg"><h2>示例剧乙</h2></a></article>` +
		`<article class="promo"><a href="/ad">广告位</a></article>`
	cards := parseHuangguoVideoCards(page, huangguoVideoBaseURL+"/search?q=x")
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want 2", len(cards))
	}
	want := map[string]string{"video/v9": "示例剧甲", "series/s8": "示例剧乙"}
	for _, card := range cards {
		if title, ok := want[card.SourceID]; !ok || card.Title != title {
			t.Fatalf("%s: title = %q", card.SourceID, card.Title)
		}
		if card.Source != sourceHuangguoVideo {
			t.Fatalf("%s: source = %q", card.SourceID, card.Source)
		}
	}
}

// 没有 article 包裹时，退到直接用剧集链接当块，标题仍可取自链接内部。
func TestHuangguoVideoCardsParseBareLinks(t *testing.T) {
	page := `<div class="grid"><a href="/video/v1"><img alt="示例短片" src="/c.jpg"></a></div>` +
		`<div class="grid"><a href="/series/s2"><img alt="示例连续剧" src="/d.jpg"></a></div>`
	cards := parseHuangguoVideoCards(page, huangguoVideoBaseURL+"/search?q=y")
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want 2", len(cards))
	}
	if cards[0].Title != "示例短片" || cards[1].Title != "示例连续剧" {
		t.Fatalf("titles = %q, %q", cards[0].Title, cards[1].Title)
	}
}

// 搜索页的分页参数是 content_page，不是 page。搜「妈妈」时站点自报 54 部，
// 三页分别是 24/24/6 条；越界请求 content_page=4 会被钳回第 3 页并返回空列表。
// 下面四段就是这三页分页器的真实结构（含越界页被钳回后的形态）。
func TestHuangguoVideoSearchPagination(t *testing.T) {
	page1 := `<nav><span aria-current="page">1</span>` +
		`<a href="/search?q=%E5%A6%88%E5%A6%88&amp;content_page=2" class="pager-btn pager-page">2</a>` +
		`<a href="/search?q=%E5%A6%88%E5%A6%88&amp;content_page=2" class="pager-btn pager-dir" rel="next">下一页</a></nav>`
	page2 := `<nav><a href="/search?q=x&amp;content_page=2" aria-current="page">2</a>` +
		`<a href="/search?q=x&amp;content_page=3" class="pager-btn pager-page">3</a>` +
		`<a href="/search?q=x&amp;content_page=3" class="pager-btn pager-dir" rel="next">下一页</a></nav>`
	page3 := `<nav><a href="/search?q=x&amp;content_page=2" class="pager-btn pager-page">2</a>` +
		`<span aria-current="page">3</span></nav>`

	if !huangguoVideoHasNextPage(page1, 1) {
		t.Fatal("第 1 页应判定为还有下一页")
	}
	if !huangguoVideoHasNextPage(page2, 2) {
		t.Fatal("第 2 页应判定为还有下一页")
	}
	if huangguoVideoHasNextPage(page3, 3) {
		t.Fatal("第 3 页应判定为到底")
	}
	if huangguoVideoHasNextPage(page3, 4) {
		t.Fatal("越界页（被钳回第 3 页）应判定为到底，不能无限翻")
	}
	// 只有 rel="next"、没有可解析的 content_page 数字时也要认。
	if !huangguoVideoHasNextPage(`<a href="/search?q=x" rel="next">下一页</a>`, 1) {
		t.Fatal("仅有 rel=next 时应判定为还有下一页")
	}
}
