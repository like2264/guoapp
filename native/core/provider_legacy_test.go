package core

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fixtureLegacyProtocol = legacyProtocol{InterfaceKey: "fixture-interface", ParamKey: "0123456789abcdef", ParamIV: "fedcba9876543210"}

func legacyFixtureDownloader(t *testing.T, transport sourceFixtureTransport) *Downloader {
	t.Helper()
	d := sourceFixtureDownloader(t, transport)
	d.cfg.APIBase = "https://legacy.example.test"
	d.cfg.MaxPagesPerSort, d.cfg.PageSize = 1, 3
	return d
}

func legacyFixtureResponse(request *http.Request, payload any) *http.Response {
	body, _ := json.Marshal(map[string]any{"code": 200, "hash": false, "data": payload})
	return sourceFixtureResponse(request, 200, string(body))
}

func legacyFixtureLogin(t *testing.T, request *http.Request) {
	t.Helper()
	var body struct {
		Data string `json:"data"`
	}
	if request.Method != http.MethodPost || request.Header.Get("Authorization") != "" || json.NewDecoder(request.Body).Decode(&body) != nil {
		t.Error("anonymous login must use an encrypted POST without another user's token")
		return
	}
	encrypted, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil || len(encrypted) == 0 || len(encrypted)%16 != 0 {
		t.Error("invalid encrypted login")
		return
	}
	block, _ := aes.NewCipher([]byte(fixtureLegacyProtocol.ParamKey))
	cipher.NewCBCDecrypter(block, []byte(fixtureLegacyProtocol.ParamIV)).CryptBlocks(encrypted, encrypted)
	plain, err := pkcs7Unpad(encrypted, 16)
	var fields struct {
		Device string `json:"devID"`
		System string `json:"sysType"`
		Store  bool   `json:"isAppStore"`
	}
	if err != nil || json.Unmarshal(plain, &fields) != nil || len(fields.Device) != 29 || fields.System != "ios" || fields.Store ||
		!strings.Contains(request.Header.Get("X-User-Agent"), "DevID="+fields.Device+";") {
		t.Error("login must carry a fresh consistent H5 device identity")
	}
}

func TestLegacyAnonymousCatalogAndSessionReuse(t *testing.T) {
	var loginCalls, scriptCalls atomic.Int32
	transport := sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/":
			return sourceFixtureResponse(request, 200, `<script src="https://unrelated.example/main-bad.js"></script><script src="/js/main-fixture.js"></script>`), nil
		case "/js/main-fixture.js":
			scriptCalls.Add(1)
			return sourceFixtureResponse(request, 200, `t(F,"interfaceKey","fixture-interface");t(F,"parameterKey","0123456789abcdef");t(F,"parameterIv","fedcba9876543210")`), nil
		case "/api/app/mine/login/h5":
			loginCalls.Add(1)
			legacyFixtureLogin(t, request)
			return legacyFixtureResponse(request, map[string]string{"token": "own-anonymous-session"}), nil
		case "/api/app/playlet-tab/all":
			if request.Header.Get("Authorization") != "own-anonymous-session" {
				t.Error("catalog lost its session")
			}
			return legacyFixtureResponse(request, map[string]any{"list": []Tab{{ID: "current-tab", Name: "当前分类"}}}), nil
		case "/api/app/playlet/home/tab/current-tab":
			return legacyFixtureResponse(request, map[string]any{"list": []Drama{{ID: "current-drama", Title: "无图剧库样本", TotalEpisode: 2}}}), nil
		case "/api/app/playlet-recommend/home/choose":
			return legacyFixtureResponse(request, map[string]any{"list": []Drama{}}), nil
		case "/api/app/playlet/detail/current-drama":
			return legacyFixtureResponse(request, map[string]any{"title": "无图剧库样本", "chapters": []Chapter{{ID: "episode-1", VideoURL: "opaque-video-id"}}}), nil
		default:
			t.Errorf("unexpected request blocked: %s", request.URL.Path)
			return nil, errors.New("unexpected endpoint")
		}
	})
	d := legacyFixtureDownloader(t, transport)
	items, _, err := d.fetchLegacyCatalogPage(context.Background(), 1)
	if err != nil || len(items) != 1 || items[0].Source != "cloudfront" || items[0].ChannelName != "当前分类" {
		t.Fatalf("catalog: count=%d error=%v", len(items), err)
	}
	_, chapters, err := d.fetchLegacyChapters(context.Background(), "current-drama")
	if err != nil || len(chapters) != 1 {
		t.Fatalf("chapters: count=%d error=%v", len(chapters), err)
	}
	if d.cfg.Token != "" || d.cfg.AESKeyHex != "" || d.cfg.InterfaceKey != "" {
		t.Fatal("automatic session changed user configuration")
	}
	info, err := os.Stat(filepath.Join(d.cfg.dataDir, "huangguo-session.json"))
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("anonymous session must be private", err)
	}
	restarted := &Downloader{cfg: d.cfg, providerHosts: map[string]string{}}
	restarted.client = &http.Client{Transport: transport}
	restarted.limiter = newRequestLimiter(8, time.Nanosecond)
	if _, err := restarted.legacyCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if loginCalls.Load() != 1 || scriptCalls.Load() != 1 {
		t.Fatal("restart did not reuse the anonymous session and public protocol")
	}
}

func TestLegacyLoginWaiterCancellationAndSingleFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var logins atomic.Int32
	d := legacyFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		logins.Add(1)
		close(started)
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return legacyFixtureResponse(request, map[string]string{"token": "shared-own-session"}), nil
	})
	d.cfg.InterfaceKey, d.cfg.ParamKey, d.cfg.ParamIV = fixtureLegacyProtocol.InterfaceKey, fixtureLegacyProtocol.ParamKey, fixtureLegacyProtocol.ParamIV
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		if _, err := d.legacyCredentials(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := d.legacyCredentials(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a waiting caller cannot be canceled", err)
	}
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := d.legacyCredentials(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	close(release)
	group.Wait()
	if logins.Load() != 1 {
		t.Fatal("concurrent callers created multiple anonymous logins")
	}
}

func TestLegacyExpiredAnonymousSessionRefreshesOnce(t *testing.T) {
	var logins atomic.Int32
	d := legacyFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/app/mine/login/h5" {
			logins.Add(1)
			return legacyFixtureResponse(request, map[string]string{"token": "renewed-session"}), nil
		}
		if request.Header.Get("Authorization") == "expired-session" {
			return sourceFixtureResponse(request, 200, `{"code":5005,"msg":"no token"}`), nil
		}
		return legacyFixtureResponse(request, []Tab{{ID: "valid", Name: "当前分类"}}), nil
	})
	state := legacySessionState{Version: 1, Scope: d.legacySessionScope(), DeviceID: "0123456789ABCDEF1789290000000", Token: "expired-session", Protocol: fixtureLegacyProtocol, UpdatedAt: time.Now()}
	if err := d.saveLegacySession(state); err != nil {
		t.Fatal(err)
	}
	var tabs legacyTabList
	if err := d.fetchAPI(context.Background(), "/api/app/playlet-tab/all", nil, &tabs); err != nil || len(tabs) != 1 {
		t.Fatal("anonymous recovery failed", err)
	}
	if logins.Load() != 1 {
		t.Fatal("session refresh did not stay bounded")
	}
}

func TestLegacyExplicitTokenIsPreservedWithoutMediaKey(t *testing.T) {
	var calls atomic.Int32
	d := legacyFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.Path != "/api/app/playlet-tab/all" || request.Header.Get("Authorization") != "configured-session" {
			t.Error("explicit configuration was replaced or sent to another endpoint")
		}
		return sourceFixtureResponse(request, 200, `{"code":5005,"msg":"expired configured session"}`), nil
	})
	d.cfg.Token = "configured-session"
	d.cfg.InterfaceKey, d.cfg.ParamKey, d.cfg.ParamIV = fixtureLegacyProtocol.InterfaceKey, fixtureLegacyProtocol.ParamKey, fixtureLegacyProtocol.ParamIV
	var tabs legacyTabList
	if err := d.fetchAPI(context.Background(), "/api/app/playlet-tab/all", nil, &tabs); err == nil {
		t.Fatal("expired explicit token was ignored")
	}
	if calls.Load() != 1 || d.cfg.Token != "configured-session" {
		t.Fatal("explicit token was silently renewed")
	}
}

func TestLegacyPublicProtocolValidation(t *testing.T) {
	for _, script := range []string{"", `t(F,"parameterKey","short")`, `t(F,"interfaceKey","fixture");t(F,"parameterKey","short");t(F,"parameterIv","fedcba9876543210")`} {
		if _, err := parseLegacyProtocol(script); err == nil {
			t.Fatal("accepted incomplete or invalid public protocol")
		}
	}
	var tabs legacyTabList
	for _, raw := range []string{`[{"id":"a","name":"甲"}]`, `{"list":[{"id":"a","name":"甲"}],"count":1}`} {
		if json.Unmarshal([]byte(raw), &tabs) != nil || len(tabs) != 1 || tabs[0].ID != "a" {
			t.Fatal("tab response format not supported")
		}
	}
}

// 站点的 count 恒为 9999、hasNext 恒为 false，两个字段都是假的，只能靠「翻到空页」判断到底。
// 这个用例验证：分类真的会一路翻到空页、跨页不重复、推荐位并得进来、到底后 hasMore 变 false。
func TestLegacyCatalogCrawlsEveryPageAndMergesRecommend(t *testing.T) {
	tabPages := map[string][][]Drama{
		"tab-a": {{{ID: "a1", Title: "甲一"}, {ID: "a2", Title: "甲二"}}, {{ID: "a3", Title: "甲三"}}, {}},
		"tab-b": {{{ID: "b1", Title: "乙一"}, {ID: "b2", Title: "乙二"}}, {}},
	}
	recommendPages := [][]Drama{{{ID: "r1", Title: "荐一"}}, {}}
	transport := sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		path := request.URL.Path
		switch {
		case path == "/api/app/playlet-tab/all":
			return legacyFixtureResponse(request, map[string]any{"list": []Tab{
				{ID: "tab-a", Name: "甲类"}, {ID: "tab-b", Name: "乙类"},
			}}), nil
		case strings.HasPrefix(path, "/api/app/playlet/home/tab/"):
			page, _ := strconv.Atoi(decodeLegacyParam(t, request)["pageNumber"])
			pages := tabPages[strings.TrimPrefix(path, "/api/app/playlet/home/tab/")]
			if page < 1 || page > len(pages) {
				return legacyFixtureResponse(request, map[string]any{"list": []Drama{}}), nil
			}
			return legacyFixtureResponse(request, map[string]any{"list": pages[page-1]}), nil
		case path == "/api/app/playlet-recommend/home/choose":
			page, _ := strconv.Atoi(decodeLegacyParam(t, request)["pageNumber"])
			if page < 1 || page > len(recommendPages) {
				return legacyFixtureResponse(request, map[string]any{"list": []Drama{}}), nil
			}
			return legacyFixtureResponse(request, map[string]any{"list": recommendPages[page-1]}), nil
		default:
			t.Errorf("unexpected request blocked: %s", path)
			return nil, errors.New("unexpected endpoint")
		}
	})
	d := legacyFixtureDownloader(t, transport)
	d.cfg.Token = "own-fixture-session"
	d.cfg.InterfaceKey, d.cfg.ParamKey, d.cfg.ParamIV = fixtureLegacyProtocol.InterfaceKey, fixtureLegacyProtocol.ParamKey, fixtureLegacyProtocol.ParamIV
	d.cfg.PageSize = 2

	var got []string
	var more bool
	for page := 1; page <= 8; page++ {
		items, next, err := d.fetchLegacyCatalogCategoryPage(context.Background(), page, "")
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, drama := range items {
			got = append(got, drama.SourceID)
		}
		more = next
		if !next {
			break
		}
	}
	if more {
		t.Fatal("catalog never reached the end")
	}
	seen := map[string]bool{}
	for _, id := range got {
		if id == "" || seen[id] {
			t.Fatalf("duplicated or empty id in catalog: %q (%v)", id, got)
		}
		seen[id] = true
	}
	for _, want := range []string{"a1", "a2", "a3", "b1", "b2", "r1"} {
		if !seen[want] {
			t.Fatalf("catalog lost %s: %v", want, got)
		}
	}
	if len(got) != 6 {
		t.Fatalf("catalog size=%d, want 6: %v", len(got), got)
	}
}

// 站点在请求过密时返回 4010，必须退避重试，不能把这一页当成空页——否则整屏剧会被判成「到底」。
func TestLegacyCatalogRetriesRateLimitedPage(t *testing.T) {
	var calls atomic.Int32
	transport := sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/app/playlet-tab/all":
			return legacyFixtureResponse(request, map[string]any{"list": []Tab{{ID: "tab-a", Name: "甲类"}}}), nil
		case "/api/app/playlet/home/tab/tab-a":
			if calls.Add(1) == 1 {
				return sourceFixtureResponse(request, 200, `{"code":4010,"msg":"too many requests"}`), nil
			}
			return legacyFixtureResponse(request, map[string]any{"list": []Drama{{ID: "a1", Title: "甲一"}}}), nil
		case "/api/app/playlet-recommend/home/choose":
			return legacyFixtureResponse(request, map[string]any{"list": []Drama{}}), nil
		default:
			return nil, errors.New("unexpected endpoint")
		}
	})
	d := legacyFixtureDownloader(t, transport)
	d.cfg.Token = "own-fixture-session"
	d.cfg.InterfaceKey, d.cfg.ParamKey, d.cfg.ParamIV = fixtureLegacyProtocol.InterfaceKey, fixtureLegacyProtocol.ParamKey, fixtureLegacyProtocol.ParamIV
	d.cfg.PageSize = 5
	items, _, err := d.fetchLegacyCatalogCategoryPage(context.Background(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SourceID != "a1" || calls.Load() < 2 {
		t.Fatalf("rate limited page was not retried: count=%d calls=%d", len(items), calls.Load())
	}
}

// 「推荐」是一条独立剧目流，只打 playlet-recommend/home/choose。
func TestLegacyRecommendCategoryUsesRecommendFeed(t *testing.T) {
	var seen []string
	transport := sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		seen = append(seen, request.URL.Path)
		switch request.URL.Path {
		case "/api/app/playlet-tab/all":
			return legacyFixtureResponse(request, map[string]any{"list": []Tab{{ID: "tab-a", Name: "甲类"}}}), nil
		case "/api/app/playlet-recommend/home/choose":
			page, _ := strconv.Atoi(decodeLegacyParam(t, request)["pageNumber"])
			if page > 1 {
				return legacyFixtureResponse(request, map[string]any{"list": []Drama{}}), nil
			}
			return legacyFixtureResponse(request, map[string]any{"list": []Drama{{ID: "r1", Title: "荐一"}}}), nil
		default:
			t.Errorf("recommend category must not touch %s", request.URL.Path)
			return nil, errors.New("unexpected endpoint")
		}
	})
	d := legacyFixtureDownloader(t, transport)
	d.cfg.Token = "own-fixture-session"
	d.cfg.InterfaceKey, d.cfg.ParamKey, d.cfg.ParamIV = fixtureLegacyProtocol.InterfaceKey, fixtureLegacyProtocol.ParamKey, fixtureLegacyProtocol.ParamIV
	d.cfg.PageSize = 5
	items, _, err := d.fetchLegacyCatalogCategoryPage(context.Background(), 1, legacyRecommendID)
	if err != nil || len(items) != 1 || items[0].ChannelName != legacyRecommendName {
		t.Fatalf("recommend catalog: count=%d error=%v paths=%v", len(items), err, seen)
	}
	for _, path := range seen {
		if strings.Contains(path, "/playlet/home/tab/") {
			t.Fatalf("recommend category hit a tab feed: %v", seen)
		}
	}
}

// 旧版搜索必须打站点的关键词接口，而不是只过滤本地剧库。
func TestLegacyKeywordSearchUsesSiteSearchEndpoint(t *testing.T) {
	var keyword, pageNumber string
	transport := sourceFixtureTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/app/playlet/keyword/search" {
			t.Errorf("unexpected request: %s", request.URL.Path)
			return nil, errors.New("unexpected endpoint")
		}
		params := decodeLegacyParam(t, request)
		keyword, pageNumber = params["keyword"], params["pageNumber"]
		return legacyFixtureResponse(request, map[string]any{"list": []Drama{
			{ID: "search-1", Title: "搜索结果一", TotalEpisode: 3},
			{ID: "search-1", Title: "重复条目"},
			{ID: "", Title: "坏条目"},
		}}), nil
	})
	d := legacyFixtureDownloader(t, transport)
	d.cfg.Token = "own-fixture-session"
	d.cfg.InterfaceKey, d.cfg.ParamKey, d.cfg.ParamIV = fixtureLegacyProtocol.InterfaceKey, fixtureLegacyProtocol.ParamKey, fixtureLegacyProtocol.ParamIV
	items, more, err := d.searchLegacyCatalog(context.Background(), 1, "  样本名  ")
	if err != nil || len(items) != 1 || !more {
		t.Fatalf("search: count=%d more=%v err=%v", len(items), more, err)
	}
	if keyword != "样本名" || pageNumber != "1" {
		t.Fatalf("params: keyword=%q pageNumber=%q", keyword, pageNumber)
	}
	if items[0].Source != sourceCloudFront || items[0].SourceID != "search-1" {
		t.Fatalf("item identity: %+v", items[0])
	}
}

func decodeLegacyParam(t *testing.T, request *http.Request) map[string]string {
	t.Helper()
	raw := request.URL.Query().Get("data")
	encrypted, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(encrypted) == 0 || len(encrypted)%16 != 0 {
		t.Fatalf("invalid encrypted params: %v", err)
	}
	block, err := aes.NewCipher([]byte(fixtureLegacyProtocol.ParamKey))
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCDecrypter(block, []byte(fixtureLegacyProtocol.ParamIV)).CryptBlocks(encrypted, encrypted)
	plain, err := pkcs7Unpad(encrypted, 16)
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]string
	if err := json.Unmarshal(plain, &params); err != nil {
		t.Fatal(err)
	}
	return params
}
