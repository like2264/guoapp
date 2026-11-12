package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// 只在 LEGACY_DIAG=1 时运行，用于线上诊断「黄果旧版」列表是否被站点截断。
// 输出只包含数量、长度、字段名，不打印任何剧名或分类名。
func TestLegacyCatalogDiagnostic(t *testing.T) {
	if os.Getenv("LEGACY_DIAG") != "1" {
		t.Skip("set LEGACY_DIAG=1 to run the live diagnostic")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	var tabs legacyTabList
	if err := d.fetchAPI(ctx, "/api/app/playlet-tab/all", nil, &tabs); err != nil {
		t.Fatalf("tab list failed: %s", diagErr(err))
	}
	fmt.Printf("[diag] api_override=%v\n", d.cfg.APIBase != "")
	fmt.Printf("[diag] pageSize_cfg=%d tabs=%d\n", d.cfg.PageSize, len(tabs))
	valid := make([]Tab, 0, len(tabs))
	for i, tab := range tabs {
		fmt.Printf("[diag] tab[%d] id=%s nameLen=%d\n", i, diagID(tab.ID), len([]rune(tab.Name)))
		if tab.ID != "" {
			valid = append(valid, tab)
		}
	}

	// 原始响应里有哪些字段：看站点有没有给 total / pages 之类，决定能不能精确翻页。
	if len(valid) > 0 {
		access, err := d.legacyCredentials(ctx)
		if err != nil {
			fmt.Printf("[diag] credentials failed: %s\n", diagErr(err))
		} else {
			path := "/api/app/playlet/home/tab/" + valid[0].ID
			raw, err := d.legacyRequest(ctx, http.MethodGet, path, map[string]string{
				"pageNumber": "1", "pageSize": "30", "tabSortType": "1",
			}, access)
			if err != nil {
				fmt.Printf("[diag] raw request failed: %s\n", diagErr(err))
			} else {
				var payload map[string]any
				if json.Unmarshal(raw, &payload) == nil {
					keys := make([]string, 0, len(payload))
					for key := range payload {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					fmt.Printf("[diag] payload keys: %s\n", strings.Join(keys, ","))
					for _, key := range keys {
						switch value := payload[key].(type) {
						case []any:
							fmt.Printf("[diag]   %s: array len=%d\n", key, len(value))
						case map[string]any:
							inner := make([]string, 0, len(value))
							for k := range value {
								inner = append(inner, k)
							}
							sort.Strings(inner)
							fmt.Printf("[diag]   %s: object keys=[%s]\n", key, strings.Join(inner, ","))
						case float64, string, bool, nil:
							fmt.Printf("[diag]   %s: scalar type=%T\n", key, value)
						}
					}
				}
			}
		}
	}

	// 每个分类第 1 页返回多少条：决定「全部」首屏到底有多少部。
	union := map[string]bool{}
	keyword := ""
	for i, tab := range valid {
		items, more, err := d.fetchLegacyCatalogCategoryPage(ctx, 1, tab.ID)
		if err != nil {
			fmt.Printf("[diag] tab[%d] p1 failed: %s\n", i, diagErr(err))
			continue
		}
		for _, drama := range items {
			union[drama.ID] = true
		}
		if keyword == "" && len(items) > 0 {
			keyword = strings.TrimSpace(items[0].Title)
		}
		fmt.Printf("[diag] tab[%d] p1 count=%d hasMore=%v\n", i, len(items), more)
	}
	fmt.Printf("[diag] union_page1=%d\n", len(union))

	// 关键词接口：拿剧库里第一条的真实标题当关键词，只打印条数，不打印标题。
	if keyword != "" {
		items, more, err := d.searchLegacyCatalog(ctx, 1, keyword)
		fmt.Printf("[diag] keyword_search count=%d hasMore=%v err=%q\n", len(items), more, diagErr(err))
		if len(items) > 0 {
			items2, _, err2 := d.searchLegacyCatalog(ctx, 2, keyword)
			fmt.Printf("[diag] keyword_search p2 count=%d err=%q\n", len(items2), diagErr(err2))
		}
	} else {
		fmt.Printf("[diag] keyword_search skipped: catalog empty\n")
	}

	// 单个分类连续翻 8 页：看站点是否真的分页、每页多少条、有没有重复。
	if len(valid) > 0 {
		seen := map[string]bool{}
		firstIDs := []string{}
		for page := 1; page <= 8; page++ {
			items, more, err := d.fetchLegacyCatalogCategoryPage(ctx, page, valid[0].ID)
			if err != nil {
				fmt.Printf("[diag] tab[0] p%d failed: %s\n", page, diagErr(err))
				break
			}
			fresh := 0
			first := ""
			for _, drama := range items {
				if first == "" {
					first = drama.SourceID
				}
				if !seen[drama.ID] {
					seen[drama.ID] = true
					fresh++
				}
			}
			same := page > 1 && first != "" && len(firstIDs) > 0 && firstIDs[len(firstIDs)-1] == first
			firstIDs = append(firstIDs, first)
			fmt.Printf("[diag] tab[0] p%d count=%d fresh=%d sameFirstAsPrev=%v hasMore=%v\n", page, len(items), fresh, same, more)
			if len(items) == 0 {
				break
			}
		}
		fmt.Printf("[diag] tab[0] unique_8pages=%d\n", len(seen))
	}
}

func diagID(id string) string {
	if id == "" {
		return "empty"
	}
	digits := true
	for _, r := range id {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	return fmt.Sprintf("len=%d digits=%v", len(id), digits)
}

// diagErr 把错误里的非 ASCII 字符换成星号，避免把站点的中文提示带进输出。
func diagErr(err error) string {
	if err == nil {
		return ""
	}
	var out []rune
	for _, r := range err.Error() {
		if r > 127 {
			r = '*'
		}
		out = append(out, r)
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return string(out)
}
