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
