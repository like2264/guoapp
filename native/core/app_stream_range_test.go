package core

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 柚子站点对带 Range 的分片请求也回整段 200（实测），播放器拖动时只能整段重下，
// 跨网一慢就会把它当成片尾。这里锁住修复后的行为：整段缓存到会话里、本地按
// Range 切片、重复请求不再走上游。
func TestStreamServesSegmentRangesFromCache(t *testing.T) {
	var hits int32
	body := strings.Repeat("abcdefghij", 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// 故意忽略 Range：站点就是这么回的。
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	d := youzidjTestDownloader(server.URL, server.Client())
	stream, err := newNativeStreamServer(d)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()

	_, token := stream.nativeOpen(providerMedia{
		URL:      server.URL + "/master.m3u8",
		Playlist: "#EXTM3U\n#EXT-X-ENDLIST\n",
	})
	defer stream.nativeRelease(token)
	stream.mu.Lock()
	session := stream.sessions[token]
	stream.mu.Unlock()
	address := stream.nativeAsset(token, session, nativeStreamAsset{kind: "segment", address: server.URL + "/seg00001.m4s"})

	read := func(headers map[string]string) (*http.Response, string) {
		request, err := http.NewRequest(http.MethodGet, address, nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return response, string(payload)
	}

	response, payload := read(nil)
	if response.StatusCode != http.StatusOK || payload != body {
		t.Fatalf("首个整段请求 = %d %d 字节", response.StatusCode, len(payload))
	}

	response, payload = read(map[string]string{"Range": "bytes=10-19"})
	if response.StatusCode != http.StatusPartialContent || payload != "abcdefghij" {
		t.Fatalf("Range 请求 = %d %q，应为本地 206", response.StatusCode, payload)
	}
	if value := response.Header.Get("Content-Range"); value != fmt.Sprintf("bytes 10-19/%d", len(body)) {
		t.Fatalf("Content-Range = %q", value)
	}

	response, payload = read(nil)
	if response.StatusCode != http.StatusOK || len(payload) != len(body) {
		t.Fatalf("缓存命中请求 = %d %d 字节", response.StatusCode, len(payload))
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("上游命中 %d 次，分片应当只取一次", hits)
	}
}
