package core

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// 上游中途掐断分片时，本机流服务回 502 会让播放器的 HLS 解复用器判定
// 「这一集播完了」，上层看到的就是「播到一半自动跳下一集」。这里锁住
// 修复后的行为：先原地重试一次上游，能读全就照常整段返回。
func TestStreamRetriesTruncatedSegment(t *testing.T) {
	var hits int32
	body := strings.Repeat("abcdefghij", 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "video/mp4")
		if hit == 1 {
			// 长度多报 100 字节再掐断：客户端读满声明长度时会拿到 EOF。
			w.Header().Set("Content-Length", strconv.Itoa(len(body)+100))
			_, _ = io.WriteString(w, body)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
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
	address := stream.nativeAsset(token, session, nativeStreamAsset{kind: "segment", address: server.URL + "/seg00002.m4s"})

	response, readErr := http.Get(address)
	if readErr != nil {
		t.Fatal(readErr)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(payload) != body {
		t.Fatalf("重试后应整段返回，实际 %d / %d 字节", response.StatusCode, len(payload))
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("上游命中 %d 次，应当先失败一次再重试一次", got)
	}
}

// 两次都读不全时不能再回 502：把已经拿到的部分按精确长度回 200，
// 让播放器自己重试这一片，而不是把整集判成结束。
func TestStreamReturnsPartialSegmentInsteadOf502(t *testing.T) {
	var hits int32
	body := strings.Repeat("abcdefghij", 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+100))
		_, _ = io.WriteString(w, body[:len(body)/2])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
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
	address := stream.nativeAsset(token, session, nativeStreamAsset{kind: "segment", address: server.URL + "/seg00003.m4s"})

	response, readErr := http.Get(address)
	if readErr != nil {
		t.Fatal(readErr)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("应当回 200 + 已读部分，实际 %d", response.StatusCode)
	}
	if len(payload) == 0 || len(payload) > len(body) {
		t.Fatalf("已读部分长度异常：%d", len(payload))
	}
	if value := response.Header.Get("Content-Length"); value != fmt.Sprintf("%d", len(payload)) {
		t.Fatalf("Content-Length = %q，应等于实际长度 %d", value, len(payload))
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("上游命中 %d 次，应当重试过一次", got)
	}
}
