package core

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

const nativeSuggestionLimit = 10

func (engine *nativeEngine) suggestions(ctx context.Context, source, query string) ([]string, error) {
	query = strings.TrimSpace(query)
	if query == "" || utf8.RuneCountInString(query) > 40 {
		return []string{}, nil
	}
	source = canonicalProviderSource(source)
	local := engine.localSuggestions(source, query)
	if len(local) >= 2 {
		return local, nil
	}
	if !nativeSourceAvailable(source) {
		return local, nil
	}

	// 红果自带的联想接口标题更准，源可用时优先。
	if source == sourceHongguo {
		items, err := engine.downloader.hongguoSearchSuggestions(ctx, query)
		if err == nil && len(items) > 0 {
			names := make([]string, 0, len(items))
			for _, item := range items {
				names = append(names, item.Name)
			}
			return nativeUniqueSuggestions(append(local, names...), query), nil
		}
	}

	// 本地没命中时用该站源真实搜一次，把返回的标题当联想词。
	// 只有这个站源还没有任何缓存时才会走到这里，代价等同于用户自己搜一次。
	result, err := engine.nativeCatalogRaw(ctx, nativeInput{
		Action: "catalog", Source: source, Query: query, Page: 1,
	})
	if err != nil {
		return local, nil
	}
	names := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		names = append(names, item.Title)
	}
	return nativeUniqueSuggestions(append(local, names...), query), nil
}

// localSuggestions 在已缓存的目录里按标题匹配，前缀命中排在包含命中前面。
func (engine *nativeEngine) localSuggestions(source, query string) []string {
	if engine == nil || query == "" {
		return nil
	}
	lower := strings.ToLower(query)
	prefix := make([]string, 0, nativeSuggestionLimit)
	contains := make([]string, 0, nativeSuggestionLimit)
	engine.mu.Lock()
	groups := make([][]nativeDrama, 0, len(engine.catalogs))
	for key, items := range engine.catalogs {
		// 缓存键是站源，或「站源/分类」。
		if source != "" && key != source && !strings.HasPrefix(key, source+"/") {
			continue
		}
		groups = append(groups, items)
	}
	engine.mu.Unlock()
	for _, items := range groups {
		for _, item := range items {
			title := strings.TrimSpace(item.Title)
			if title == "" || utf8.RuneCountInString(title) > 200 {
				continue
			}
			position := strings.Index(strings.ToLower(title), lower)
			if position < 0 {
				continue
			}
			if position == 0 {
				prefix = append(prefix, title)
			} else {
				contains = append(contains, title)
			}
		}
	}
	return nativeUniqueSuggestions(append(prefix, contains...), query)
}

// nativeUniqueSuggestions 去重、按「前缀命中优先 + 短的排前面」排序并截断。
func nativeUniqueSuggestions(names []string, query string) []string {
	lower := strings.ToLower(strings.TrimSpace(query))
	seen := make(map[string]bool, len(names))
	prefix := make([]string, 0, nativeSuggestionLimit)
	contains := make([]string, 0, nativeSuggestionLimit)
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] || utf8.RuneCountInString(name) > 200 {
			continue
		}
		seen[name] = true
		if lower != "" && strings.HasPrefix(strings.ToLower(name), lower) {
			prefix = append(prefix, name)
		} else {
			contains = append(contains, name)
		}
	}
	sort.SliceStable(prefix, func(i, j int) bool {
		left, right := utf8.RuneCountInString(prefix[i]), utf8.RuneCountInString(prefix[j])
		if left != right {
			return left < right
		}
		return prefix[i] < prefix[j]
	})
	result := append(prefix, contains...)
	if len(result) > nativeSuggestionLimit {
		result = result[:nativeSuggestionLimit]
	}
	return result
}

func nativeParseSuggestions(body []byte) ([]string, error) {
	var result map[string]json.RawMessage
	if json.Unmarshal(body, &result) != nil {
		return nil, errors.New("搜索联想暂不可用")
	}
	list := result["suggest_list"]
	if len(list) == 0 {
		var data map[string]json.RawMessage
		if json.Unmarshal(result["data"], &data) == nil {
			list = data["suggest_list"]
		}
	}
	if len(list) == 0 || string(list) == "null" {
		return []string{}, nil
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(list, &entries) != nil {
		return nil, errors.New("搜索联想暂不可用")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return nativeUniqueSuggestions(names, ""), nil
}
