package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type nativeStreamAsset struct {
	kind        string
	address     string
	data        []byte
	contentType string
	total       int64
	etag        string
	modified    string
	usedAt      time.Time
}

type nativeStreamSession struct {
	credentials  *providerMediaCredentials
	mu           sync.Mutex
	assets       map[string]nativeStreamAsset
	referer      string
	key          []byte
	ctx          context.Context
	cancel       context.CancelFunc
	lastUsed     time.Time
	diagnosticID string
	// refresh 用于播放途中凭证失效时重新领取一次，例如柚子短剧的访客会话与
	// 年龄确认都有有效期：拖到未缓冲位置才会去取新分片，这时凭证若已过期，
	// 站点回 403，播放器就会卡住直到健康检查把它判死。
	refresh     func(context.Context) (*providerMediaCredentials, error)
	refreshMu   sync.Mutex
	refreshing  bool
	refreshedAt time.Time
}

type nativeStreamServer struct {
	mu         sync.Mutex
	downloader *Downloader
	address    string
	sessions   map[string]*nativeStreamSession
	server     *http.Server
}

// nativeStreamIdleTimeout 是上游「一直没有新数据」的容忍时间。站点偶尔会把
// 分片请求挂住不返回（跨网链路丢包时尤其明显），本地流服务若一直等下去，
// 播放器只会看到进度条卡死，最后被健康检查判死；这里主动掐断让播放器重发。
const nativeStreamIdleTimeout = 30 * time.Second

// 站点对带 Range 的请求也回整段 200（实测该站分片不支持 Range），
// 播放器每次拖动都要整段重下；跨网链路一段就要 0.6~1.3 秒，连续拖几下
// 播放器就会放弃并报 EOF。这里把装得下的分片整段读进来：
// 本地就能按 Range 切片、Content-Length 也是精确值，重复拖动不再走网络。
const (
	nativeSegmentBufferLimit = 16 << 20
	nativeSegmentCacheBytes  = 32 << 20
)

// nativeCacheAsset 把整段分片存进会话缓存，并按内存预算淘汰旧分片。
func (stream *nativeStreamServer) nativeCacheAsset(session *nativeStreamSession, id string, data []byte, contentType string) {
	if len(data) == 0 {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	asset, found := session.assets[id]
	if !found {
		return
	}
	asset.data = data
	asset.total = 0
	asset.usedAt = time.Now()
	if asset.contentType == "" {
		asset.contentType = contentType
	}
	session.assets[id] = asset
	total := int64(0)
	for _, item := range session.assets {
		total += int64(len(item.data))
	}
	if total <= nativeSegmentCacheBytes {
		return
	}
	type cached struct {
		id   string
		used time.Time
	}
	items := make([]cached, 0, len(session.assets))
	for key, item := range session.assets {
		if key == id || len(item.data) == 0 || item.kind == "entry_playlist" || item.kind == "child_playlist" {
			continue
		}
		items = append(items, cached{id: key, used: item.usedAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].used.Before(items[j].used) })
	for _, item := range items {
		if total <= nativeSegmentCacheBytes {
			break
		}
		dropped := session.assets[item.id]
		total -= int64(len(dropped.data))
		dropped.data = nil
		session.assets[item.id] = dropped
	}
}

// nativeIdleBody 在读取上游响应体时不断重置空闲计时器，读取中断就取消请求。
type nativeIdleBody struct {
	io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
}

func (body *nativeIdleBody) Read(chunk []byte) (int, error) {
	read, err := body.ReadCloser.Read(chunk)
	if read > 0 {
		body.timer.Reset(nativeStreamIdleTimeout)
	}
	return read, err
}

func (body *nativeIdleBody) Close() error {
	body.timer.Stop()
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}

func (stream *nativeStreamServer) nativeRequest(request *http.Request) (*http.Response, error) {
	client := *stream.downloader.client
	client.Timeout = 0
	idleContext, releaseIdle := context.WithCancel(request.Context())
	request = request.WithContext(idleContext)
	idle := time.AfterFunc(nativeStreamIdleTimeout, releaseIdle)
	info, _ := request.Context().Value(nativeStreamDiagnosticKey{}).(nativeStreamDiagnostic)
	response, err := stream.downloader.doMediaRequestWithClient(request, &client)
	if err != nil {
		idle.Stop()
		releaseIdle()
		stream.nativeLogError(info, "upstream_error", request.URL.String(), err, "")
		return response, err
	}
	address := request.URL.String()
	if response.Request != nil && response.Request.URL != nil {
		address = response.Request.URL.String()
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		stream.nativeLog(info, "upstream_status", address, "", response.StatusCode, response.Header.Get("Content-Type"))
	}
	body := &nativeDiagnosticBody{ReadCloser: response.Body, stream: stream, info: info, address: address}
	response.Body = &nativeIdleBody{ReadCloser: body, timer: idle, cancel: releaseIdle}
	return response, nil
}

// nativeReplay 用给定凭证重发一次原始请求，保留 Range/If-Range。
// 返回的请求对象用于错误信息里带上真实地址。
func (stream *nativeStreamServer) nativeReplay(ctx context.Context, session *nativeStreamSession, original *http.Request, address string, credentials *providerMediaCredentials) (*http.Response, *http.Request) {
	replay, err := http.NewRequestWithContext(providerMediaContext(ctx, credentials), original.Method, address, nil)
	if err != nil {
		return nil, nil
	}
	replay.Header.Set("User-Agent", userAgent)
	replay.Header.Set("Referer", session.referer)
	for _, name := range []string{"Range", "If-Range"} {
		if value := original.Header.Get(name); value != "" {
			replay.Header.Set(name, value)
		}
	}
	response, err := stream.nativeRequest(replay)
	if err != nil {
		return nil, nil
	}
	return response, replay
}

var nativePlaylistURI = regexp.MustCompile(`URI="([^"]+)"`)

func newNativeStreamServer(d *Downloader) (*nativeStreamServer, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("无法初始化本机播放器")
	}
	stream := &nativeStreamServer{downloader: d, address: "http://" + listener.Addr().String(), sessions: map[string]*nativeStreamSession{}}
	server := &http.Server{Handler: http.HandlerFunc(stream.nativeServe), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	stream.server = server
	go func() { _ = server.Serve(listener) }()
	return stream, nil
}

func (stream *nativeStreamServer) nativeOpen(media providerMedia) (string, string) {
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(tokenBytes)
	ctx, cancel := context.WithCancel(providerMediaContext(context.Background(), media.credentials))
	session := &nativeStreamSession{assets: map[string]nativeStreamAsset{}, referer: media.Referer, key: media.HLSKey, ctx: ctx, cancel: cancel, lastUsed: time.Now(), credentials: media.credentials, diagnosticID: nativeStreamID(token), refresh: media.refresh}
	stream.mu.Lock()
	for id, old := range stream.sessions {
		if time.Since(old.lastUsed) > 10*time.Minute {
			stream.nativeCancel(old, "idle_expired")
			delete(stream.sessions, id)
		}
	}
	if len(stream.sessions) >= 16 {
		// 只淘汰确实空闲的会话。之前按「最久未用」无条件淘汰，正在播的那条
		// 有可能被刚开出来的预加载会话挤掉，之后所有分片都回 410，播放器
		// 直接当成 EOF，于是「卡一下 → 自动跳下一集」。
		oldest := ""
		for id, old := range stream.sessions {
			if time.Since(old.lastUsed) < 2*time.Minute {
				continue
			}
			if oldest == "" || old.lastUsed.Before(stream.sessions[oldest].lastUsed) {
				oldest = id
			}
		}
		if oldest != "" {
			stream.nativeCancel(stream.sessions[oldest], "capacity_eviction")
			delete(stream.sessions, oldest)
		}
	}
	stream.sessions[token] = session
	stream.mu.Unlock()
	entry := nativeStreamAsset{kind: "media", address: media.URL, contentType: "video/mp4"}
	parsedURL, parseErr := url.Parse(media.URL)
	isHLS := media.Playlist != "" || (parseErr == nil && strings.HasSuffix(strings.ToLower(parsedURL.Path), ".m3u8"))
	if isHLS {
		entry.contentType = "application/vnd.apple.mpegurl"
	}
	if media.Playlist != "" {
		entry.data = []byte(media.Playlist)
		entry.contentType = "application/vnd.apple.mpegurl"
	}
	if isHLS || strings.Contains(entry.contentType, "mpegurl") {
		entry.kind = "entry_playlist"
	}
	return stream.nativeAsset(token, session, entry), token
}

// credentialSnapshot 取当前生效的播放凭证。
func (session *nativeStreamSession) credentialSnapshot() *providerMediaCredentials {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.credentials
}

// refreshCredential 在播放途中凭证失效时重新领取一次。
// 5 秒内不重复尝试，避免一批分片同时 403 时把站点打爆。
func (session *nativeStreamSession) refreshCredential(ctx context.Context) *providerMediaCredentials {
	if session.refresh == nil {
		return nil
	}
	session.refreshMu.Lock()
	if session.refreshing || time.Since(session.refreshedAt) < 5*time.Second {
		session.refreshMu.Unlock()
		return nil
	}
	session.refreshing = true
	session.refreshMu.Unlock()
	fresh, err := session.refresh(ctx)
	session.refreshMu.Lock()
	session.refreshing = false
	session.refreshedAt = time.Now()
	session.refreshMu.Unlock()
	if err != nil || fresh == nil {
		return nil
	}
	session.mu.Lock()
	session.credentials = fresh
	session.mu.Unlock()
	return fresh
}

func (stream *nativeStreamServer) nativeCancel(session *nativeStreamSession, reason string) {
	if reason != "release" {
		stream.nativeLog(nativeStreamDiagnostic{session: session.diagnosticID}, "session_cancel", "", "reason="+reason, 0, "")
	}
	session.cancel()
}

func (stream *nativeStreamServer) nativeRelease(token string) {
	stream.mu.Lock()
	if session := stream.sessions[token]; session != nil {
		stream.nativeCancel(session, "release")
		delete(stream.sessions, token)
	}
	stream.mu.Unlock()
}

func (stream *nativeStreamServer) nativeAsset(token string, session *nativeStreamSession, asset nativeStreamAsset) string {
	digest := sha256.Sum256([]byte(asset.address + "\x00" + asset.contentType))
	extension := ".ts"
	if parsed, err := url.Parse(asset.address); err == nil {
		switch candidate := strings.ToLower(path.Ext(parsed.Path)); candidate {
		case ".m3u8", ".ts", ".m4s", ".mp4", ".aac", ".m4a", ".mp3", ".vtt", ".webvtt", ".key":
			extension = candidate
		}
	}
	switch asset.contentType {
	case "application/vnd.apple.mpegurl":
		extension = ".m3u8"
	case "application/octet-stream":
		extension = ".key"
	case "video/mp4":
		extension = ".mp4"
	}
	id := hex.EncodeToString(digest[:12]) + extension
	session.mu.Lock()
	if previous, found := session.assets[id]; !found || len(previous.data) == 0 || len(asset.data) > 0 {
		session.assets[id] = asset
	}
	session.mu.Unlock()
	return stream.address + "/" + token + "/" + id
}

func (stream *nativeStreamServer) nativeRewrite(token string, session *nativeStreamSession, body, base string) (string, error) {
	var output []string
	nextPlaylist := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		text := strings.TrimSpace(line)
		rewrite := func(reference string, key bool, contentType string) string {
			if strings.HasPrefix(reference, "data:") {
				return reference
			}
			parsed, err := url.Parse(base)
			if err != nil {
				return ""
			}
			relative, err := url.Parse(reference)
			if err != nil {
				return ""
			}
			address := parsed.ResolveReference(relative).String()
			if !isProviderHTTPMediaURL(address) {
				return ""
			}
			kind := "segment"
			switch contentType {
			case "application/vnd.apple.mpegurl":
				kind = "child_playlist"
			case "application/octet-stream":
				kind = "key"
			case "video/mp4":
				kind = "map"
			}
			asset := nativeStreamAsset{kind: kind, address: address, contentType: contentType}
			if key && len(session.key) == 16 {
				asset.data = append([]byte{}, session.key...)
				asset.contentType = "application/octet-stream"
			}
			return stream.nativeAsset(token, session, asset)
		}
		if text != "" && !strings.HasPrefix(text, "#") {
			contentType := ""
			if nextPlaylist {
				contentType = "application/vnd.apple.mpegurl"
			}
			line = rewrite(text, false, contentType)
			nextPlaylist = false
			if line == "" {
				return "", errors.New("播放列表中的媒体地址无效")
			}
		} else if strings.Contains(text, "URI=") {
			invalid := false
			line = nativePlaylistURI.ReplaceAllStringFunc(line, func(match string) string {
				reference := nativePlaylistURI.FindStringSubmatch(match)[1]
				key := strings.HasPrefix(text, "#EXT-X-KEY:") || strings.HasPrefix(text, "#EXT-X-SESSION-KEY:")
				contentType := ""
				switch {
				case key:
					contentType = "application/octet-stream"
				case strings.HasPrefix(text, "#EXT-X-MAP:"):
					contentType = "video/mp4"
				case strings.HasPrefix(text, "#EXT-X-MEDIA:"), strings.HasPrefix(text, "#EXT-X-I-FRAME-STREAM-INF:"), strings.HasPrefix(text, "#EXT-X-RENDITION-REPORT:"):
					contentType = "application/vnd.apple.mpegurl"
				}
				updated := rewrite(reference, key, contentType)
				if updated == "" {
					invalid = true
				}
				return "URI=\"" + updated + "\""
			})
			if invalid {
				return "", errors.New("播放列表中的附属地址无效")
			}
		}
		if strings.HasPrefix(text, "#EXT-X-STREAM-INF:") {
			nextPlaylist = true
		}
		output = append(output, line)
	}
	return strings.Join(output, "\n"), nil
}

func (stream *nativeStreamServer) nativeServe(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(writer, request)
		return
	}
	stream.mu.Lock()
	session := stream.sessions[parts[0]]
	if session != nil {
		session.lastUsed = time.Now()
	}
	stream.mu.Unlock()
	if session == nil {
		http.Error(writer, "播放已结束", http.StatusGone)
		return
	}
	session.mu.Lock()
	asset, found := session.assets[parts[1]]
	session.mu.Unlock()
	info := nativeStreamDiagnostic{session: session.diagnosticID, asset: asset.kind}
	if !found {
		stream.nativeLog(info, "asset_missing", "", "", 0, "")
		http.NotFound(writer, request)
		return
	}
	// 凭证快到点就提前续一次：等到真正过期时，apply 会在本地直接把请求挡回来，
	// 这时再去续领已经晚了一拍（之前拖动进度卡死就是这条路径）。
	credentials := session.credentialSnapshot()
	if credentials.expiringWithin(60*time.Second, time.Now()) {
		if fresh := session.refreshCredential(request.Context()); fresh != nil {
			credentials = fresh
		}
	}
	ctx, cancel := context.WithCancel(providerMediaContext(request.Context(), credentials))
	defer cancel()
	ctx = context.WithValue(ctx, nativeStreamDiagnosticKey{}, info)
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	writer.Header().Set("Cache-Control", "no-store")
	if len(asset.data) > 0 && asset.total > int64(len(asset.data)) {
		if stream.nativeServePrefix(ctx, writer, request, session, asset) {
			return
		}
		asset.data = nil
	}
	if len(asset.data) > 0 {
		if strings.Contains(asset.contentType, "mpegurl") {
			body, err := stream.nativeRewrite(parts[0], session, string(asset.data), asset.address)
			if err != nil {
				stream.nativeLog(info, "playlist_rewrite", asset.address, "kind="+nativePlaylistKind(string(asset.data))+" error="+nativeStreamError(err), 0, "")
				http.Error(writer, err.Error(), http.StatusBadGateway)
				return
			}
			writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			if request.Method == http.MethodGet {
				_, _ = io.WriteString(writer, body)
			}
		} else {
			writer.Header().Set("Content-Type", asset.contentType)
			if asset.etag != "" {
				writer.Header().Set("ETag", asset.etag)
			}
			http.ServeContent(writer, request, parts[1], time.Time{}, bytes.NewReader(asset.data))
		}
		return
	}
	upstream, err := http.NewRequestWithContext(ctx, request.Method, asset.address, nil)
	if err != nil {
		http.Error(writer, "媒体地址无效", http.StatusBadGateway)
		return
	}
	upstream.Header.Set("User-Agent", userAgent)
	upstream.Header.Set("Referer", session.referer)
	for _, name := range []string{"Range", "If-Range"} {
		if value := request.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	response, err := stream.nativeRequest(upstream)
	// 本地判定凭证过期时请求根本没发出去。这时续领一次再原样重发，
	// 否则拖到未缓冲位置会直接拿到 502，播放器只能黑屏。
	if err != nil && errors.Is(err, errProviderMediaExpired) {
		if fresh := session.refreshCredential(ctx); fresh != nil {
			if retried, retriedRequest := stream.nativeReplay(ctx, session, request, asset.address, fresh); retried != nil {
				response, upstream, err = retried, retriedRequest, nil
				stream.nativeLog(info, "credential_refreshed", asset.address, "trigger=expired", retried.StatusCode, "")
			}
		}
	}
	if err != nil {
		http.Error(writer, "读取媒体失败，请重试", http.StatusBadGateway)
		return
	}
	// 播放凭证过期（或站点换了签名）时，拖动进度到未缓冲位置才会暴露：
	// 这时才去取新分片，站点回 403，播放器会一直等，最后被健康检查判死。
	// 这里续领一次凭证并原样重试，成功就当没发生过。
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if fresh := session.refreshCredential(ctx); fresh != nil {
			if retried, retriedRequest := stream.nativeReplay(ctx, session, request, asset.address, fresh); retried != nil {
				response, upstream = retried, retriedRequest
				stream.nativeLog(info, "credential_refreshed", asset.address, "", response.StatusCode, "")
			}
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
			err := stream.downloader.catalogResponseError(upstream, response, body)
			http.Error(writer, err.Error(), response.StatusCode)
			return
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		err := stream.downloader.catalogResponseError(upstream, response, body)
		http.Error(writer, err.Error(), response.StatusCode)
		return
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	finalURL := upstream.URL
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL
	}
	contentIsMP4 := strings.Contains(contentType, "video/mp4")
	playlist := strings.Contains(contentType, "mpegurl") ||
		(strings.HasSuffix(strings.ToLower(finalURL.Path), ".m3u8") && !contentIsMP4)
	reader := bufio.NewReader(response.Body)
	if request.Method == http.MethodGet {
		peek, _ := reader.Peek(512)
		bodyIsPlaylist := strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(string(peek), "\ufeff")), "#EXTM3U")
		if bodyIsPlaylist {
			playlist = true
		} else if contentIsMP4 {
			playlist = false
		}
	}
	if playlist && request.Method == http.MethodHead {
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		writer.WriteHeader(http.StatusOK)
		return
	}
	if playlist && request.Method == http.MethodGet {
		body, err := io.ReadAll(io.LimitReader(reader, 4<<20+1))
		if err != nil || len(body) > 4<<20 {
			stream.nativeLogError(info, "playlist_read_failed", finalURL.String(), err, contentType)
			http.Error(writer, "播放列表过大或读取失败", http.StatusBadGateway)
			return
		}
		text := strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff"))
		if !strings.HasPrefix(text, "#EXTM3U") {
			stream.nativeLog(info, "playlist_invalid", finalURL.String(), "", 0, contentType)
			http.Error(writer, "播放列表无效", http.StatusBadGateway)
			return
		}
		rewritten, err := stream.nativeRewrite(parts[0], session, text, finalURL.String())
		if err != nil {
			stream.nativeLog(info, "playlist_rewrite", finalURL.String(), "kind="+nativePlaylistKind(text)+" error="+nativeStreamError(err), 0, contentType)
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(writer, rewritten)
		return
	}
	// 分片整段读进内存再本地切 Range：站点不认 Range，只有整段 200；
	// 播放器拖动时若拿到「整段 + 无精确长度」，跨网一慢就会放弃并报 EOF，
	// 上层只能看到「播完了」，于是自动跳下一集。这里补齐 Content-Length
	// 与 Range 支持，并把分片留在会话缓存里供重复拖动复用。
	// 上游已经自己按 Range 回了 206 的，保持原样透传，别二次切片。
	if request.Method != http.MethodHead &&
		response.StatusCode == http.StatusOK &&
		(asset.kind == "segment" || asset.kind == "map") &&
		response.ContentLength > 0 &&
		response.ContentLength <= nativeSegmentBufferLimit {
		buffered, readErr := io.ReadAll(io.LimitReader(reader, nativeSegmentBufferLimit+1))
		if readErr == nil && int64(len(buffered)) == response.ContentLength {
			stream.nativeCacheAsset(session, parts[1], buffered, response.Header.Get("Content-Type"))
			writer.Header().Set("Content-Type", response.Header.Get("Content-Type"))
			http.ServeContent(writer, request, parts[1], time.Time{}, bytes.NewReader(buffered))
			return
		}
		stream.nativeLogError(info, "segment_buffer_failed", finalURL.String(), readErr, "")
		http.Error(writer, "读取媒体失败，请重试", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := response.Header.Get(name); value != "" {
			writer.Header().Set(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	if request.Method == http.MethodGet {
		_, _ = io.Copy(writer, reader)
	}
}
