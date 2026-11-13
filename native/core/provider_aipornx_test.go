package core

import "testing"

func TestAipornxCardsParseHTMLFixture(t *testing.T) {
	html := `<div class="vod-grid">
<article class="vod-card">
  <a href="/detail/3583/"><div class="vod-pic"><img data-original="https://media.hgpicimg.cc//upload/huangguo/13267.jpg" alt="测试短剧"></div></a>
  <span class="vod-remarks">更新至5集</span>
  <h3 class="vod-name"><a href="/detail/3583/">测试短剧</a></h3>
</article>
</div>`
	items := parseAipornxCards(html, "https://aipornx.cc/aidj/anthurium/", "AI成人短剧")
	if len(items) != 1 {
		t.Fatalf("expected one card, got %d", len(items))
	}
	item := items[0]
	if item.ID != "aipornx:3583" || item.SourceID != "3583" || item.Title != "测试短剧" {
		t.Fatalf("unexpected card identity/title: %+v", item)
	}
	if item.CoverURL != "https://media.hgpicimg.cc//upload/huangguo/13267.jpg" {
		t.Fatalf("unexpected cover: %v", item.CoverURL)
	}
	if item.Remark != "更新至5集" || item.CategoryName != "AI成人短剧" {
		t.Fatalf("unexpected card metadata: %+v", item)
	}
}

func TestAipornxPlayURLPattern(t *testing.T) {
	match := reAipornxPlay.FindStringSubmatch("https://aipornx.cc/play/3583-1-1/")
	if len(match) != 4 || match[1] != "3583" || match[2] != "1" || match[3] != "1" {
		t.Fatalf("unexpected episode route parse: %#v", match)
	}
}
