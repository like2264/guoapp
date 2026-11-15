package core

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type suggestionTransport func(*http.Request) (*http.Response, error)

func (transport suggestionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestNativeSuggestionsOfficialContract(t *testing.T) {
	engine := &nativeEngine{downloader: &Downloader{
		limiter: newRequestLimiter(1, 0),
		client: &http.Client{Timeout: time.Second, Transport: suggestionTransport(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "hongguoduanju.com" || request.URL.Path != "/incent_resource/suggestion" ||
				request.URL.Query().Get("query") != "永 & 世" || request.URL.Query().Get("app_id") != "8662" ||
				request.URL.Query().Get("count") != "10" || request.Header.Get("Cookie") != "" {
				t.Fatalf("unexpected suggestion request: %s", request.URL)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(`{"data":{"suggest_list":[{"name":"永世长青"},{"name":"永冬之下"},{"name":"永世长青"},{"name":" "}]}}`))}, nil
		})},
	}}
	items, err := engine.suggestions(context.Background(), sourceHongguo, " 永 & 世 ")
	if err != nil || len(items) != 2 || items[0] != "永世长青" || items[1] != "永冬之下" {
		t.Fatalf("unexpected suggestions: %v %v", items, err)
	}
	if items, err := engine.suggestions(context.Background(), sourceHongguo, strings.Repeat("永", 101)); err != nil || len(items) != 0 {
		t.Fatalf("oversized query should be ignored: %v %v", items, err)
	}
}

// 已缓存的剧库要能在不打网络的情况下给出联想词，前缀命中排在包含命中前面。
func TestNativeSuggestionsLocalLibrary(t *testing.T) {
	engine := &nativeEngine{
		catalogs: map[string][]nativeDrama{
			sourceYouzidj: {
				{Title: "狐妖小红娘"},
				{Title: "狐妖小红娘黄风岭篇"},
				{Title: "狐妖x仆s"},
				{Title: "完全不同"},
				{Title: ""},
			},
			sourceYouzidj + "/face-swap": {{Title: "狐妖换脸"}},
			sourceHuangju:                {{Title: "狐妖皇后"}},
		},
	}
	items := engine.localSuggestions(sourceYouzidj, "狐妖")
	if len(items) != 4 {
		t.Fatalf("local suggestions = %v, want 4 from youzidj only", items)
	}
	if items[0] != "狐妖换脸" || items[1] != "狐妖x仆s" || items[2] != "狐妖小红娘" || items[3] != "狐妖小红娘黄风岭篇" {
		t.Fatalf("prefix ordering broken: %v", items)
	}
}

func TestNativeUniqueSuggestions(t *testing.T) {
	got := nativeUniqueSuggestions([]string{" 重生 ", "重生", "", "爱上重生", "重生之路"}, "重生")
	if len(got) != 3 || got[0] != "重生" || got[1] != "重生之路" || got[2] != "爱上重生" {
		t.Fatalf("unique suggestions = %v", got)
	}
	long := make([]string, 0, 20)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		long = append(long, name)
	}
	if got := nativeUniqueSuggestions(long, ""); len(got) != nativeSuggestionLimit {
		t.Fatalf("limit not applied: %d", len(got))
	}
}

func TestNativeSuggestionsAcceptRootAndEmptyResults(t *testing.T) {
	for _, body := range []string{`{"suggest_list":[]}`, `{"data":{"suggest_list":null}}`} {
		items, err := nativeParseSuggestions([]byte(body))
		if err != nil || len(items) != 0 {
			t.Fatalf("empty results rejected: %v", err)
		}
	}
	items, err := nativeParseSuggestions([]byte(`{"suggest_list":[{"name":"重生"}]}`))
	if err != nil || len(items) != 1 || items[0] != "重生" {
		t.Fatalf("root result: %v %v", items, err)
	}
	if _, err := nativeParseSuggestions([]byte("<html>error</html>")); err == nil {
		t.Fatal("HTML accepted")
	}
}
