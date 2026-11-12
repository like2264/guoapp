package core

import (
	"context"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// 只在 HGV_PROBE=1 时运行：确认黄果视频的站内搜索入口是哪个路径。
// 只打印字节数、解析出的卡片数和错误摘要，不打印剧名、分类名和域名。
func TestHuangguoVideoSearchProbe(t *testing.T) {
	if os.Getenv("HGV_PROBE") != "1" {
		t.Skip("set HGV_PROBE=1 to run the huangguo video search probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	base := d.providerBaseURL(sourceHuangguoVideo)
	keyword := "总裁"
	escaped := url.QueryEscape(keyword)
	paths := []string{
		"/videos?page=1",
		"/search?q=" + escaped,
		"/search?keyword=" + escaped,
		"/search?wd=" + escaped,
		"/search?word=" + escaped,
		"/search/" + url.PathEscape(keyword),
		"/videos?q=" + escaped,
		"/videos?keyword=" + escaped,
		"/videos?search=" + escaped,
		"/api/search?q=" + escaped,
		"/api/videos?q=" + escaped,
		"/api/video/search?q=" + escaped,
	}
	for index, path := range paths {
		body, err := d.fetchProviderText(ctx, base+path, base+"/")
		cards := 0
		if err == nil {
			cards = len(parseHuangguoVideoCards(body, base+path))
		}
		links := 0
		if err == nil {
			links = len(reHuangguoLink.FindAllStringSubmatch(body, -1))
		}
		t.Logf("[hgv] cand[%02d] bytes=%d cards=%d links=%d err=%q", index, len(body), cards, links, diagErr(err))
		time.Sleep(400 * time.Millisecond)
	}
}

var reProbeHref = regexp.MustCompile(`(?is)href=["']([^"']{2,200})["']`)
var reProbeClass = regexp.MustCompile(`(?is)class=["']([^"']{1,160})["']`)
var reProbeAlnumRun = regexp.MustCompile(`[0-9A-Za-z_-]{3,}`)

// 第二段：搜索页的结构。只打印 href 形状（ID 已打码）、class 词频和标签计数。
func TestHuangguoVideoSearchStructure(t *testing.T) {
	if os.Getenv("HGV_PROBE") != "1" {
		t.Skip("set HGV_PROBE=1 to run the huangguo video search probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	base := d.providerBaseURL(sourceHuangguoVideo)
	pageURL := base + "/search?q=" + url.QueryEscape("总裁")
	body, err := d.fetchProviderText(ctx, pageURL, base+"/")
	if err != nil {
		t.Fatalf("search page: %v", diagErr(err))
	}
	t.Logf("[hgv2] search bytes=%d article=%d videoCard=%d seriesLink=%d",
		len(body),
		strings.Count(strings.ToLower(body), "<article"),
		strings.Count(strings.ToLower(body), "video-card"),
		len(reHuangguoLink.FindAllStringSubmatch(body, -1)))

	shapes := map[string]int{}
	for _, match := range reProbeHref.FindAllStringSubmatch(body, -1) {
		href := match[1]
		if index := strings.IndexAny(href, "?#"); index >= 0 {
			href = href[:index]
		}
		shapes[reProbeAlnumRun.ReplaceAllString(href, "X")]++
	}
	type shapeCount struct {
		shape string
		count int
	}
	var ordered []shapeCount
	for shape, count := range shapes {
		ordered = append(ordered, shapeCount{shape, count})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].shape < ordered[j].shape
	})
	for index, entry := range ordered {
		if index >= 12 {
			break
		}
		t.Logf("[hgv2] href[%02d] count=%d shape=%s", index, entry.count, entry.shape)
	}

	classes := map[string]int{}
	for _, match := range reProbeClass.FindAllStringSubmatch(body, -1) {
		for _, token := range strings.Fields(match[1]) {
			if len(token) > 1 {
				classes[token]++
			}
		}
	}
	var classOrder []shapeCount
	for name, count := range classes {
		classOrder = append(classOrder, shapeCount{name, count})
	}
	sort.Slice(classOrder, func(i, j int) bool {
		if classOrder[i].count != classOrder[j].count {
			return classOrder[i].count > classOrder[j].count
		}
		return classOrder[i].shape < classOrder[j].shape
	})
	for index, entry := range classOrder {
		if index >= 15 {
			break
		}
		t.Logf("[hgv2] class[%02d] count=%d name=%s", index, entry.count, entry.shape)
	}

	// 用站内链接自身当卡片块解析一次，看标题能不能取到（只打印条数和标题长度）。
	blocks := reHuangguoLink.FindAllString(body, -1)
	titles := 0
	for _, block := range blocks {
		title := firstNonEmpty(extractClosestAttr(block, 0, len(block), "title", "alt", "aria-label"), cleanText(block))
		if len([]rune(title)) >= 2 {
			titles++
		}
	}
	t.Logf("[hgv2] linkBlocks=%d withTitle=%d", len(blocks), titles)
}

// 第三段：搜索页能不能翻页、能不能放大单页条数。只看条数和「首条是否和第一页相同」。
func TestHuangguoVideoSearchPaging(t *testing.T) {
	if os.Getenv("HGV_PROBE") != "1" {
		t.Skip("set HGV_PROBE=1 to run the huangguo video search probe")
	}
	engine, err := newNativeEngine(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	base := d.providerBaseURL(sourceHuangguoVideo)
	escaped := url.QueryEscape("总裁")
	firstOfPageOne := ""
	suffixes := []string{"", "&page=2", "&p=2", "&pageNum=2", "&page_no=2", "&size=50", "&limit=50", "&size=50&page=2"}
	for index, suffix := range suffixes {
		address := base + "/search?q=" + escaped + suffix
		body, fetchErr := d.fetchProviderText(ctx, address, base+"/")
		items := 0
		first := ""
		if fetchErr == nil {
			parsed := parseHuangguoVideoCards(body, address)
			items = len(parsed)
			if items > 0 {
				first = parsed[0].SourceID
			}
		}
		if index == 0 {
			firstOfPageOne = first
		}
		t.Logf("[hgv3] suffix=%q count=%d sameAsP1=%v bytes=%d err=%q",
			suffix, items, first != "" && first == firstOfPageOne, len(body), diagErr(fetchErr))
		time.Sleep(500 * time.Millisecond)
	}
}
