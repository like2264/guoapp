package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 柚子短剧（youzidj）回归测试。样本按站点真实响应结构裁剪，标题、封面、
// 视频 ID 全部换成中性占位，只验证解析与凭证链路。

const youzidjListFixture = `<html><body><div class="drama-grid">
<article class="drama-card pomelo-drama-card" data-analytics-work="w-1"><a class="home-drama-card-link" aria-label="查看详情：测试剧集甲" href="/dramas/bai-jie/season-1/"><div class="drama-card-cover"><div class="drama-poster"><picture><img src="/catalogue-art/cover-a-960.webp" alt="测试剧集甲海报"/></picture><div class="home-card-bottom"><span class="home-card-episodes"><span class="home-card-episodes-long">更新至36集</span><span class="home-card-episodes-short">至36集</span></span></div></div></div><div class="drama-card-copy"><p class="drama-card-title" title="测试剧集甲">测试剧集甲</p></div></a></article>
<article class="drama-card pomelo-drama-card" data-content-type="face_swap"><a href="https://youzidj.com/ai-face-swap/g4af0f22/"><img src="https://pic.example.test/cover-b.webp"/><div class="drama-card-copy"><p class="drama-card-title" title="单片乙">单片乙</p></div></a></article>
<article class="drama-card"><div class="ad-slot">广告位，不应被当成剧集</div></article>
</div><nav aria-label="分页"><a rel="next" class="pagination-control" href="/dramas/page/2/">下一页</a></nav></body></html>`

const youzidjSearchFixture = `<div data-pages="2"><article class="drama-card"><a href="/dramas/mama-ni-hao/season-1/"><img src="/catalogue-art/cover-c-960.webp"/><p class="drama-card-title" title="妈妈你好">妈妈你好</p></a></article></div>`

const youzidjSeriesFixture = `<html><head>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"TVSeries","name":"测试剧集甲","description":"这里是简介","url":"https://youzidj.com/dramas/bai-jie/","image":"https://pic.example.test/cover-a.webp","numberOfEpisodes":3,"numberOfSeasons":2}</script>
</head><body>
<a href="/tags/romance/">言情</a><a href="/tags/urban/">都市</a>
<div class="episode-grid">
<a href="/dramas/bai-jie/season-1-episode-1/">1</a>
<a href="/dramas/bai-jie/season-1-episode-2/">2</a>
<a href="/dramas/bai-jie/season-2-episode-1/">1</a>
<a href="/dramas/bai-jie/season-2-episode-2/">2</a>
</div></body></html>`

const youzidjSingleFixture = `<html><head>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"VideoObject","name":"单片乙","description":"单片简介","thumbnailUrl":["https://pic.example.test/cover-b.webp"],"contentUrl":"https://youzidj.com/media/video-b/master.m3u8"}</script>
</head><body><section class="watch-player" id="player" data-video="video-b" data-work="work-b" data-version="ver-b" data-price="0"><video id="video"></video></section></body></html>`

const youzidjEpisodeFixture = `<html><body><section class="watch-player" id="player" tabindex="-1" data-video="video-a" data-work="work-a" data-version="ver-a" data-price="0"><video id="video"></video></section></body></html>`

const youzidjMasterPlaylist = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-STREAM-INF:BANDWIDTH=9442288,RESOLUTION=1280x720
delivery-v2/720/playlist.m3u8
`

func youzidjTestDownloader(site string, client *http.Client) *Downloader {
	return &Downloader{
		cfg:           Config{YouzidjURL: site, Retries: 1},
		client:        client,
		limiter:       newRequestLimiter(3, 0),
		providerHosts: map[string]string{},
	}
}

func TestYouzidjRegistration(t *testing.T) {
	if !isHuangguoProviderSource(sourceYouzidj) {
		t.Fatalf("youzidj should be a provider source")
	}
	if got := canonicalProviderSource("www.youzidj.com"); got != sourceYouzidj {
		t.Fatalf("canonical source = %q", got)
	}
	if got := (&Downloader{cfg: defaultConfig()}).providerBaseURL(sourceYouzidj); got != youzidjBaseURL {
		t.Fatalf("base url = %q", got)
	}
	if got := (&Downloader{cfg: defaultConfig(), providerHosts: map[string]string{}}).providerDramaWebURL(sourceYouzidj, "dramas/bai-jie"); got != youzidjBaseURL+"/dramas/bai-jie/" {
		t.Fatalf("web url = %q", got)
	}
	if got := (&Downloader{cfg: defaultConfig(), providerHosts: map[string]string{}}).providerDramaWebURL(sourceYouzidj, "ai-face-swap/g4af0f22"); got != youzidjBaseURL+"/ai-face-swap/g4af0f22/" {
		t.Fatalf("single web url = %q", got)
	}
	for _, category := range []string{"", "dramas", "face-swap", "adult"} {
		if !validYouzidjCategory(category) {
			t.Fatalf("category %q should be valid", category)
		}
	}
	if validYouzidjCategory("unknown") {
		t.Fatalf("unknown category should be rejected")
	}
	for _, id := range []string{"dramas/bai-jie", "ai-face-swap/g4af0f22", "ai-adult/a656b991"} {
		if !validYouzidjSourceID(id) {
			t.Fatalf("source id %q should be valid", id)
		}
	}
	for _, id := range []string{"", "dramas/", "../etc", "ai-face-swap/x", "videos/1"} {
		if validYouzidjSourceID(id) {
			t.Fatalf("source id %q should be rejected", id)
		}
	}
	items, err := (&Downloader{}).fetchYouzidjCategories(context.Background())
	if err != nil || len(items) != 3 || items[0].ID != youzidjCategoryDramas {
		t.Fatalf("categories = %+v err=%v", items, err)
	}
}

func TestYouzidjCatalogAndSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/dramas/":
			response.Write([]byte(youzidjListFixture))
		case request.URL.Path == "/api/catalogue/search":
			if request.URL.Query().Get("q") != "妈妈" || request.URL.Query().Get("kind") != "works" {
				t.Errorf("unexpected search query: %s", request.URL.RawQuery)
			}
			response.Write([]byte(youzidjSearchFixture))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := youzidjTestDownloader(server.URL, server.Client())

	items, more, err := d.fetchYouzidjCatalogPage(context.Background(), 1, youzidjCategoryDramas, "")
	if err != nil || len(items) != 2 {
		t.Fatalf("catalog items=%d err=%v", len(items), err)
	}
	if !more {
		t.Fatalf("catalog should report more pages")
	}
	first := items[0]
	if first.SourceID != "dramas/bai-jie" || first.Title != "测试剧集甲" {
		t.Fatalf("first item = %+v", first)
	}
	if first.CategoryName != "AI短剧" || first.Remark != "更新至36集" {
		t.Fatalf("first item meta = %q / %q", first.CategoryName, first.Remark)
	}
	if !strings.HasPrefix(first.Cover.(string), server.URL+"/catalogue-art/") {
		t.Fatalf("cover should be absolute: %v", first.Cover)
	}
	if items[1].SourceID != "ai-face-swap/g4af0f22" || items[1].CategoryName != "AI换脸" {
		t.Fatalf("second item = %+v", items[1])
	}

	found, foundMore, err := d.fetchYouzidjCatalogPage(context.Background(), 1, "", "妈妈")
	if err != nil || len(found) != 1 || !foundMore {
		t.Fatalf("search items=%d more=%v err=%v", len(found), foundMore, err)
	}
	if found[0].SourceID != "dramas/mama-ni-hao" {
		t.Fatalf("search item = %+v", found[0])
	}
}

func TestYouzidjSeriesAndSingleDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/dramas/bai-jie/":
			response.Write([]byte(youzidjSeriesFixture))
		case "/ai-face-swap/g4af0f22/":
			response.Write([]byte(youzidjSingleFixture))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := youzidjTestDownloader(server.URL, server.Client())

	drama, chapters, err := d.fetchYouzidjDetail(context.Background(), "dramas/bai-jie")
	if err != nil || len(chapters) != 4 {
		t.Fatalf("series chapters=%d err=%v", len(chapters), err)
	}
	if drama.Title != "测试剧集甲" || fmt.Sprint(drama.EpisodeCount) != "4" {
		t.Fatalf("series drama = %+v", drama)
	}
	if len(drama.Tags) != 2 || drama.Tags[0] != "言情" {
		t.Fatalf("series tags = %v", drama.Tags)
	}
	// 多季剧按 (季, 集) 排序并带上季号。
	if chapters[0].Title != "第1季 第1集" || chapters[3].Title != "第2季 第2集" {
		t.Fatalf("chapter titles = %q ... %q", chapters[0].Title, chapters[3].Title)
	}
	if chapters[0].PageURL != server.URL+"/dramas/bai-jie/season-1-episode-1/" {
		t.Fatalf("chapter page url = %q", chapters[0].PageURL)
	}
	if got := chapters[1].EpisodeString(0); got != "2" {
		t.Fatalf("chapter episode = %q", got)
	}

	single, singleChapters, err := d.fetchYouzidjDetail(context.Background(), "ai-face-swap/g4af0f22")
	if err != nil || len(singleChapters) != 1 {
		t.Fatalf("single chapters=%d err=%v", len(singleChapters), err)
	}
	if single.Title != "单片乙" || single.CategoryName != "AI换脸" {
		t.Fatalf("single drama = %+v", single)
	}
	if singleChapters[0].PageURL != server.URL+"/ai-face-swap/g4af0f22/" {
		t.Fatalf("single chapter page url = %q", singleChapters[0].PageURL)
	}
}

func TestYouzidjPlaybackFlow(t *testing.T) {
	var bootstrapCalls, playbackCalls int32
	var sawPlayCookie, sawGuestCookie int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/dramas/bai-jie/season-1-episode-1/":
			response.Write([]byte(youzidjEpisodeFixture))
		case request.URL.Path == "/api/account/bootstrap":
			atomic.AddInt32(&bootstrapCalls, 1)
			http.SetCookie(response, &http.Cookie{Name: youzidjAccountCookie, Value: "guest-token", Path: "/", HttpOnly: true, Secure: true})
			response.Write([]byte(`{"id":"guest","guest":true}`))
		case strings.HasPrefix(request.URL.Path, "/api/playback/"):
			atomic.AddInt32(&playbackCalls, 1)
			if !strings.Contains(request.Header.Get("Cookie"), youzidjAccountCookie+"=guest-token") {
				t.Errorf("playback missing guest cookie: %q", request.Header.Get("Cookie"))
				response.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.SetCookie(response, &http.Cookie{Name: "eden_play_video-a", Value: "play-token", Path: "/media/video-a/", HttpOnly: true, Secure: true})
			payload, _ := json.Marshal(map[string]any{
				"url":        "/media/video-a/ver-a/adaptive-cmaf-sample.m3u8",
				"expires_at": time.Now().Add(10 * time.Minute).Unix(),
			})
			response.Write(payload)
		case request.URL.Path == "/media/video-a/ver-a/adaptive-cmaf-sample.m3u8":
			cookie := request.Header.Get("Cookie")
			if strings.Contains(cookie, "eden_play_video-a=play-token") {
				atomic.AddInt32(&sawPlayCookie, 1)
			}
			if strings.Contains(cookie, youzidjAccountCookie+"=guest-token") {
				atomic.AddInt32(&sawGuestCookie, 1)
			}
			response.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			response.Write([]byte(youzidjMasterPlaylist))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := youzidjTestDownloader(server.URL, server.Client())

	media, err := d.resolveYouzidjMedia(context.Background(), Task{
		DramaID: providerDramaID(sourceYouzidj, "dramas/bai-jie"),
		Chapter: Chapter{
			ID:      providerChapterID(sourceYouzidj, "dramas/bai-jie", "season-1-episode-1"),
			Source:  sourceYouzidj,
			PageURL: server.URL + "/dramas/bai-jie/season-1-episode-1/",
		},
	})
	if err != nil {
		t.Fatalf("resolve media: %v", err)
	}
	if !strings.Contains(media.Playlist, "#EXTM3U") {
		t.Fatalf("playlist not fetched: %q", media.Playlist)
	}
	if !strings.HasSuffix(media.URL, "/media/video-a/ver-a/adaptive-cmaf-sample.m3u8") {
		t.Fatalf("media url = %q", media.URL)
	}
	if media.credentials == nil || !strings.Contains(media.credentials.cookie, "eden_play_video-a=play-token") {
		t.Fatalf("credentials = %+v", media.credentials)
	}
	if atomic.LoadInt32(&sawPlayCookie) == 0 || atomic.LoadInt32(&sawGuestCookie) == 0 {
		t.Fatalf("media request should carry both cookies, play=%d guest=%d", sawPlayCookie, sawGuestCookie)
	}
	if atomic.LoadInt32(&bootstrapCalls) != 1 {
		t.Fatalf("bootstrap calls = %d", bootstrapCalls)
	}

	// 第二次播放应复用缓存的访客凭证，不再重复 bootstrap。
	if _, err := d.resolveYouzidjMedia(context.Background(), Task{
		DramaID: providerDramaID(sourceYouzidj, "dramas/bai-jie"),
		Chapter: Chapter{
			ID:      providerChapterID(sourceYouzidj, "dramas/bai-jie", "season-1-episode-1"),
			Source:  sourceYouzidj,
			PageURL: server.URL + "/dramas/bai-jie/season-1-episode-1/",
		},
	}); err != nil {
		t.Fatalf("second resolve media: %v", err)
	}
	if atomic.LoadInt32(&bootstrapCalls) != 1 {
		t.Fatalf("guest cookie should be cached, bootstrap calls = %d", bootstrapCalls)
	}
	if atomic.LoadInt32(&playbackCalls) != 2 {
		t.Fatalf("playback calls = %d", playbackCalls)
	}
}

func TestYouzidjPlaybackRefreshesStaleGuest(t *testing.T) {
	var bootstrapCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/dramas/bai-jie/season-1-episode-1/":
			response.Write([]byte(youzidjEpisodeFixture))
		case request.URL.Path == "/api/account/bootstrap":
			atomic.AddInt32(&bootstrapCalls, 1)
			http.SetCookie(response, &http.Cookie{Name: youzidjAccountCookie, Value: "fresh-guest", Path: "/"})
			response.Write([]byte(`{"guest":true}`))
		case strings.HasPrefix(request.URL.Path, "/api/playback/"):
			// 旧凭证第一次被拒绝，重新领取后放行。
			if !strings.Contains(request.Header.Get("Cookie"), "fresh-guest") {
				response.WriteHeader(http.StatusUnauthorized)
				response.Write([]byte(`{"error":"请刷新页面以恢复账户"}`))
				return
			}
			payload, _ := json.Marshal(map[string]any{
				"url":        "/media/video-a/ver-a/adaptive-cmaf-sample.m3u8",
				"expires_at": time.Now().Add(10 * time.Minute).Unix(),
			})
			response.Write(payload)
		case request.URL.Path == "/media/video-a/ver-a/adaptive-cmaf-sample.m3u8":
			response.Write([]byte(youzidjMasterPlaylist))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := youzidjTestDownloader(server.URL, server.Client())

	// 预置一个过期凭证，模拟缓存里的访客会话失效。
	d.youzidjGuest = "stale-guest"
	d.youzidjGuestAt = time.Now()

	if _, err := d.resolveYouzidjMedia(context.Background(), Task{
		DramaID: providerDramaID(sourceYouzidj, "dramas/bai-jie"),
		Chapter: Chapter{
			ID:      providerChapterID(sourceYouzidj, "dramas/bai-jie", "season-1-episode-1"),
			Source:  sourceYouzidj,
			PageURL: server.URL + "/dramas/bai-jie/season-1-episode-1/",
		},
	}); err != nil {
		t.Fatalf("resolve media after refresh: %v", err)
	}
	if atomic.LoadInt32(&bootstrapCalls) != 1 {
		t.Fatalf("should re-bootstrap once, calls = %d", bootstrapCalls)
	}
}

func TestYouzidjRejectsMismatchedChapter(t *testing.T) {
	d := youzidjTestDownloader("https://example.test", http.DefaultClient)
	if _, err := d.resolveYouzidjMedia(context.Background(), Task{
		DramaID: providerDramaID(sourceCrdj, "dramas/bai-jie"),
		Chapter: Chapter{ID: providerChapterID(sourceCrdj, "dramas/bai-jie", "season-1-episode-1")},
	}); err == nil {
		t.Fatalf("mismatched source should be rejected")
	}
	if _, _, err := d.fetchYouzidjDetail(context.Background(), "unknown/path"); err == nil {
		t.Fatalf("invalid source id should be rejected")
	}
}
