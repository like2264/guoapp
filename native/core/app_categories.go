package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type nativeCategory struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// 黄果 2026-11-13 改版后，AI 换脸(ai-huanlian) 与 AI 魔改(ai-mogai) 两个分类已下线，
// 站点对新旧分类接口统一回 HTTP 410「内容已下线」，首页导航里也只剩 AI 短剧 / AI 漫剧。
// 这里不再保留这两个分类，另外 fetchHuangguoAICatalogPage 仍按单分类失败容错，
// 以后站点再撤别的分类时也不会把整页拉挂。
var nativeAICategories = []nativeCategory{
	{ID: "ai-duanju", Name: "AI 短剧"},
	{ID: "ai-manju", Name: "AI 漫剧"},
}

func nativeCatalogKey(source, category string) string {
	source = canonicalProviderSource(source)
	if category = strings.TrimSpace(category); category != "" {
		return source + "|" + category
	}
	return source
}

func validNativeCategory(source, category string) bool {
	if category == "" {
		return true
	}
	if len(category) > 128 || strings.ContainsAny(category, "|/\\\x00\r\n") {
		return false
	}
	switch source {
	case sourceHongguo:
		for _, genre := range hongguoAppGenres {
			if category == genre.key {
				return true
			}
		}
		return hongguoTagPattern.MatchString(category)
	case sourceHuangguoAI:
		for _, entry := range nativeAICategories {
			if category == entry.ID {
				return true
			}
		}
	case sourceHuangguoVideo:
		id, err := strconv.Atoi(category)
		return err == nil && id > 0
	case sourceCloudFront:
		return strings.TrimSpace(category) == category
	case sourceHuangju:
		return category == huangjuNewestCategory || validHuangjuID(category) && !strings.HasPrefix(category, "@")
	case sourceYeguo:
		return validYeguoCategory(category)
	case sourceJinguoDuanju:
		return validJinguoDuanjuCategory(category)
	case sourceJinguo:
		return validJinguoCategory(category)
	case sourceDSD:
		return webProviderNumericID.MatchString(category)
	case sourceSorani:
		return validSoraniCategory(category)
	case sourceGuipian:
		return validGuipianCategory(category)
	case sourceHanxiaoquan:
		return validHanxiaoquanCategory(category)
	case sourceHeitao:
		return validHeitaoCategory(category)
	case sourceBeabox:
		return validBeaboxCategory(category)
	case sourceXduanju:
		return validXduanjuCategory(category)
	case sourceHuangtao:
		return validHuangtaoCategory(category)
	case sourceYesheai:
		return validYesheaiCategory(category)
	case sourceCrdj:
		return validCrdjCategory(category)
	case sourceYouzidj:
		return validYouzidjCategory(category)
	}
	return false
}

func (engine *nativeEngine) nativeCategories(ctx context.Context, source string, force bool) ([]nativeCategory, error) {
	source = canonicalProviderSource(source)
	all := []nativeCategory{{Name: "全部"}}
	switch source {
	case sourceHongguo:
		for _, genre := range hongguoAppGenres {
			all = append(all, nativeCategory{ID: genre.key, Name: genre.name})
		}
		return engine.hongguoCategories(ctx, all, force), nil
	case sourceHuangguoAI:
		// 站点会临时下线某些分类，被判定为 410 的直接不再出现在标签栏里。
		for _, entry := range nativeAICategories {
			if huangguoCategoryGone(entry.ID) {
				continue
			}
			all = append(all, entry)
		}
		return all, nil
	case sourceHuangdou:
		return all, nil
	}
	engine.mu.Lock()
	cached := append([]nativeCategory{}, engine.categoryOptions[source]...)
	engine.mu.Unlock()
	if len(cached) > 1 && !force {
		return cached, nil
	}
	d := engine.downloader
	var err error
	switch source {
	case sourceCloudFront:
		// 站点首页分类只有 5 个、合计两百多部；推荐位是另一条无限流，并进来能多出上百部。
		all = append(all, nativeCategory{ID: legacyRecommendID, Name: legacyRecommendName})
		var tabs legacyTabList
		err = d.fetchAPI(ctx, "/api/app/playlet-tab/all", nil, &tabs)
		for _, tab := range tabs {
			if validNativeCategory(source, tab.ID) && tab.ID != "" && tab.Name != "" {
				all = append(all, nativeCategory{ID: tab.ID, Name: tab.Name})
			}
		}
	case sourceHuangguoVideo:
		address := d.providerBaseURL(source) + "/videos"
		var body string
		body, err = d.fetchProviderText(ctx, address, d.providerBaseURL(source)+"/")
		if err == nil {
			all = append(all, parseHuangguoVideoCategories(body)...)
		}
	case sourceHuangju:
		var categories []nativeCategory
		categories, err = d.fetchHuangjuCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceYeguo:
		var categories []nativeCategory
		categories, err = d.fetchYeguoCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceJinguoDuanju:
		var categories []nativeCategory
		categories, err = d.fetchJinguoDuanjuCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceJinguo:
		var categories []nativeCategory
		categories, err = d.fetchJinguoCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceDSD:
		var categories []nativeCategory
		categories, err = d.fetchDSDCategories(ctx, force)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceSorani:
		all = append(all, d.fetchSoraniCategories()...)
	case sourceGuipian:
		all = append(all, d.fetchGuipianCategories()...)
	case sourceHanxiaoquan:
		all = append(all, d.fetchHanxiaoquanCategories()...)
	case sourceHeitao:
		var categories []nativeCategory
		categories, err = d.fetchHeitaoCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceBeabox:
		var categories []nativeCategory
		categories, err = d.fetchBeaboxCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceXduanju:
		var categories []nativeCategory
		categories, err = d.fetchXduanjuCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceHuangtao:
		var categories []nativeCategory
		categories, err = d.fetchHuangtaoCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceYesheai:
		var categories []nativeCategory
		categories, err = d.fetchYesheaiCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceCrdj:
		var categories []nativeCategory
		categories, err = d.fetchCrdjCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	case sourceYouzidj:
		var categories []nativeCategory
		categories, err = d.fetchYouzidjCategories(ctx)
		if err == nil {
			all = append(all, categories...)
		}
	default:
		return nil, errors.New("请选择有效站源")
	}
	if err != nil {
		return nil, err
	}
	if len(all) == 1 {
		return nil, errors.New("站源暂未返回内容分类，请稍后重试")
	}
	engine.rememberCategories(source, all)
	return all, nil
}

func (engine *nativeEngine) hongguoCategories(ctx context.Context, genres []nativeCategory, force bool) []nativeCategory {
	engine.mu.Lock()
	cached := append([]nativeCategory{}, engine.categoryOptions[sourceHongguo]...)
	engine.mu.Unlock()
	if len(cached) > len(genres) && !force {
		return cached
	}
	tags, err := engine.downloader.fetchHongguoTagCategories(ctx)
	if err != nil {
		if len(cached) > len(genres) {
			return cached
		}
		return genres
	}
	all := append(genres, tags...)
	engine.rememberCategories(sourceHongguo, all)
	return all
}

func (engine *nativeEngine) rememberCategories(source string, all []nativeCategory) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.categoryOptions == nil {
		engine.categoryOptions = map[string][]nativeCategory{}
	}
	engine.categoryOptions[source] = all
	engine.writeCatalogDiskLocked()
}

var categoryAnchorPattern = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
var categoryTextPattern = regexp.MustCompile(`(?s)<[^>]*>`)

func parseHuangguoVideoCategories(body string) []nativeCategory {
	var categories []nativeCategory
	seen := map[string]bool{}
	for _, match := range categoryAnchorPattern.FindAllStringSubmatch(body, -1) {
		href := html.UnescapeString(extractAttr("<a "+match[1]+">", "href"))
		address, err := url.Parse(href)
		if err != nil || address.Path != "" && strings.TrimRight(address.Path, "/") != "/videos" {
			continue
		}
		id := address.Query().Get("category")
		name := strings.TrimSpace(html.UnescapeString(categoryTextPattern.ReplaceAllString(match[2], "")))
		if !validNativeCategory(sourceHuangguoVideo, id) || id == "" || seen[id] || name == "" || len([]rune(name)) > 24 {
			continue
		}
		seen[id] = true
		categories = append(categories, nativeCategory{ID: id, Name: name})
	}
	return categories
}

const huangguoAICatalogPageSize = 24

// 站点下线某个分类后会稳定返回 410，本次进程内记住它，后续翻页不再白跑一趟。
var huangguoGoneCategories = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

func huangguoCategoryGone(category string) bool {
	huangguoGoneCategories.Lock()
	defer huangguoGoneCategories.Unlock()
	return huangguoGoneCategories.seen[category]
}

func rememberHuangguoCategoryGone(category string) {
	if category == "" {
		return
	}
	huangguoGoneCategories.Lock()
	defer huangguoGoneCategories.Unlock()
	huangguoGoneCategories.seen[category] = true
}

// fetchHuangguoAICatalogPage 把「全部」拼成几个 AI 分类的合集。
//
// 单个分类失败（下架、临时 5xx、超时）不应该让整页翻车：只丢掉那个分类，
// 其余分类照常出内容；只有所有分类都失败且一条都没拿到时才向上报错。
func (d *Downloader) fetchHuangguoAICatalogPage(ctx context.Context, page int, category string) ([]Drama, bool, error) {
	categories := nativeAICategories
	if category != "" {
		// 只认当前分类表里的 ID；旧版本残留下来的已下线分类按「没有内容」处理，
		// 不能退化成「全部」，否则会把别的分类内容冒充成它。
		matched := false
		for _, entry := range categories {
			if entry.ID == category {
				categories = []nativeCategory{entry}
				matched = true
				break
			}
		}
		if !matched {
			return nil, false, nil
		}
	}
	type categoryResult struct {
		items []Drama
		err   error
	}
	results := make([]categoryResult, len(categories))
	slots := make(chan struct{}, 2)
	var group sync.WaitGroup
	for index, entry := range categories {
		group.Add(1)
		go func(index int, entry nativeCategory) {
			defer group.Done()
			if huangguoCategoryGone(entry.ID) {
				return
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				results[index].err = ctx.Err()
				return
			}
			path := fmt.Sprintf("/api/videos/category/%s?sort=hot&page=%d&size=%d", url.PathEscape(entry.ID), page, huangguoAICatalogPageSize)
			var body json.RawMessage
			err := d.huangguoAIAPI(ctx, path, &body)
			if err != nil {
				if isProviderGone(err) {
					rememberHuangguoCategoryGone(entry.ID)
					return
				}
				results[index].err = fmt.Errorf("%s: %w", entry.Name, err)
				return
			}
			results[index].items = parseHuangguoAIJSONCards(body, d.providerBaseURL(sourceHuangguoAI)+"/", entry.Name)
		}(index, entry)
	}
	group.Wait()
	var items []Drama
	var failures []error
	more := false
	for _, result := range results {
		items = append(items, result.items...)
		more = more || len(result.items) >= huangguoAICatalogPageSize
		if result.err != nil {
			failures = append(failures, result.err)
		}
	}
	if len(items) == 0 && len(failures) > 0 {
		return nil, false, errors.Join(failures...)
	}
	return items, more, nil
}
