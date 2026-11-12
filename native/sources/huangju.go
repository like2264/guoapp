package sources

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"duanjuapp/native/core"
)

// 剧果（huangju.net）：JSON 接口 + 访客令牌。
//
//   - 分类：GET /categories
//   - 列表：GET /dramas?page=&sort=hot|new&category=
//   - 详情：GET /dramas/{slug-id}，分集在 episodes[].id
//   - 播放：GET /play/{episodeID} → url + expiresAt（CloudFront 签名 Cookie）
//
// 所有接口都要 Bearer 访客令牌，由 POST /auth/guest（deviceId）获取。
const (
	sourceHuangju      = "huangju"
	huangjuName        = "剧果"
	huangjuSiteURL     = "https://huangju.net"
	huangjuAPIURL      = "https://api.huangju.net"
	huangjuNewestCat   = "@new"
	huangjuUserAgent   = "Mozilla/5.0 (Linux; Android 11; Pixel 5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.91 Mobile Safari/537.36"
	huangjuMaxPage     = 1000000
	huangjuDefaultSize = 20
	huangjuMaxEpisodes = 10000
)

var (
	errHuangjuGuestExpired = errors.New("剧果访客授权已失效，请重试")
	huangjuSignedCookie    = regexp.MustCompile(`(?:^|[,;\s])CloudFront-(Policy|Signature|Key-Pair-Id)\s*=\s*([^;,\s"']+)`)
)

var huangjuDescriptor = core.SourceDescriptor{
	ID:            sourceHuangju,
	Name:          huangjuName,
	Description:   "短剧分类 · 在线搜索",
	BaseURL:       huangjuSiteURL,
	DefaultOff:    true,
	HostSuffixes:  []string{"huangju.net"},
	ValidCategory: validHuangjuCategory,
}

func init() {
	core.Register(huangjuDescriptor, func(host core.Host) core.Provider {
		return &huangjuProvider{host: host}
	})
}

type huangjuProvider struct {
	host core.Host

	mu       sync.Mutex
	deviceID string
	token    string
}

func (p *huangjuProvider) Descriptor() core.SourceDescriptor { return huangjuDescriptor }

func validHuangjuID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 450 && value != "." && value != ".." &&
		!strings.ContainsAny(value, "/\\:?#%|\"<>") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validHuangjuCategory(category string) bool {
	category = strings.TrimSpace(category)
	if category == "" || category == huangjuNewestCat {
		return true
	}
	return validHuangjuID(category) && !strings.HasPrefix(category, "@")
}

func (p *huangjuProvider) Categories(ctx context.Context, force bool) ([]core.Category, error) {
	response, err := p.get(ctx, "/categories", nil, 256<<10)
	if err != nil {
		return nil, err
	}
	rows, valid := huangjuPayload(response).([]any)
	if !valid || len(rows) > 300 {
		return nil, errors.New("剧果分类数据格式无效")
	}
	categories := []core.Category{{ID: huangjuNewestCat, Name: "最新"}}
	seen := map[string]bool{huangjuNewestCat: true}
	for _, entry := range rows {
		row, _ := entry.(map[string]any)
		id, name := core.MapString(row, "slug"), core.MapString(row, "name")
		if id == "hot" {
			continue
		}
		if !validHuangjuCategory(id) || id == "" || name == "" || len([]rune(name)) > 48 || seen[id] {
			return nil, errors.New("剧果分类数据包含无效或重复条目")
		}
		seen[id] = true
		categories = append(categories, core.Category{ID: id, Name: name})
	}
	return categories, nil
}

func (p *huangjuProvider) CatalogPage(ctx context.Context, page int, category string) ([]core.Drama, bool, error) {
	if page < 1 || page > huangjuMaxPage || !validHuangjuCategory(category) {
		return nil, false, errors.New("剧果目录查询参数无效")
	}
	values := url.Values{"page": {strconv.Itoa(page)}}
	switch {
	case category == huangjuNewestCat:
		values.Set("sort", "new")
	case category == "":
		values.Set("sort", "hot")
	default:
		values.Set("category", category)
	}
	response, err := p.get(ctx, "/dramas", values, 4<<20)
	if err != nil {
		return nil, false, err
	}
	row, _ := huangjuPayload(response).(map[string]any)
	rows, valid := row["items"].([]any)
	if !valid || len(rows) > 500 {
		return nil, false, errors.New("剧果目录数据格式无效")
	}
	pageSize := huangjuDefaultSize
	if row["pageSize"] != nil {
		pageSize, valid = huangjuInteger(row["pageSize"], 500)
		if !valid || pageSize < 1 {
			return nil, false, errors.New("剧果分页数据格式无效")
		}
	}
	if row["page"] != nil {
		actual, valid := huangjuInteger(row["page"], huangjuMaxPage)
		if !valid {
			return nil, false, errors.New("剧果分页数据格式无效")
		}
		if actual != page {
			if actual < page {
				// 站点会把超出末页的请求钳回前面的页码，按已到底结束，不要当成错误弹给用户。
				return nil, false, nil
			}
			return nil, false, errors.New("剧果返回的页码与请求不符")
		}
	}
	if len(rows) > pageSize {
		return nil, false, errors.New("剧果分页数据格式无效")
	}
	hasMore := len(rows) >= pageSize
	if row["total"] != nil {
		total, valid := huangjuInteger(row["total"], 100000000)
		if !valid || total < len(rows) {
			return nil, false, errors.New("剧果分页总数无效")
		}
		hasMore = page < (total+pageSize-1)/pageSize
	}
	if hasMore && len(rows) == 0 {
		return nil, false, errors.New("剧果未返回应有的目录页，请重试")
	}
	site := p.host.BaseURL(sourceHuangju)
	items := make([]core.Drama, 0, len(rows))
	seen := map[string]bool{}
	for _, entry := range rows {
		value, _ := entry.(map[string]any)
		drama, err := huangjuDramaFromMap(value, site, "")
		if err != nil {
			return nil, false, err
		}
		if seen[drama.ID] {
			return nil, false, errors.New("剧果目录包含重复剧集，请重试")
		}
		seen[drama.ID] = true
		items = append(items, drama)
	}
	return items, hasMore, nil
}

func (p *huangjuProvider) Search(ctx context.Context, page int, query string) ([]core.Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 128 {
		return nil, false, errors.New("剧果搜索关键词无效")
	}
	values := url.Values{"page": {strconv.Itoa(max(1, page))}, "q": {query}}
	response, err := p.get(ctx, "/dramas", values, 4<<20)
	if err != nil {
		return nil, false, err
	}
	row, _ := huangjuPayload(response).(map[string]any)
	rows, valid := row["items"].([]any)
	if !valid {
		return nil, false, errors.New("剧果搜索数据格式无效")
	}
	pageSize := huangjuDefaultSize
	if size, ok := huangjuInteger(row["pageSize"], 500); ok && size > 0 {
		pageSize = size
	}
	site := p.host.BaseURL(sourceHuangju)
	items := make([]core.Drama, 0, len(rows))
	for _, entry := range rows {
		value, _ := entry.(map[string]any)
		if drama, err := huangjuDramaFromMap(value, site, ""); err == nil {
			items = append(items, drama)
		}
	}
	hasMore := len(rows) >= pageSize
	if row["total"] != nil {
		if total, ok := huangjuInteger(row["total"], 100000000); ok {
			hasMore = page < (total+pageSize-1)/pageSize
		}
	}
	return items, hasMore, nil
}

func (p *huangjuProvider) Detail(ctx context.Context, sourceID string) (core.Drama, []core.Chapter, error) {
	if !validHuangjuID(sourceID) {
		return core.Drama{}, nil, errors.New("剧果剧集地址无效")
	}
	response, err := p.get(ctx, "/dramas/"+url.PathEscape(sourceID), nil, 8<<20)
	if err != nil {
		return core.Drama{}, nil, err
	}
	site := p.host.BaseURL(sourceHuangju)
	row, _ := huangjuPayload(response).(map[string]any)
	drama, err := huangjuDramaFromMap(row, site, sourceID)
	if err != nil {
		return core.Drama{}, nil, err
	}
	rows, valid := row["episodes"].([]any)
	if !valid || len(rows) > huangjuMaxEpisodes {
		return core.Drama{}, nil, errors.New("剧果分集目录格式无效")
	}
	chapters := make([]core.Chapter, 0, len(rows))
	seenIDs, seenNumbers := map[string]bool{}, map[int]bool{}
	for index, entry := range rows {
		episode, _ := entry.(map[string]any)
		if playable, ok := episode["playable"].(bool); ok && !playable {
			continue
		}
		id := core.MapString(episode, "id")
		if !validHuangjuID(id) || seenIDs[id] {
			return core.Drama{}, nil, errors.New("剧果分集目录包含无效或重复分集")
		}
		number := index + 1
		if episode["epNo"] != nil {
			var valid bool
			number, valid = huangjuInteger(episode["epNo"], 100000)
			if !valid {
				return core.Drama{}, nil, errors.New("剧果分集编号无效")
			}
			if number == 0 {
				number = index + 1
			}
		}
		if seenNumbers[number] {
			return core.Drama{}, nil, errors.New("剧果分集编号重复，请刷新目录")
		}
		seenIDs[id], seenNumbers[number] = true, true
		chapters = append(chapters, core.Chapter{
			ID: core.ProviderChapterID(sourceHuangju, sourceID, id), Source: sourceHuangju,
			Title:          fmt.Sprintf("第 %d 集", number),
			CurrentEpisode: core.RawEpisode(number),
			VideoURL:       "huangju-play://" + url.PathEscape(id),
			Referer:        site + "/",
		})
	}
	core.SortProviderChapters(chapters)
	return drama, chapters, nil
}

func (p *huangjuProvider) ResolveMedia(ctx context.Context, task core.Task) (core.ProviderMedia, error) {
	source, sourceID, valid := core.SplitProviderDramaID(task.DramaID)
	prefix := core.ProviderChapterID(sourceHuangju, sourceID, "")
	if !valid || source != sourceHuangju || !validHuangjuID(sourceID) || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return core.ProviderMedia{}, errors.New("剧果播放分集信息无效，请刷新详情")
	}
	id := strings.TrimPrefix(task.Chapter.ID, prefix)
	if !validHuangjuID(id) {
		return core.ProviderMedia{}, errors.New("剧果分集地址无效")
	}
	response, headers, err := p.getWithHeaders(ctx, "/play/"+url.PathEscape(id), nil, 64<<10)
	if err != nil {
		return core.ProviderMedia{}, err
	}
	site := p.host.BaseURL(sourceHuangju)
	row, _ := huangjuPayload(response).(map[string]any)
	address, _ := row["url"].(string)
	credentials, err := huangjuCredentials(headers, address, site, row["expiresAt"])
	if err != nil {
		return core.ProviderMedia{}, err
	}
	media := core.ProviderMedia{URL: address, Referer: credentials.Referer, Credentials: credentials}
	if parsed, err := url.Parse(address); err == nil && strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8") {
		playlist, finalURL, err := p.host.FetchPlaylist(ctx, media.URL, media.Referer, credentials)
		if err != nil {
			return core.ProviderMedia{}, fmt.Errorf("获取剧果播放列表失败：%w", err)
		}
		media.Playlist, media.URL = playlist, finalURL
	}
	return media, nil
}

// ---------- 请求层 ----------

func (p *huangjuProvider) guestToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	if p.token != "" {
		token := p.token
		p.mu.Unlock()
		return token, nil
	}
	if p.deviceID == "" {
		var identifier [16]byte
		if _, err := rand.Read(identifier[:]); err != nil {
			p.mu.Unlock()
			return "", errors.New("无法初始化剧果访客会话")
		}
		identifier[6] = identifier[6]&0x0f | 0x40
		identifier[8] = identifier[8]&0x3f | 0x80
		p.deviceID = fmt.Sprintf("%x-%x-%x-%x-%x", identifier[:4], identifier[4:6], identifier[6:8], identifier[8:10], identifier[10:])
	}
	deviceID := p.deviceID
	p.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"deviceId": deviceID})
	payload, _, err := p.do(ctx, http.MethodPost, "/auth/guest", nil, body, "", 64<<10)
	if err != nil {
		return "", err
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	_ = decoder.Decode(&decoded)
	token := ""
	if row, ok := decoded.(map[string]any); ok {
		token, _ = row["token"].(string)
		if token == "" {
			if data, ok := row["data"].(map[string]any); ok {
				token, _ = data["token"].(string)
			}
		}
	}
	if token == "" || len(token) > 8192 || strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r >= 127 }) >= 0 {
		return "", errors.New("剧果未返回有效的访客授权")
	}
	p.mu.Lock()
	p.token = token
	p.mu.Unlock()
	return token, nil
}

func (p *huangjuProvider) get(ctx context.Context, route string, query url.Values, limit int64) (any, error) {
	value, _, err := p.getWithHeaders(ctx, route, query, limit)
	return value, err
}

func (p *huangjuProvider) getWithHeaders(ctx context.Context, route string, query url.Values, limit int64) (any, http.Header, error) {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := p.guestToken(ctx)
		if err != nil {
			return nil, nil, err
		}
		response, headers, err := p.do(ctx, http.MethodGet, route, query, nil, token, limit)
		if attempt == 0 && errors.Is(err, errHuangjuGuestExpired) {
			p.mu.Lock()
			if p.token == token {
				p.token = ""
			}
			p.mu.Unlock()
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(string(response)))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, nil, errors.New("剧果返回的数据格式无效")
		}
		return decoded, headers, nil
	}
	return nil, nil, errHuangjuGuestExpired
}

func (p *huangjuProvider) do(ctx context.Context, method, route string, query url.Values, body []byte, token string, limit int64) ([]byte, http.Header, error) {
	site := strings.TrimRight(p.host.BaseURL(sourceHuangju), "/")
	address := huangjuAPIURL + route
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	header := map[string]string{
		"User-Agent":      huangjuUserAgent,
		"Accept":          "application/json, text/plain, */*",
		"Accept-Language": "zh-CN,zh;q=0.9",
		"Referer":         site + "/",
		"Origin":          site,
	}
	if len(body) > 0 {
		header["Content-Type"] = "application/json"
	}
	if token != "" {
		header["Authorization"] = "Bearer " + token
	}
	response, err := p.host.Fetch(ctx, core.HTTPRequest{
		Label:     huangjuName,
		Method:    method,
		URL:       address,
		Header:    header,
		Body:      body,
		Limit:     limit,
		Timeout:   15 * time.Second,
		AnyStatus: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("剧果连接失败：%w", core.PublicError(err))
	}
	if response.Status == http.StatusUnauthorized || response.Status == http.StatusForbidden {
		if token != "" && json.Valid(response.Body) {
			return nil, nil, errHuangjuGuestExpired
		}
		return nil, nil, fmt.Errorf("剧果接口请求被拒绝 (HTTP %d)", response.Status)
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, nil, fmt.Errorf("剧果接口暂时不可用 (HTTP %d)", response.Status)
	}
	return response.Body, http.Header(response.Header), nil
}

// ---------- 解析 ----------

func huangjuPayload(value any) any {
	if row, ok := value.(map[string]any); ok && row["data"] != nil && row["id"] == nil && row["items"] == nil && row["url"] == nil {
		return row["data"]
	}
	return value
}

func huangjuInteger(value any, maximum int) (int, bool) {
	number, err := strconv.Atoi(core.NativeText(value))
	return number, err == nil && number >= 0 && number <= maximum
}

func huangjuCategoryNames(row map[string]any) []string {
	var names []string
	seen := map[string]bool{}
	add := func(value any) {
		name := ""
		switch typed := value.(type) {
		case map[string]any:
			name, _ = typed["name"].(string)
		case string:
			name = typed
		}
		name = strings.TrimSpace(name)
		if name != "" && len([]rune(name)) <= 48 && !seen[name] && len(names) < 24 {
			seen[name] = true
			names = append(names, name)
		}
	}
	add(row["category"])
	if entries, ok := row["categories"].([]any); ok {
		for _, entry := range entries {
			add(entry)
		}
	}
	return names
}

func huangjuDramaFromMap(row map[string]any, site, sourceID string) (core.Drama, error) {
	id := core.MapString(row, "id")
	slug := core.MapString(row, "slug")
	title, _ := row["title"].(string)
	if !validHuangjuID(id) || strings.TrimSpace(title) == "" {
		return core.Drama{}, errors.New("剧果返回的剧集信息不完整")
	}
	if sourceID == "" {
		if !validHuangjuID(slug) {
			return core.Drama{}, errors.New("剧果返回的剧集地址无效")
		}
		sourceID = slug + "-" + id
	}
	if !validHuangjuID(sourceID) || !strings.HasSuffix(sourceID, "-"+id) {
		return core.Drama{}, errors.New("剧果详情与请求剧集不符")
	}
	episodes := 0
	if row["totalEpisodes"] != nil {
		var valid bool
		episodes, valid = huangjuInteger(row["totalEpisodes"], 100000)
		if !valid {
			return core.Drama{}, errors.New("剧果集数格式无效")
		}
	}
	categories := huangjuCategoryNames(row)
	category := "短剧"
	if len(categories) > 0 {
		category = categories[0]
	}
	tags := append([]string{}, categories...)
	if region := core.MapString(row, "region"); region != "" {
		tags = append(tags, core.Truncate(region, 48))
	}
	if year, valid := huangjuInteger(row["year"], 9999); valid && year >= 1000 {
		tags = append(tags, strconv.Itoa(year)+"年")
	}
	status := core.MapString(row, "status")
	if status == "completed" {
		status = "finished"
	}
	if status != "finished" && status != "ongoing" {
		status = ""
	}
	description, _ := row["description"].(string)
	cover := huangjuCover(row["coverUrl"], site+"/")
	return core.Drama{
		ID: core.ProviderDramaID(sourceHuangju, sourceID), Source: sourceHuangju, SourceID: sourceID,
		Title: core.Truncate(title, 256), Name: core.Truncate(title, 256),
		Desc: core.Truncate(description, 12000), Intro: core.Truncate(description, 12000),
		Cover: cover, CoverURL: cover, TotalEpisode: episodes, EpisodeCount: episodes,
		Category: category, ChannelName: huangjuName, Tags: tags, ReleaseStatus: status,
		Score: core.MapString(row, "score"),
	}, nil
}

func huangjuCover(value any, baseURL string) string {
	raw := core.NativeText(value)
	if raw == "" {
		return ""
	}
	address := core.ResolveProviderURL(baseURL, raw)
	parsed, err := url.Parse(address)
	if err != nil || !core.ValidNativeCoverURL(parsed) {
		return ""
	}
	return parsed.String()
}

func huangjuExpiration(value any) time.Time {
	text := core.NativeText(value)
	if stamp, err := strconv.ParseInt(text, 10, 64); err == nil && stamp > 0 && stamp <= 253402300799000 {
		if stamp > 100000000000 {
			return time.UnixMilli(stamp)
		}
		return time.Unix(stamp, 0)
	}
	parsed, _ := time.Parse(time.RFC3339Nano, text)
	return parsed
}

func huangjuCredentials(headers http.Header, address, site string, expiration any) (*core.MediaCredentials, error) {
	cookies := map[string]string{}
	expires := huangjuExpiration(expiration)
	for _, header := range headers.Values("Set-Cookie") {
		for _, cookie := range strings.Split(header, ";") {
			name, value, found := strings.Cut(strings.TrimSpace(cookie), "=")
			if !found {
				continue
			}
			switch name {
			case "CloudFront-Policy", "CloudFront-Signature", "CloudFront-Key-Pair-Id":
				cookies[name] = value
			}
		}
		for _, match := range huangjuSignedCookie.FindAllStringSubmatch(header, 12) {
			cookies["CloudFront-"+match[1]] = match[2]
		}
	}
	var values []string
	for _, name := range []string{"CloudFront-Policy", "CloudFront-Signature", "CloudFront-Key-Pair-Id"} {
		value := cookies[name]
		if value == "" || len(value) > 8192 || strings.ContainsAny(value, ",;\"\\") ||
			strings.IndexFunc(value, func(r rune) bool { return r <= 32 || r >= 127 }) >= 0 {
			return nil, errors.New("剧果未返回完整的媒体凭证，请重试")
		}
		values = append(values, name+"="+value)
	}
	if !expires.IsZero() && !time.Now().Before(expires) {
		return nil, errors.New("剧果媒体凭证已过期，请检查设备时间后重试")
	}
	parsed, err := url.Parse(address)
	if err != nil || !core.IsProviderHTTPMediaURL(address) || parsed.User != nil {
		return nil, errors.New("剧果播放地址无效")
	}
	return &core.MediaCredentials{
		Cookie:    strings.Join(values, "; "),
		Origin:    core.ProviderMediaOrigin(parsed),
		Referer:   site + "/",
		UserAgent: huangjuUserAgent,
		Expires:   expires,
	}, nil
}
