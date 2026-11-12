package core

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const huangguoAISearchPageSize = 20

// 黄果站点 2026-11 改版后，所有取流接口都换成了 edge 加密通道：
//
//	POST /api/playback/request  {v:1,type:"hello",client_pub:<P-256 公钥>}
//	→ {v:1,type:"hello",kid,sid,site,server_pub,salt,server_time,expires_at}
//
// 之后每个请求都是 ECDH(P-256) → HKDF-SHA256 → AES-256-GCM 的密文信封：
//
//	POST /api/playback/request  {v:1,type:"call",kid,sid,seq,ts,ciphertext}
//	→ {v:1,type:"result",sid,kid,seq,ts,ciphertext}
//
// 明文体是 {"request":{method,path,...}}，响应体是 {status,headers,body(base64)}。
// 取某一集播放地址走 POST /api/videos/{id}/episodes/{ep}/playback（无请求体）。
// 协议名固定在 HKDF info 里，改动站点就会失效，届时需要重新对照站点脚本。
const (
	huangguoAIEdgeProtocol   = "dx150-edge-v1"
	huangguoAIEdgeScope      = "playback"
	huangguoAIEdgePath       = "/api/playback/request"
	huangguoAIEdgeMaxTries   = 4
	huangguoAIEdgeMaxPayload = 32 << 20
)

type huangguoAIEdgeSession struct {
	info   string
	kid    string
	sid    string
	offset int64
	seq    uint32
	req    cipher.AEAD
	res    cipher.AEAD
}

func huangguoAIBase64URL(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

func huangguoAIBase64URLDecode(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
}

// huangguoAIEdgePost 固定发往指定主机（不参与站源域名轮换），
// 否则握手与取流可能落在不同镜像上，sid 对不上会直接 400。
func (d *Downloader) huangguoAIEdgePost(ctx context.Context, address, referer string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Referer", referer)
	resp, err := d.doCatalogRequestWithTimeout(req, providerTimeout)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, huangguoAIEdgeMaxPayload))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("黄果加密通道返回 HTTP %d：%s", resp.StatusCode, truncate(string(body), 160))
	}
	return body, nil
}

func (d *Downloader) huangguoAIEdgeHandshake(ctx context.Context, base string) (*huangguoAIEdgeSession, error) {
	curve := ecdh.P256()
	private, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"v": 1, "type": "hello", "client_pub": huangguoAIBase64URL(private.PublicKey().Bytes()),
	})
	if err != nil {
		return nil, err
	}
	raw, err := d.huangguoAIEdgePost(ctx, base+huangguoAIEdgePath, base+"/", payload)
	if err != nil {
		return nil, err
	}
	var hello struct {
		Type       string `json:"type"`
		Kid        string `json:"kid"`
		Sid        string `json:"sid"`
		Site       string `json:"site"`
		ServerPub  string `json:"server_pub"`
		Salt       string `json:"salt"`
		ServerTime int64  `json:"server_time"`
	}
	if err := json.Unmarshal(raw, &hello); err != nil {
		return nil, fmt.Errorf("黄果加密通道握手响应无效：%w", err)
	}
	if hello.Type != "hello" || hello.Kid == "" || hello.Sid == "" || hello.ServerPub == "" {
		return nil, errors.New("黄果加密通道握手被拒绝")
	}
	serverRaw, err := huangguoAIBase64URLDecode(hello.ServerPub)
	if err != nil {
		return nil, err
	}
	serverKey, err := curve.NewPublicKey(serverRaw)
	if err != nil {
		return nil, err
	}
	shared, err := private.ECDH(serverKey)
	if err != nil {
		return nil, err
	}
	salt, err := huangguoAIBase64URLDecode(hello.Salt)
	if err != nil {
		return nil, err
	}
	info := strings.Join([]string{huangguoAIEdgeProtocol, huangguoAIEdgeScope,
		hello.Site, hello.Kid, hello.Sid}, "\n")
	requestKey, err := hkdf.Key(sha256.New, shared, salt, info+"\nreq", 32)
	if err != nil {
		return nil, err
	}
	responseKey, err := hkdf.Key(sha256.New, shared, salt, info+"\nres", 32)
	if err != nil {
		return nil, err
	}
	requestBlock, err := aes.NewCipher(requestKey)
	if err != nil {
		return nil, err
	}
	responseBlock, err := aes.NewCipher(responseKey)
	if err != nil {
		return nil, err
	}
	requestAEAD, err := cipher.NewGCM(requestBlock)
	if err != nil {
		return nil, err
	}
	responseAEAD, err := cipher.NewGCM(responseBlock)
	if err != nil {
		return nil, err
	}
	serverTime := hello.ServerTime
	if serverTime == 0 {
		serverTime = time.Now().Unix()
	}
	return &huangguoAIEdgeSession{
		info: info, kid: hello.Kid, sid: hello.Sid,
		offset: serverTime - time.Now().Unix(),
		req:    requestAEAD, res: responseAEAD,
	}, nil
}

// huangguoAIEdgeCall 走一次加密请求，返回解开后的业务响应体。
func (d *Downloader) huangguoAIEdgeCall(ctx context.Context, base string, session *huangguoAIEdgeSession, request map[string]any) ([]byte, error) {
	session.seq++
	sequence := session.seq
	iv := make([]byte, 12)
	binary.BigEndian.PutUint32(iv[8:], sequence)
	timestamp := time.Now().Unix() + session.offset
	aad := session.info + "\nreq\n" + strconv.FormatUint(uint64(sequence), 10) + "\n" + strconv.FormatInt(timestamp, 10)
	plain, err := json.Marshal(map[string]any{"request": request})
	if err != nil {
		return nil, err
	}
	sealed := session.req.Seal(nil, iv, plain, []byte(aad))
	payload, err := json.Marshal(map[string]any{
		"v": 1, "type": "call", "kid": session.kid, "sid": session.sid,
		"seq": sequence, "ts": timestamp, "ciphertext": huangguoAIBase64URL(sealed),
	})
	if err != nil {
		return nil, err
	}
	raw, err := d.huangguoAIEdgePost(ctx, base+huangguoAIEdgePath, base+"/", payload)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Type       string `json:"type"`
		Kid        string `json:"kid"`
		Sid        string `json:"sid"`
		Seq        uint32 `json:"seq"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("黄果加密通道响应无效：%w", err)
	}
	if reply.Type != "result" || reply.Seq != sequence || reply.Sid != session.sid {
		return nil, errors.New("黄果加密通道响应不匹配")
	}
	ciphertext, err := huangguoAIBase64URLDecode(reply.Ciphertext)
	if err != nil {
		return nil, err
	}
	responseAAD := session.info + "\nres\n" + strconv.FormatUint(uint64(sequence), 10) + "\n" + strconv.FormatInt(timestamp, 10)
	inner, err := session.res.Open(nil, iv, ciphertext, []byte(responseAAD))
	if err != nil {
		return nil, fmt.Errorf("黄果加密通道解密失败：%w", err)
	}
	var envelope struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(inner, &envelope); err != nil {
		return nil, fmt.Errorf("黄果加密通道响应格式已变化：%w", err)
	}
	body, err := base64.StdEncoding.DecodeString(envelope.Body)
	if err != nil {
		return nil, fmt.Errorf("黄果响应体解码失败：%w", err)
	}
	if envelope.Status != http.StatusOK {
		return nil, fmt.Errorf("黄果取流被拒绝（HTTP %d）：%s", envelope.Status, truncate(string(body), 160))
	}
	return body, nil
}

// huangguoAIEdgeBases 依次给出可尝试的站点主机：先用当前生效域名，再退回域名轮换表。
func (d *Downloader) huangguoAIEdgeBases() []string {
	primary := strings.TrimRight(d.providerBaseURL(sourceHuangguoAI), "/")
	bases := []string{primary}
	seen := map[string]bool{primary: true}
	for _, candidate := range d.providerURLCandidates(primary + "/api/site/config") {
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		origin := parsed.Scheme + "://" + parsed.Host
		if seen[origin] {
			continue
		}
		seen[origin] = true
		bases = append(bases, origin)
		if len(bases) >= huangguoAIEdgeMaxTries {
			break
		}
	}
	return bases
}

func (d *Downloader) fetchHuangguoAIPlayURL(ctx context.Context, sourceID string, episode int) (string, error) {
	if !rankingSourceID.MatchString(sourceID) || episode < 1 {
		return "", errors.New("黄果集数无效")
	}
	escaped := url.PathEscape(sourceID)
	request := map[string]any{
		"method": http.MethodPost,
		"path":   "/api/videos/" + escaped + "/episodes/" + strconv.Itoa(episode) + "/playback",
	}
	var lastErr error
	for _, base := range d.huangguoAIEdgeBases() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		session, err := d.huangguoAIEdgeHandshake(ctx, base)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := d.huangguoAIEdgeCall(ctx, base, session, request)
		if err != nil {
			lastErr = err
			continue
		}
		var reply map[string]any
		if err := json.Unmarshal(body, &reply); err != nil {
			lastErr = fmt.Errorf("黄果取流响应无效：%w", err)
			continue
		}
		if media := huangguoAIPlaybackMedia(base, huangguoAIPlaybackURL(reply)); media != "" {
			return media, nil
		}
		lastErr = errors.New("黄果未返回播放地址")
	}
	if lastErr == nil {
		lastErr = errors.New("黄果取流通道不可用")
	}
	return "", fmt.Errorf("黄果站点取流失败：%w", lastErr)
}

func (d *Downloader) huangguoAIAPI(ctx context.Context, path string, out any) error {
	return d.huangguoAIRequest(ctx, http.MethodGet, path, "", "", out)
}

// huangguoAIRequest 通过站点自带的 edge 通道转发请求；method 与 body 都写在信封里。
func (d *Downloader) huangguoAIRequest(ctx context.Context, method, path, body, contentType string, out any) error {
	envelope, err := json.Marshal(map[string]string{
		"method":       method,
		"path":         path,
		"body":         body,
		"content_type": contentType,
	})
	if err != nil {
		return err
	}
	base := d.providerBaseURL(sourceHuangguoAI)
	raw, err := d.fetchProviderRequest(ctx, base+"/api/edge/request", base+"/", envelope)
	if err != nil {
		return err
	}
	var reply struct {
		Status int             `json:"status"`
		Msg    string          `json:"msg"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		return fmt.Errorf("黄果接口返回格式无效：%w", err)
	}
	if reply.Status != 1 {
		return fmt.Errorf("黄果接口返回失败：%s", firstNonEmpty(reply.Msg, "未知错误"))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(reply.Data, out); err != nil {
		return fmt.Errorf("黄果接口数据格式已变化：%w", err)
	}
	return nil
}

func (d *Downloader) fetchHuangguoAIDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	sourceID = strings.TrimPrefix(strings.Trim(strings.TrimSpace(sourceID), "/"), "detail/")
	if !rankingSourceID.MatchString(sourceID) {
		return Drama{}, nil, errors.New("无效的黄果剧集 ID")
	}
	var detail map[string]any
	if err := d.huangguoAIAPI(ctx, "/api/videos/"+url.PathEscape(sourceID), &detail); err != nil {
		return Drama{}, nil, err
	}
	base := d.providerBaseURL(sourceHuangguoAI)
	drama, ok := dramaFromAIMap(detail, base+"/", "")
	if !ok || drama.SourceID != sourceID {
		return Drama{}, nil, errors.New("黄果详情与请求剧集不符")
	}
	rows, _ := detail["episodes"].([]any)
	var chapters []Chapter
	for _, value := range rows {
		row, _ := value.(map[string]any)
		episode, err := strconv.Atoi(mapString(row, "ep_num"))
		if err != nil || episode < 1 {
			continue
		}
		chapters = append(chapters, huangguoAIChapter(base, sourceID, episode, mapString(row, "title")))
	}
	if len(rows) == 0 {
		chapters = append(chapters, huangguoAIChapter(base, sourceID, 1, drama.DisplayTitle()))
	}
	sortProviderChapters(chapters)
	return drama, uniqueChapters(chapters), nil
}

func huangguoAIChapter(base, sourceID string, episode int, title string) Chapter {
	key, pageURL := "auto-1", base+"/video/"+sourceID+"/"
	if episode > 1 {
		key = "ep-" + strconv.Itoa(episode)
		pageURL += key + "/"
	}
	return Chapter{ID: providerChapterID(sourceHuangguoAI, sourceID, key), Source: sourceHuangguoAI,
		Title: firstNonEmpty(title, fmt.Sprintf("第%d集", episode)), PageURL: pageURL, CurrentEpisode: rawEpisode(episode)}
}

// huangguoAIPlaybackMedia 把接口给出的原始地址归一化成可播放地址；
// 新版接口可能返回不带扩展名的地址，因此允许放宽到任意 http(s) 媒体地址。
func huangguoAIPlaybackMedia(base, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if media := normalizeProviderMediaURL(base+"/", raw); media != "" {
		return media
	}
	if resolved := resolveProviderURL(base+"/", raw); isProviderHTTPMediaURL(resolved) {
		return resolved
	}
	return ""
}

// huangguoAIPlaybackURL 在返回体里递归找出播放地址，先认 .m3u8/.mp4，再放宽到已知字段名。
func huangguoAIPlaybackURL(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	return huangguoAIPlaybackFromMap(payload, 0)
}

func huangguoAIPlaybackFromMap(row map[string]any, depth int) string {
	if depth > 3 {
		return ""
	}
	for _, key := range []string{"main", "hd", "sd"} {
		if streams, ok := row["streams"].(map[string]any); ok {
			if value := strings.TrimSpace(mapString(streams, key)); isProviderHTTPMediaURL(value) {
				return value
			}
		}
	}
	for _, key := range []string{"video_url", "play_url", "playback_url", "video_api", "m3u8", "url", "src"} {
		value := strings.TrimSpace(mapString(row, key))
		lower := strings.ToLower(value)
		if strings.Contains(lower, ".m3u8") || strings.Contains(lower, ".mp4") {
			return value
		}
	}
	for _, key := range []string{"video_url", "play_url", "playback_url", "video_api", "url", "src"} {
		if value := strings.TrimSpace(mapString(row, key)); isProviderHTTPMediaURL(value) {
			return value
		}
	}
	for _, value := range row {
		if child, ok := value.(map[string]any); ok {
			if media := huangguoAIPlaybackFromMap(child, depth+1); media != "" {
				return media
			}
		}
	}
	return ""
}

func (d *Downloader) searchHuangguoAI(ctx context.Context, page int, query string) ([]Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 256 {
		return nil, false, errors.New("黄果搜索关键词无效")
	}
	var result struct {
		Items      json.RawMessage `json:"items"`
		Pagination struct {
			Pages int `json:"pages"`
		} `json:"pagination"`
	}
	path := "/api/search?" + url.Values{"q": {query}, "page": {strconv.Itoa(page)}, "size": {strconv.Itoa(huangguoAISearchPageSize)}}.Encode()
	if err := d.huangguoAIAPI(ctx, path, &result); err != nil {
		return nil, false, err
	}
	items := parseHuangguoAIJSONCards(result.Items, d.providerBaseURL(sourceHuangguoAI)+"/", "")
	return items, page < result.Pagination.Pages, nil
}

func parseHuangguoRanking(rows []map[string]any, pageURL string) (rankingPage, error) {
	failure := errors.New("黄果未返回有效榜单")
	if len(rows) == 0 || len(rows) > 100 {
		return rankingPage{}, failure
	}
	result := rankingPage{Items: make([]rankingItem, 0, len(rows)), TotalPages: 1}
	seen := map[string]bool{}
	for index, row := range rows {
		row["id"] = row["video_id"]
		drama, ok := dramaFromAIMap(row, pageURL, mapString(row, "cate_name"))
		if !ok || !rankingSourceID.MatchString(drama.SourceID) || seen[drama.SourceID] {
			return rankingPage{}, failure
		}
		seen[drama.SourceID] = true
		rank, err := strconv.Atoi(mapString(row, "rank"))
		if err != nil || rank < 1 {
			rank = index + 1
		}
		result.Items = append(result.Items, rankingItem{Rank: rank, Drama: drama, Metric: huangguoRankingMetric(row)})
	}
	return result, nil
}

func huangguoRankingMetric(row map[string]any) string {
	label := mapString(row, "metric_label")
	value, err := strconv.ParseFloat(mapString(row, "metric_value"), 64)
	if label == "" || err != nil {
		return ""
	}
	if value >= 10000 {
		return label + " " + strconv.FormatFloat(value/10000, 'f', 1, 64) + "万"
	}
	return label + " " + strconv.FormatFloat(value, 'f', -1, 64)
}
