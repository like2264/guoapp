package sources

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"duanjuapp/native/core"
)

// 黄果旧版（cloudfront 双域名）：AES 参数加密的 JSON 接口，匿名访客登录。
//
//   - 列表：/api/app/playlet-tab/all 取分类，/api/app/playlet/home/tab/{id} 逐页翻，
//     推荐位 /api/app/playlet-recommend/home/choose。
//   - 搜索：/api/app/playlet/keyword/search。
//   - 详情：/api/app/playlet/detail/{id}，分集回退 /api/app/playlet-chapter/list/{id}。
//   - 播放：/api/app/vid/h5/m3u8/{vid}?token=&c=。
//
// 站点只认 pageNumber（count/hasNext 都是假值），所以「全部」分类靠逐页翻到空页合并，
// 结果在 provider 内缓存 10 分钟。
const (
	sourceCloudFront      = "cloudfront"
	legacyName            = "黄果旧版"
	legacyFrontendURL     = "https://d2pypzndaqisk.cloudfront.net"
	legacyDefaultAPIBase  = "https://dr6skssi3nxbk.cloudfront.net"
	legacyDefaultCDNURL   = "https://sjljsla.lkkwip.cn"
	legacyPicCDNURL       = "https://pic.zdmhyg.cn/"
	legacyStaticCDNURL    = "https://zzzznnn.lkkwip.cn/"
	legacyRecommendID     = "recommend"
	legacyRecommendName   = "推荐"
	legacyCatalogMaxPage  = 40
	legacyCatalogMaxItems = 1200
	legacyPageSize        = 30
	legacyMaxBodyBytes    = 8 << 20
	legacySessionName     = "huangguo-session"
	legacyUserAgent       = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	legacyXUserAgent      = "okhttp/3.12.0 DevID=00000000000000000000000000000"
)

var (
	legacyCatalogTTL = 10 * time.Minute
	legacyPageGap    = 200 * time.Millisecond

	legacyAPIFallbackHosts = []string{
		legacyDefaultAPIBase,
		"https://d18ka9rqpfd3lo.cloudfront.net",
		"https://d37n0wjehmw08f.cloudfront.net",
	}

	reLegacyDeviceID = regexp.MustCompile(`^[A-Fa-f0-9]{16}[0-9]{13}$`)
	reLegacyScript   = regexp.MustCompile(`(?i)<script\b[^>]*\bsrc=["']([^"']+)["']`)
	reLegacyAPIHost  = regexp.MustCompile(`https://[a-z0-9]+\.cloudfront\.net`)
)

var legacyDescriptor = core.SourceDescriptor{
	ID:                 sourceCloudFront,
	Name:               legacyName,
	Description:        "黄果经典站 · 全站检索",
	BaseURL:            legacyFrontendURL,
	DefaultOff:         true,
	HostSuffixes:       []string{"cloudfront.net"},
	AllowEmptyCatalog:  true,
	KeepDetailEpisodes: true,
}

func init() {
	core.Register(legacyDescriptor, func(host core.Host) core.Provider {
		return &legacyProvider{
			host:        host,
			walks:       map[string]*legacyWalk{},
			apiFailures: map[string]time.Time{},
		}
	})
}

type legacyProvider struct {
	host core.Host

	mu    sync.Mutex
	walks map[string]*legacyWalk

	apiMu       sync.Mutex
	apiBase     string
	apiFailures map[string]time.Time

	sessionMu sync.Mutex
	session   legacySessionState
	loaded    bool
	access    *legacyAccess
}

func (p *legacyProvider) Descriptor() core.SourceDescriptor { return legacyDescriptor }

// ---------- 协议与信封 ----------

type legacyProtocol struct {
	InterfaceKey string `json:"interfaceKey"`
	ParamKey     string `json:"paramKey"`
	ParamIV      string `json:"paramIV"`
}

type legacySessionState struct {
	Version   int            `json:"version"`
	Scope     string         `json:"scope"`
	DeviceID  string         `json:"deviceId"`
	Token     string         `json:"token"`
	Protocol  legacyProtocol `json:"protocol"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

type legacyAccess struct {
	legacyProtocol
	Token     string
	DeviceID  string
	Anonymous bool
}

type legacyAPIError struct{ code, message string }

func (err *legacyAPIError) Error() string {
	return fmt.Sprintf("黄果旧版接口错误 %s: %s", err.code, err.message)
}

type legacyTab struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type legacyTabList []legacyTab

func (tabs *legacyTabList) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		return json.Unmarshal(raw, (*[]legacyTab)(tabs))
	}
	var wrapped struct {
		List []legacyTab `json:"list"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return err
	}
	*tabs = wrapped.List
	return nil
}

type legacyEnvelope struct {
	Code json.RawMessage `json:"code"`
	Msg  string          `json:"msg"`
	Hash json.RawMessage `json:"hash"`
	Data json.RawMessage `json:"data"`
}

type legacyList struct {
	List []core.Drama `json:"list"`
}

func legacyHashEnabled(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(strings.ToLower(s))
		return s != "" && s != "false" && s != "0" && s != "null"
	}
	return true
}

func legacyPKCS7Pad(src []byte, blockSize int) []byte {
	pad := blockSize - len(src)%blockSize
	return append(src, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func legacyPKCS7Unpad(src []byte, blockSize int) ([]byte, error) {
	if len(src) == 0 || len(src)%blockSize != 0 {
		return nil, errors.New("invalid pkcs7 length")
	}
	pad := int(src[len(src)-1])
	if pad == 0 || pad > blockSize || pad > len(src) {
		return nil, errors.New("invalid pkcs7 padding")
	}
	for _, v := range src[len(src)-pad:] {
		if int(v) != pad {
			return nil, errors.New("invalid pkcs7 padding bytes")
		}
	}
	return src[:len(src)-pad], nil
}

func legacyEncryptParam(obj any, key, iv []byte) (string, error) {
	plain, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := legacyPKCS7Pad(plain, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func legacySHA256(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func legacyDecryptResponse(dataStr string, interfaceKey []byte) ([]byte, error) {
	o, err := base64.StdEncoding.DecodeString(strings.TrimSpace(dataStr))
	if err != nil {
		return nil, err
	}
	if len(o) < 12 {
		return nil, errors.New("encrypted payload too short")
	}
	i := o[:12]
	r := append(append([]byte{}, interfaceKey...), i...)
	l := len(r) >> 1
	s := legacySHA256(r)[8:24]
	c := append(append([]byte{}, s...), r[:l]...)
	a := append(append([]byte{}, r[l:]...), s...)
	d := legacySHA256(c)
	f := legacySHA256(a)
	m := append(append(append([]byte{}, d[:8]...), f[8:24]...), d[24:]...)
	p := append(append(append([]byte{}, f[:4]...), d[12:20]...), f[28:]...)
	g := o[12:]
	block, err := aes.NewCipher(m)
	if err != nil {
		return nil, err
	}
	if len(g)%block.BlockSize() != 0 {
		return nil, errors.New("ciphertext is not block aligned")
	}
	plain := make([]byte, len(g))
	cipher.NewCBCDecrypter(block, p).CryptBlocks(plain, g)
	return legacyPKCS7Unpad(plain, block.BlockSize())
}

func legacyValidProtocol(protocol legacyProtocol) bool {
	return len(protocol.InterfaceKey) > 0 && len(protocol.InterfaceKey) <= 512 &&
		len(protocol.ParamKey) == 16 && len(protocol.ParamIV) == 16
}

func legacyValidToken(token string) bool {
	return len(token) <= 4096 && !strings.ContainsAny(token, "\r\n\t ")
}

func legacyParseProtocol(script string) (legacyProtocol, error) {
	value := func(name string) string {
		pattern := `["']` + regexp.QuoteMeta(name) + `["']\s*[:,]\s*["']([^"'\\\r\n]+)["']`
		match := regexp.MustCompile(pattern).FindStringSubmatch(script)
		if len(match) == 2 {
			return match[1]
		}
		return ""
	}
	protocol := legacyProtocol{
		InterfaceKey: value("interfaceKey"),
		ParamKey:     value("parameterKey"),
		ParamIV:      value("parameterIv"),
	}
	if !legacyValidProtocol(protocol) {
		return legacyProtocol{}, errors.New("黄果旧版官网协议已变化，无法识别当前接口参数")
	}
	return protocol, nil
}

// ---------- 会话 ----------

func (p *legacyProvider) credentials(ctx context.Context) (legacyAccess, error) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if p.access != nil {
		return *p.access, nil
	}
	if !p.loaded {
		p.session = p.loadSession()
		p.loaded = true
	}
	access, updated, err := p.bootstrapSession(ctx, p.session)
	p.session = updated
	if err != nil {
		return legacyAccess{}, err
	}
	p.access = &access
	return access, nil
}

func (p *legacyProvider) invalidateToken(access legacyAccess) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if access.Anonymous && p.access != nil && p.access.Token == access.Token {
		p.session.Token = ""
		p.access = nil
	}
}

func (p *legacyProvider) loadSession() legacySessionState {
	empty := legacySessionState{Version: 1, Scope: legacyDefaultAPIBase}
	raw, err := p.host.LoadState(legacySessionName)
	if err != nil || len(raw) == 0 {
		return empty
	}
	var state legacySessionState
	if json.Unmarshal(raw, &state) != nil || state.Version != 1 || state.Scope != empty.Scope {
		return empty
	}
	if !reLegacyDeviceID.MatchString(state.DeviceID) || !legacyValidToken(state.Token) {
		return empty
	}
	return state
}

func (p *legacyProvider) saveSession(state legacySessionState) {
	raw, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = p.host.SaveState(legacySessionName, raw)
}

func (p *legacyProvider) bootstrapSession(ctx context.Context, state legacySessionState) (legacyAccess, legacySessionState, error) {
	access := legacyAccess{Anonymous: true}
	protocol := state.Protocol
	if !legacyValidProtocol(protocol) || time.Since(state.UpdatedAt) > 24*time.Hour {
		script, err := p.frontendScript(ctx)
		if err != nil {
			return access, state, fmt.Errorf("获取黄果旧版官网协议失败: %w", core.PublicError(err))
		}
		protocol, err = legacyParseProtocol(script)
		if err != nil {
			return access, state, err
		}
		state.Protocol, state.UpdatedAt = protocol, time.Now()
	}
	access.legacyProtocol = protocol
	if access.Anonymous {
		if state.DeviceID == "" {
			state.DeviceID = legacyRandomDeviceID()
		}
		access.DeviceID, access.Token = state.DeviceID, state.Token
		if access.Token == "" {
			payload, err := p.request(ctx, http.MethodPost, "/api/app/mine/login/h5", map[string]any{
				"devID": access.DeviceID, "sysType": "ios", "isAppStore": false,
			}, access)
			if err != nil {
				return access, state, fmt.Errorf("黄果旧版访客登录失败: %w", err)
			}
			var login struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(payload, &login) != nil || !legacyValidToken(login.Token) || login.Token == "" {
				return access, state, errors.New("黄果旧版访客登录未返回有效令牌")
			}
			access.Token, state.Token = login.Token, login.Token
		}
		p.saveSession(state)
	}
	return access, state, nil
}

func legacyRandomDeviceID() string {
	const alphabet = "0123456789ABCDEF"
	buf := make([]byte, 8)
	for i := range buf {
		buf[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return string(buf) + strconv.FormatInt(time.Now().UnixMilli(), 10)
}

func (access legacyAccess) userAgent() string {
	if access.DeviceID == "" {
		return legacyXUserAgent
	}
	return strings.Replace(legacyXUserAgent, "DevID=00000000000000000000000000000", "DevID="+access.DeviceID, 1)
}

// ---------- HTTP ----------

func (p *legacyProvider) text(ctx context.Context, address string) (string, error) {
	response, err := p.host.FetchText(ctx, core.TextRequest{
		URL:       address,
		Referer:   legacyFrontendURL + "/",
		UserAgent: legacyUserAgent,
	})
	if err != nil {
		return "", err
	}
	return response.Body, nil
}

func (p *legacyProvider) frontendScript(ctx context.Context) (string, error) {
	page, err := p.text(ctx, legacyFrontendURL+"/")
	if err != nil {
		return "", err
	}
	for _, script := range reLegacyScript.FindAllStringSubmatch(page, -1) {
		if !strings.Contains(script[1], "/main-") || !strings.HasSuffix(script[1], ".js") {
			continue
		}
		assetURL := core.ResolveProviderURL(legacyFrontendURL+"/", script[1])
		parsed, err := url.Parse(assetURL)
		if err != nil || !strings.EqualFold(parsed.Hostname(), "d2pypzndaqisk.cloudfront.net") {
			continue
		}
		return p.text(ctx, assetURL)
	}
	return "", errors.New("黄果旧版官网未提供可识别的接口脚本")
}

func (p *legacyProvider) apiEndpoint(ctx context.Context) (string, error) {
	p.apiMu.Lock()
	if p.apiBase != "" {
		base := p.apiBase
		p.apiMu.Unlock()
		return base, nil
	}
	p.apiMu.Unlock()

	seen := map[string]bool{}
	candidates := append([]string{legacyDefaultAPIBase}, legacyAPIFallbackHosts...)
	var lastErr error
	for pass := 0; pass < 2; pass++ {
		if pass == 1 {
			script, err := p.frontendScript(ctx)
			if err != nil {
				lastErr = err
				break
			}
			hosts := []string{}
			for _, host := range reLegacyAPIHost.FindAllString(script, -1) {
				if host != legacyFrontendURL {
					hosts = append(hosts, host)
				}
			}
			candidates = hosts
		}
		for _, candidate := range candidates {
			if seen[candidate] {
				continue
			}
			seen[candidate] = true
			p.apiMu.Lock()
			cooling := time.Now().Before(p.apiFailures[candidate])
			p.apiMu.Unlock()
			if cooling {
				continue
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := p.probeEndpoint(ctx, candidate); err != nil {
				lastErr = err
				continue
			}
			p.apiMu.Lock()
			p.apiBase = candidate
			p.apiMu.Unlock()
			return candidate, nil
		}
	}
	return "", fmt.Errorf("黄果旧版接口域名均不可用: %w", core.PublicError(lastErr))
}

func (p *legacyProvider) probeEndpoint(ctx context.Context, host string) error {
	response, err := p.host.Fetch(ctx, core.HTTPRequest{
		Label:     legacyName,
		Method:    http.MethodGet,
		URL:       host + "/api/app/ping/check",
		Header:    map[string]string{"User-Agent": legacyUserAgent, "Accept": "application/json"},
		Limit:     64 << 10,
		Timeout:   6 * time.Second,
		AnyStatus: true,
	})
	if err != nil {
		return core.PublicError(err)
	}
	if response.Status != http.StatusOK {
		return fmt.Errorf("接口健康检查失败 (HTTP %d)", response.Status)
	}
	var envelope struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(response.Body, &envelope) != nil || envelope.Code != 200 {
		return errors.New("接口健康检查未返回预期响应")
	}
	return nil
}

func (p *legacyProvider) resetEndpoint(failed string) {
	p.apiMu.Lock()
	if p.apiFailures == nil {
		p.apiFailures = map[string]time.Time{}
	}
	p.apiFailures[failed] = time.Now().Add(time.Minute)
	if p.apiBase == failed {
		p.apiBase = ""
	}
	p.apiMu.Unlock()
}

func (p *legacyProvider) request(ctx context.Context, method, apiPath string, params any, access legacyAccess) ([]byte, error) {
	attempts := p.host.Retries()
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		fullPath := apiPath
		var body []byte
		if params != nil {
			enc, err := legacyEncryptParam(params, []byte(access.ParamKey), []byte(access.ParamIV))
			if err != nil {
				return nil, err
			}
			if method == http.MethodGet {
				separator := "?"
				if strings.Contains(fullPath, "?") {
					separator = "&"
				}
				fullPath += separator + "data=" + url.QueryEscape(enc)
			} else {
				body, _ = json.Marshal(map[string]string{"data": enc})
			}
		}
		base, err := p.apiEndpoint(ctx)
		if err != nil {
			return nil, err
		}
		header := map[string]string{
			"Accept":       "application/json",
			"temp":         "test",
			"X-User-Agent": access.userAgent(),
			"Origin":       legacyFrontendURL,
			"Referer":      legacyFrontendURL + "/",
			"User-Agent":   legacyUserAgent,
		}
		if access.Token != "" {
			header["Authorization"] = access.Token
		}
		if body != nil {
			header["Content-Type"] = "application/json"
		}
		response, err := p.host.Fetch(ctx, core.HTTPRequest{
			Label:     legacyName,
			Method:    method,
			URL:       base + fullPath,
			Header:    header,
			Body:      body,
			Limit:     legacyMaxBodyBytes,
			Timeout:   20 * time.Second,
			AnyStatus: true,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, core.PublicError(err)
			}
			lastErr = core.PublicError(err)
			p.resetEndpoint(base)
			continue
		}
		if response.Status >= 500 {
			lastErr = fmt.Errorf("黄果旧版接口暂时不可用 (HTTP %d)", response.Status)
			p.resetEndpoint(base)
			continue
		}
		if response.Status < 200 || response.Status >= 300 {
			return nil, fmt.Errorf("黄果旧版接口请求被拒绝 (HTTP %d)", response.Status)
		}
		var envelope legacyEnvelope
		if err := json.Unmarshal(response.Body, &envelope); err != nil {
			lastErr = err
			continue
		}
		if code := strings.Trim(string(envelope.Code), `"`); code != "" && code != "null" && code != "200" && code != "0" {
			message := core.FirstNonEmpty(envelope.Msg, "请检查访问配置或刷新登录凭证")
			if access.Token != "" {
				message = strings.ReplaceAll(message, access.Token, "[已隐藏]")
			}
			return nil, &legacyAPIError{code: core.Truncate(code, 20), message: core.Truncate(message, 160)}
		}
		payload := envelope.Data
		if legacyHashEnabled(envelope.Hash) && len(envelope.Data) > 0 {
			var encrypted string
			if err := json.Unmarshal(envelope.Data, &encrypted); err != nil {
				lastErr = err
				continue
			}
			plain, err := legacyDecryptResponse(encrypted, []byte(access.InterfaceKey))
			if err != nil {
				lastErr = err
				continue
			}
			payload = plain
		}
		if len(payload) == 0 || string(payload) == "null" {
			return nil, errors.New("黄果旧版接口未返回数据，请检查登录凭证或接口是否变更")
		}
		return payload, nil
	}
	return nil, lastErr
}

// call 带一次访客令牌失效重试的接口调用。
func (p *legacyProvider) call(ctx context.Context, apiPath string, params any, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		access, err := p.credentials(ctx)
		if err != nil {
			return err
		}
		payload, err := p.request(ctx, http.MethodGet, apiPath, params, access)
		var apiErr *legacyAPIError
		if attempt == 0 && access.Anonymous && errors.As(err, &apiErr) && apiErr.code == "5005" {
			p.invalidateToken(access)
			continue
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(payload, out)
	}
	return errors.New("黄果旧版访客登录已失效，请稍后重试")
}

// legacyCoverURL 把站点的相对封面路径补成可访问地址。
func legacyCoverURL(value any) string {
	address := strings.TrimSpace(legacyCoverPath(value))
	if address == "" {
		return ""
	}
	if strings.HasPrefix(address, "//") {
		address = "https:" + address
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.User != nil {
		return ""
	}
	if parsed.IsAbs() {
		if core.ValidNativeCoverURL(parsed) {
			return address
		}
		return ""
	}
	if strings.Contains(address, "..") || strings.Contains(address, "://") {
		return ""
	}
	address = strings.TrimLeft(address, "/")
	if strings.HasPrefix(address, "upload/") || strings.HasPrefix(address, "upload_01/") {
		return legacyPicCDNURL + address
	}
	return legacyStaticCDNURL + address
}

func legacyCoverPath(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64, int, int64:
		return fmt.Sprintf("%v", typed)
	case map[string]any:
		for _, key := range []string{"url", "src", "cover", "image", "path"} {
			if nested, ok := typed[key]; ok {
				if address := legacyCoverPath(nested); address != "" {
					return address
				}
			}
		}
	}
	return ""
}

// legacyMediaKey 读取可选的自定义 AES 密钥（配置项在旧版里可选，这里恒为空）。
func legacyMediaKey() []byte {
	return nil
}

func legacyDecodeHexKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 16 {
		return nil, errors.New("aesKeyHex 必须是 16 字节 AES 密钥的 hex 编码")
	}
	return key, nil
}
