package core

import (
	"sort"
	"strings"
)

// 精准搜索：站源的搜索接口是「标题/简介/标签里出现过就算命中」的宽匹配，
// 搜「总裁」会带出大量只在简介里提一句、甚至只是同字不同词的剧。
//
// 这里在搜索出口统一做一次相关度分级：
//
//	标题完全相同 > 标题以关键词开头 > 标题包含关键词 >
//	标题拆词全命中 > 标签/分类命中 > 仅简介命中 > 完全不命中（剔除）
//
// 排序稳定，同分保持站源原本的顺序；如果整页都被判成不相关，则原样返回，
// 避免出现「搜得到东西却一条都不给」这种更糟的体验。
type searchRelevance int

const (
	searchRelevanceNone searchRelevance = iota
	searchRelevanceIntro
	searchRelevanceMeta
	searchRelevanceFuzzy
	searchRelevanceContains
	searchRelevancePrefix
	searchRelevanceExact
)

type scoredDrama struct {
	drama LibraryDrama
	score searchRelevance
	order int
}

// NativePreciseSearch 按相关度重排搜索结果，并剔除完全不相关的条目。
func NativePreciseSearch(items []LibraryDrama, query string) []LibraryDrama {
	keyword := NativeSearchText(query)
	if keyword == "" || len(items) < 2 {
		return items
	}
	words := nativeSearchWords(query)
	scored := make([]scoredDrama, 0, len(items))
	for index, item := range items {
		score := nativeDramaRelevance(item, keyword, words)
		if score == searchRelevanceNone {
			continue
		}
		scored = append(scored, scoredDrama{drama: item, score: score, order: index})
	}
	if len(scored) == 0 {
		return items
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].order < scored[j].order
	})
	out := make([]LibraryDrama, 0, len(scored))
	for _, entry := range scored {
		out = append(out, entry.drama)
	}
	return out
}

func nativeSearchWords(query string) []string {
	raw := strings.FieldsFunc(query, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '　'
	})
	words := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, word := range raw {
		word = NativeSearchText(word)
		if word == "" || seen[word] {
			continue
		}
		seen[word] = true
		words = append(words, word)
	}
	return words
}

func nativeDramaRelevance(item LibraryDrama, keyword string, words []string) searchRelevance {
	title := NativeSearchText(item.Title)
	if title != "" {
		switch {
		case title == keyword:
			return searchRelevanceExact
		case strings.HasPrefix(title, keyword):
			return searchRelevancePrefix
		case strings.Contains(title, keyword):
			return searchRelevanceContains
		}
		if len(words) > 1 && nativeContainsAll(title, words) {
			return searchRelevanceFuzzy
		}
	}
	for _, value := range append([]string{item.Category}, item.Tags...) {
		if value = NativeSearchText(value); value != "" && strings.Contains(value, keyword) {
			return searchRelevanceMeta
		}
	}
	if strings.Contains(NativeSearchText(item.Description), keyword) {
		return searchRelevanceIntro
	}
	return searchRelevanceNone
}

func nativeContainsAll(text string, words []string) bool {
	for _, word := range words {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}
