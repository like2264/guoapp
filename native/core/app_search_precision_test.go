package core

import "testing"

func TestNativePreciseSearchRanksAndDrops(t *testing.T) {
	items := []LibraryDrama{
		{ID: "a:1", Title: "都市爱情故事", Description: "总裁也会心动"},
		{ID: "a:2", Title: "霸道总裁的替身新娘", Description: "无"},
		{ID: "a:3", Title: "总裁", Description: "无"},
		{ID: "a:4", Title: "总裁在上", Description: "无"},
		{ID: "a:5", Title: "完全无关的农村题材", Description: "无", Tags: []string{"总裁文"}},
		{ID: "a:6", Title: "另一个无关剧", Description: "无"},
	}
	got := NativePreciseSearch(items, "总裁")
	// 「适中」策略：仅简介命中的保留在末尾，完全不命中的（a:6）剔除。
	if len(got) != 5 {
		t.Fatalf("expected 5 results, got %d: %+v", len(got), got)
	}
	want := []string{"a:3", "a:4", "a:2", "a:5", "a:1"}
	for index, id := range want {
		if got[index].ID != id {
			t.Fatalf("position %d: want %s, got %s (all=%v)", index, id, got[index].ID, ids(got))
		}
	}
}

func TestNativePreciseSearchKeepsEverythingWhenNothingMatches(t *testing.T) {
	items := []LibraryDrama{
		{ID: "a:1", Title: "甲剧"},
		{ID: "a:2", Title: "乙剧"},
	}
	got := NativePreciseSearch(items, "完全不存在")
	if len(got) != len(items) {
		t.Fatalf("expected fallback to original list, got %d", len(got))
	}
}

func TestNativePreciseSearchMultiWord(t *testing.T) {
	items := []LibraryDrama{
		{ID: "a:1", Title: "总裁的契约新娘"},
		{ID: "a:2", Title: "总裁归来"},
		{ID: "a:3", Title: "无关剧"},
	}
	got := NativePreciseSearch(items, "总裁 新娘")
	if len(got) != 1 || got[0].ID != "a:1" {
		t.Fatalf("expected only a:1, got %v", ids(got))
	}
}

func ids(items []LibraryDrama) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}
