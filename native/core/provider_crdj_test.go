package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 91 短剧（crdj）回归测试。样本是照站点真实响应结构裁剪的骨架，
// 标题、封面域名、播放域名全部换成中性占位，只验证解析规则。

const crdjCardFixture = `<div class="grid">
<a class="card" href="/duanju/8378-sample-slug/" data-track-item-id="8378" data-track-item-type="video">
  <div class="poster" style="background:#1a1c22">
    <img class="p-img" width="360" height="640" loading="lazy" alt="封面甲" src="data:image/gif;base64,R0lGODlhAQABAAAAACH5BAEKAAEALAAAAAABAAEAAAICTAEAOw==" data-src="https://pic.example.test/cover-a.jpg?auth_key=abc">
    <span class="rank-num">1</span><span class="badge hot">热门</span>
    <span class="eps-flag">短剧 · 7集</span>
    <span class="play-ico"><span></span></span>
  </div>
  <div class="card-info"><h3>测试剧集甲</h3><div class="meta"><span>短剧 · 918热度</span><span class="score">★8.0</span></div></div>
</a>
<a class="card" href="/manju/10502-sample-slug/" data-track-item-id="10502" data-track-item-type="video">
  <div class="poster"><img class="p-img" src="data:image/gif;base64,R0lGODlhAQABAAAAACH5BAEKAAEALAAAAAABAAEAAAICTAEAOw==" data-src="https://pic.example.test/cover-b.jpg"></div>
  <div class="card-info"><h3>测试剧集乙</h3><div class="meta"><span class="score">★9.1</span></div></div>
</a>
<div class="card pm-ssp-card pm-ssp-standalone" data-ssp-shell>广告占位，不应被当成剧集</div>
</div>`

const crdjPlayDataFixture = `<html><body>
<script id="playInitialData" type="application/json">{"title":"测试剧集甲","poster":"https://pic.example.test/poster.jpg","description":"这里是简介","playBase":"/duanju/8378-sample-slug/","detailURL":"/duanju/8378-sample-slug/","eps":[{"n":1,"title":"第一集","videoId":"38108"},{"n":2,"title":"第二集","videoId":"38110"},{"n":3,"title":"第三集","videoId":"38341"},{"n":4,"title":"第四集","videoId":"38342"}]}</script>
</body></html>`

const crdjDetailGridFixture = `<html><body><div class="ep-panel">
<div class="ep-grid"><a data-chunk="0" href="/duanju/8378-sample-slug/1/">1</a><a data-chunk="0" href="/duanju/8378-sample-slug/2/">2</a><a data-chunk="0" href="/duanju/8378-sample-slug/3/">3</a><a data-chunk="0" href="/duanju/8378-sample-slug/4/">4</a></div>
</div></body></html>`

func crdjListFixture(page, pages int) string {
	seed := []map[string]any{
		{"id": 8378, "title": "测试剧集甲", "cover": "https://pic.example.test/cover-a.jpg", "tags": []string{"短剧", "测试"}},
		{"id": 10502, "title": "测试剧集乙", "cover": "https://pic.example.test/cover-b.jpg", "tags": []string{"漫剧"}},
	}
	// 站点固定 20 条一页；末页不足 20 条。
	items := make([]map[string]any, 0, 20)
	if page < pages {
		for index := 0; index < 20; index++ {
			item := map[string]any{}
			for key, value := range seed[index%len(seed)] {
				item[key] = value
			}
			item["id"] = 100000 + index
			items = append(items, item)
		}
	} else {
		items = append(items, seed...)
	}
	payload := map[string]any{
		"status": 1,
		"msg":    "",
		"data": map[string]any{
			"items":      items,
			"pagination": map[string]any{"page": page, "pages": pages, "size": 20, "total": 41},
		},
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

func crdjTestDownloader(site string, client *http.Client) *Downloader {
	return &Downloader{
		cfg:    Config{CrdjURL: site, Retries: 1},
		client: client, limiter: newRequestLimiter(3, 0),
		providerHosts: map[string]string{},
	}
}

func TestCrdjRegistration(t *testing.T) {
	if !isHuangguoProviderSource(sourceCrdj) {
		t.Fatal("crdj 未加入 provider 白名单，会导致播放分集校验失败")
	}
	for _, name := range []string{"crdj", "91crdjai.com", "www.91crdjai.com", "91crdj.co", "aicrdj7.com"} {
		if got := canonicalProviderSource(name); got != sourceCrdj {
			t.Fatalf("canonicalProviderSource(%q) = %q", name, got)
		}
	}
	if got := (&Downloader{cfg: defaultConfig()}).providerBaseURL(sourceCrdj); got != crdjBaseURL {
		t.Fatalf("default base URL = %q", got)
	}
	if !validCrdjCategory("") || !validCrdjCategory("duanju") || !validCrdjCategory("shipin") {
		t.Fatal("合法分类被拒绝")
	}
	if validCrdjCategory("unknown") || validCrdjCategory("../../etc") {
		t.Fatal("非法分类被放行")
	}
	if got := (&Downloader{cfg: defaultConfig(), providerHosts: map[string]string{}}).providerDramaWebURL(sourceCrdj, "8378"); got != crdjBaseURL+"/duanju/8378/" {
		t.Fatalf("web URL = %q", got)
	}
}

func TestCrdjCardParsing(t *testing.T) {
	items := parseCrdjCards(crdjCardFixture, crdjBaseURL, "短剧")
	if len(items) != 2 {
		t.Fatalf("卡片数 = %d，期望 2（广告占位不能算剧集）", len(items))
	}
	first := items[0]
	if first.SourceID != "8378" || first.ID != "crdj:8378" || first.Title != "测试剧集甲" {
		t.Fatalf("首张卡片解析错误：%+v", first)
	}
	if got := nativeText(first.EpisodeCount); got != "7" {
		t.Fatalf("集数 = %q，期望 7", got)
	}
	if first.Score != "8.0" {
		t.Fatalf("评分 = %q", first.Score)
	}
	if got := nativeText(first.Cover); !strings.HasPrefix(got, "https://pic.example.test/cover-a.jpg") {
		t.Fatalf("封面 = %q", got)
	}
	if items[1].SourceID != "10502" {
		t.Fatal("第二张卡片 ID 错误")
	}
}

func TestCrdjJSONParsing(t *testing.T) {
	var envelope crdjEnvelope
	if err := json.Unmarshal([]byte(crdjListFixture(1, 1)), &envelope); err != nil {
		t.Fatal(err)
	}
	items := parseCrdjItems(envelope.Data, crdjBaseURL, "")
	if len(items) != 2 || items[0].SourceID != "8378" || items[0].Title != "测试剧集甲" {
		t.Fatalf("JSON 卡片解析错误：%+v", items)
	}
	if len(items[0].Tags) != 2 {
		t.Fatalf("标签解析错误：%+v", items[0].Tags)
	}
}

func TestCrdjChapters(t *testing.T) {
	chapters, playBase, play := crdjPlayChapters(crdjPlayDataFixture, "8378", crdjBaseURL)
	if play == nil || len(chapters) != 4 {
		t.Fatalf("playInitialData 解析失败：chapters=%d", len(chapters))
	}
	if playBase != "/duanju/8378-sample-slug/" {
		t.Fatalf("playBase = %q", playBase)
	}
	if chapters[0].PageURL != crdjBaseURL+"/duanju/8378-sample-slug/1/" || chapters[3].PageURL != crdjBaseURL+"/duanju/8378-sample-slug/4/" {
		t.Fatalf("分集页地址错误：%q / %q", chapters[0].PageURL, chapters[3].PageURL)
	}
	if chapters[0].ID != "crdj:8378:1" || chapters[0].EpisodeString(1) != "1" {
		t.Fatalf("分集标识错误：%q", chapters[0].ID)
	}
	fallback := crdjDetailChapters(crdjDetailGridFixture, "8378", crdjBaseURL)
	if len(fallback) != 4 {
		t.Fatalf("ep-grid 兜底解析出 %d 集，期望 4", len(fallback))
	}
}

func TestCrdjCatalogAndPlayback(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/videos" || r.URL.Path == "/api/search":
			page := 1
			if r.URL.Query().Get("page") == "2" {
				page = 2
			}
			_, _ = w.Write([]byte(crdjListFixture(page, 3)))
		case strings.HasSuffix(r.URL.Path, "/playback"):
			_, _ = w.Write([]byte(`{"status":1,"msg":"","data":{"manifest_method":"GET","streams":{"main":"` + server.URL + `/media/a.m3u8?auth=1","hevc":"","preview":""},"n":1}}`))
		case r.URL.Path == "/api/videos/8378":
			_, _ = w.Write([]byte(`{"status":1,"data":{"title":"测试剧集甲","cover":"https://pic.example.test/cover-a.jpg","description":"这里是简介","tags":["短剧"],"breadcrumb":[{"name":"短剧","url":"/duanju/"}]}}`))
		case r.URL.Path == "/duanju/8378/1/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(crdjPlayDataFixture))
		case r.URL.Path == "/media/a.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXTINF:10.0,\nseg.ts\n#EXT-X-ENDLIST\n"))
		case strings.HasPrefix(r.URL.Path, "/duanju/") || strings.HasPrefix(r.URL.Path, "/manju/"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(crdjCardFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	d := crdjTestDownloader(server.URL, server.Client())
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		page     int
		category string
		query    string
		wantMore bool
	}{
		{"全部第1页", 1, "", "", true},
		{"全部末页", 3, "", "", false},
		{"搜索", 1, "", "测试", true},
		{"分类短剧", 1, "duanju", "", false},
	} {
		items, more, err := d.fetchCrdjCatalogPage(ctx, tc.page, tc.category, tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(items) == 0 {
			t.Fatalf("%s: 无结果", tc.name)
		}
		if more != tc.wantMore {
			t.Fatalf("%s: hasMore=%v，期望 %v", tc.name, more, tc.wantMore)
		}
	}

	drama, chapters, err := d.fetchCrdjDetail(ctx, "8378")
	if err != nil {
		t.Fatalf("详情：%v", err)
	}
	if drama.Title != "测试剧集甲" || drama.CategoryName != "短剧" || len(drama.Tags) != 1 {
		t.Fatalf("详情元数据错误：%+v", drama)
	}
	if len(chapters) != 4 {
		t.Fatalf("详情分集数 = %d，期望 4", len(chapters))
	}

	media, err := d.resolveCrdjMedia(ctx, Task{DramaID: drama.ID, Chapter: chapters[0], Index: 1})
	if err != nil {
		t.Fatalf("取流：%v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(media.Playlist), "#EXTM3U") {
		t.Fatal("未取到 HLS 播放列表")
	}
	if media.Referer == "" {
		t.Fatal("缺少播放 Referer")
	}
}
