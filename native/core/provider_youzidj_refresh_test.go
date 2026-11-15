package core

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 拖动进度到未缓冲位置时才会去取新分片，那时凭证可能已过期。
// 本机流服务应续领一次凭证并原样重试，而不是把 403 交给播放器。
func TestStreamRefreshesExpiredCredentials(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if strings.Contains(r.Header.Get("Cookie"), "eden_play_v=new") {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = io.WriteString(w, "fresh-segment")
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	d := youzidjTestDownloader(server.URL, server.Client())
	stream, err := newNativeStreamServer(d)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()

	var refreshes int32
	media := providerMedia{
		URL:     server.URL + "/seg.m4s",
		Referer: server.URL + "/dramas/demo/",
		credentials: &providerMediaCredentials{
			origin: server.URL, cookie: "eden_play_v=old", referer: server.URL + "/dramas/demo/",
			userAgent: youzidjUserAgent, expires: time.Now().Add(time.Hour),
		},
		refresh: func(context.Context) (*providerMediaCredentials, error) {
			atomic.AddInt32(&refreshes, 1)
			return &providerMediaCredentials{
				origin: server.URL, cookie: "eden_play_v=new", referer: server.URL + "/dramas/demo/",
				userAgent: youzidjUserAgent, expires: time.Now().Add(time.Hour),
			}, nil
		},
	}
	address, token := stream.nativeOpen(media)
	defer stream.nativeRelease(token)

	response, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "fresh-segment" {
		t.Fatalf("stream = %d %q, want 200 fresh-segment", response.StatusCode, string(body))
	}
	if atomic.LoadInt32(&refreshes) != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshes)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits)
	}

	// 第二次请求仍失败时不应无限续领：5 秒冷却窗口内不再尝试。
	second, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second stream status = %d", second.StatusCode)
	}
	if atomic.LoadInt32(&refreshes) != 1 {
		t.Fatalf("refresh calls after second request = %d, want 1", refreshes)
	}
}

// 没有续领入口的站源，403 仍按原样报错，不能被静默吞掉。
func TestStreamWithoutRefreshReportsForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "denied")
	}))
	defer server.Close()

	d := youzidjTestDownloader(server.URL, server.Client())
	stream, err := newNativeStreamServer(d)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()

	media := providerMedia{
		URL: server.URL + "/seg.m4s", Referer: server.URL + "/",
		credentials: &providerMediaCredentials{origin: server.URL, cookie: "x=1", referer: server.URL + "/", userAgent: youzidjUserAgent},
	}
	address, token := stream.nativeOpen(media)
	defer stream.nativeRelease(token)
	response, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}
}
