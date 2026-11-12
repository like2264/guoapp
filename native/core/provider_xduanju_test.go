package core

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const xduanjuCardFixture = `<ul class="grid">
  <li class="card">
    <a href="/ju/sample-drama">
      <span class="cover">
        <img src="/tu/1989/sample-drama.webp?v=1" alt="样本短剧" width="300" height="400">
        <em class="eps">更新至 11 集</em>
      </span>
      <span class="name">样本短剧</span>
      <span class="card-meta"><b class="new">刚刚更新</b>校园</span>
    </a>
  </li>
  <li class="card">
    <a href="/ju/vip-drama">
      <span class="cover">
        <img src="/tu/1988/vip-drama.webp?v=3" alt="会员短剧" width="300" height="400">
        <b class="tag tag-vip">会员</b>
        <em class="eps">全 3 集</em>
      </span>
      <span class="name">会员短剧</span>
      <span class="card-meta">都市</span>
    </a>
  </li>
  <li class="card">
    <a href="/ju/sample-drama">
      <span class="cover"><em class="eps">重复卡片</em></span>
      <span class="name">重复卡片</span>
    </a>
  </li>
</ul>`

const xduanjuDetailFixture = `<html><head>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"BreadcrumbList","itemListElement":[{"@type":"ListItem","position":1,"name":"首页","item":"https://x-duanju.com/"}]}</script>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"TVSeries","name":"样本短剧","url":"https://x-duanju.com/ju/sample-drama","image":"https://x-duanju.com/tu/1989/sample-drama.webp?v=1","description":"短简介","numberOfEpisodes":3,"genre":["校园","甜宠"],"datePublished":"2026-11-10T15:55:03+08:00"}</script>
</head><body>
<div class="jd-desc" data-desc>
  <p>这是详情页里更完整的简介，共 69 个字。</p>
  <button type="button" class="jd-more" data-more hidden>展开</button>
</div>
<section class="jd-eps" data-eps>
  <ol class="eps-grid eps-main">
    <li data-n="1"><a href="/ju/sample-drama/1" title="样本短剧 第1集" >1</a></li>
    <li data-n="2"><a href="/ju/sample-drama/2" title="样本短剧 第2集" >2</a></li>
    <li data-n="3"><a href="/ju/sample-drama/3" class="on" title="样本短剧 第3集" >3</a></li>
    <li data-n="4"><a href="/ju/other-drama/4" title="别的剧集" >4</a></li>
  </ol>
</section>
</body></html>`

const xduanjuPlayFixture = `<html><body>
<div class="player" data-src="/bf/504117/master.m3u8" data-ep="504117" data-next="/ju/sample-drama/2" data-page="/ju/sample-drama/1" data-ju="30876" data-n="1"></div>
</body></html>`

func xduanjuFixtureDownloader(t *testing.T, routes map[string]string) *Downloader {
	t.Helper()
	d := sourceFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "xduanju.test" {
			t.Fatalf("unexpected host: %s", request.URL.String())
		}
		body, ok := routes[request.URL.Path]
		if !ok {
			return sourceFixtureResponse(request, http.StatusNotFound, "not found"), nil
		}
		return sourceFixtureResponse(request, http.StatusOK, body), nil
	})
	d.cfg.XduanjuURL = "https://xduanju.test"
	return d
}

func TestXduanjuCardParsing(t *testing.T) {
	items := parseXduanjuCards(xduanjuCardFixture, xduanjuBaseURL)
	if len(items) != 2 {
		t.Fatalf("expected 2 unique cards, got %d", len(items))
	}
	first := items[0]
	if first.SourceID != "sample-drama" || first.ID != "xduanju:sample-drama" || first.Title != "样本短剧" {
		t.Fatalf("first card parsed incorrectly: %+v", first)
	}
	if first.Cover != "https://x-duanju.com/tu/1989/sample-drama.webp?v=1" || first.CoverURL != first.Cover {
		t.Fatalf("cover was not resolved: %q", first.Cover)
	}
	if first.Remark != "更新至 11 集" {
		t.Fatalf("unexpected remark: %q", first.Remark)
	}
	if first.VIP != nil {
		t.Fatal("non-vip card was flagged as vip")
	}
	if second := items[1]; second.SourceID != "vip-drama" || second.VIP == nil || !*second.VIP {
		t.Fatalf("vip card parsed incorrectly: %+v", second)
	}
}

func TestXduanjuListPathsAndNextPage(t *testing.T) {
	cases := []struct {
		category string
		page     int
		want     string
	}{
		{"", 1, "/zuixin"},
		{"", 3, "/zuixin/3"},
		{"xiao-yuan", 1, "/fenlei/xiao-yuan"},
		{"xiao-yuan", 2, "/fenlei/xiao-yuan/2"},
		{xduanjuDoneCategory, 2, "/wanjie/2"},
	}
	for _, item := range cases {
		if got := xduanjuListPath(item.category, item.page); got != item.want {
			t.Fatalf("xduanjuListPath(%q, %d) = %q, want %q", item.category, item.page, got, item.want)
		}
	}
	firstPage := `<a href="/zuixin/2">下一页</a><a href="/zuixin/79">尾页</a>`
	if !xduanjuHasNextPage(firstPage, "", 1) {
		t.Fatal("next page link was missed on the first page")
	}
	if xduanjuHasNextPage(`<a href="/zuixin/10">尾页</a>`, "", 1) {
		t.Fatal("a jump-to-last link was mistaken for the next page")
	}
	if xduanjuHasNextPage(`<a href="/zuixin/1">上一页</a>`, "", 79) {
		t.Fatal("last page was reported as having a next page")
	}
	if !xduanjuHasNextPage(`<a href="/fenlei/xiao-yuan/3">下一页</a>`, "xiao-yuan", 2) {
		t.Fatal("category next page link was missed")
	}
}

func TestXduanjuCategoriesPrependDoneSection(t *testing.T) {
	body := `<main class="wrap">
  <ul class="fl-grid">
    <li><a class="fl-tile" href="/fenlei/hou-gong"><span class="fl-fan"></span><span class="fl-name">后宫<small>12</small></span><span class="fl-count">共 12 部</span></a></li>
    <li><a class="fl-tile" href="/fenlei/xiao-yuan"><span class="fl-name">校园</span></a></li>
  </ul>
</main>`
	d := xduanjuFixtureDownloader(t, map[string]string{"/fenlei": body})
	categories, err := d.fetchXduanjuCategories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(categories) != 3 {
		t.Fatalf("unexpected category count: %+v", categories)
	}
	if categories[0].ID != xduanjuDoneCategory || categories[0].Name != "完结" {
		t.Fatalf("done section must lead the list: %+v", categories[0])
	}
	if categories[1].ID != "hou-gong" || categories[1].Name != "后宫" {
		t.Fatalf("genre name was not cleaned: %+v", categories[1])
	}
	if categories[2].ID != "xiao-yuan" || categories[2].Name != "校园" {
		t.Fatalf("genre tile parsed incorrectly: %+v", categories[2])
	}
	if !validNativeCategory(sourceXduanju, xduanjuDoneCategory) || validNativeCategory(sourceXduanju, "Bad Slug") {
		t.Fatal("category validation changed")
	}
}

func TestXduanjuCatalogUsesPathPagination(t *testing.T) {
	var sawPath string
	d := sourceFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		sawPath = request.URL.Path
		return sourceFixtureResponse(request, http.StatusOK, xduanjuCardFixture+`<a href="/fenlei/xiao-yuan/3">下一页</a>`), nil
	})
	d.cfg.XduanjuURL = "https://xduanju.test"
	items, more, err := d.fetchXduanjuCatalogPage(context.Background(), 2, "xiao-yuan", "")
	if err != nil {
		t.Fatal(err)
	}
	if sawPath != "/fenlei/xiao-yuan/2" {
		t.Fatalf("category pagination used the wrong path: %s", sawPath)
	}
	if len(items) != 2 || !more {
		t.Fatalf("unexpected catalog page: items=%d more=%t", len(items), more)
	}
}

func TestXduanjuSearchUsesQParameterAndDetectsEmptyState(t *testing.T) {
	var query string
	d := sourceFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/sousuo" {
			t.Fatalf("unexpected search path: %s", request.URL.Path)
		}
		query = request.URL.Query().Get("q")
		if query == "不存在" {
			return sourceFixtureResponse(request, http.StatusOK, `<div class="ss-empty"><p class="ss-empty-t">没有找到相关短剧</p></div>`), nil
		}
		return sourceFixtureResponse(request, http.StatusOK, xduanjuCardFixture), nil
	})
	d.cfg.XduanjuURL = "https://xduanju.test"
	items, more, err := d.fetchXduanjuCatalogPage(context.Background(), 1, "", "样本")
	if err != nil || query != "样本" || len(items) != 2 || more {
		t.Fatalf("search request was malformed: query=%q items=%d more=%t err=%v", query, len(items), more, err)
	}
	empty, more, err := d.searchXduanju(context.Background(), 1, "不存在")
	if err != nil || len(empty) != 0 || more {
		t.Fatalf("empty search state was not detected: items=%d more=%t err=%v", len(empty), more, err)
	}
}

func TestXduanjuDetailParsing(t *testing.T) {
	d := xduanjuFixtureDownloader(t, map[string]string{"/ju/sample-drama": xduanjuDetailFixture})
	drama, chapters, err := d.fetchXduanjuDetail(context.Background(), "sample-drama")
	if err != nil {
		t.Fatal(err)
	}
	if drama.Title != "样本短剧" || drama.EpisodeCount != 3 || drama.OnlineDate != "2026-11-10" {
		t.Fatalf("detail drama parsed incorrectly: %+v", drama)
	}
	if !strings.Contains(drama.Desc, "更完整的简介") {
		t.Fatalf("synopsis did not prefer the detail block: %q", drama.Desc)
	}
	if drama.CategoryName != "校园" || len(drama.Tags) != 2 {
		t.Fatalf("genres were not mapped to tags: %+v tags=%v", drama.CategoryName, drama.Tags)
	}
	if len(chapters) != 3 {
		t.Fatalf("cross-drama episode leaked into the list: %+v", chapters)
	}
	first := chapters[0]
	if first.Title != "样本短剧 第1集" || first.VideoURL != "xduanju-play://1" {
		t.Fatalf("chapter parsed incorrectly: %+v", first)
	}
	if first.PageURL != "https://xduanju.test/ju/sample-drama/1" || first.ID != "xduanju:sample-drama:1" {
		t.Fatalf("chapter identity parsed incorrectly: %+v", first)
	}
}

func TestXduanjuMediaResolvesPlaylistFromPlayPage(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1286286,RESOLUTION=1280x720,NAME=\"720p\"\n720p/index.m3u8?n=fixture\n"
	d := xduanjuFixtureDownloader(t, map[string]string{
		"/ju/sample-drama/2":     xduanjuPlayFixture,
		"/bf/504117/master.m3u8": playlist,
	})
	media, err := d.resolveXduanjuMedia(context.Background(), Task{
		DramaID: providerDramaID(sourceXduanju, "sample-drama"),
		Chapter: Chapter{ID: providerChapterID(sourceXduanju, "sample-drama", "2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if media.URL != "https://xduanju.test/bf/504117/master.m3u8" || !strings.HasPrefix(media.Playlist, "#EXTM3U") {
		t.Fatalf("playback was not resolved: %+v", media)
	}
	if _, err := d.resolveXduanjuMedia(context.Background(), Task{
		DramaID: providerDramaID(sourceXduanju, "sample-drama"),
		Chapter: Chapter{ID: providerChapterID(sourceXduanju, "missing-drama", "1")},
	}); err == nil {
		t.Fatal("mismatched chapter identity was accepted")
	}
}
