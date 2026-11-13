package core

import (
	"net/url"
	"strings"
)

// providerDramaWebURL 给出「该剧在站源网页上的详情页地址」，供 App 详情页的
// 「用浏览器打开」按钮使用。
//
// 只覆盖有真实网页详情页的站源；纯 App 接口的站源（红果、黄豆、DSD、琥珀、
// 橘果、禁果、笔盒等）没有稳定可读的网页地址，返回空串，Dart 侧据此隐藏按钮。
//
// 站点主机优先用最近一次请求成功落到的主机：黄果这类站会轮换镜像域名，
// 用真在用的那个比写死官网域名更容易打开。
func (d *Downloader) providerDramaWebURL(source, sourceID string) string {
	source = canonicalProviderSource(source)
	sourceID = strings.Trim(strings.TrimSpace(sourceID), "/")
	if source == "" || sourceID == "" {
		return ""
	}
	var route string
	switch source {
	case sourceHuangguoAI:
		route = "/video/" + url.PathEscape(sourceID) + "/"
	case sourceHuangguoVideo:
		if strings.HasPrefix(sourceID, "series/") || strings.HasPrefix(sourceID, "video/") {
			route = "/" + sourceID
		} else {
			route = "/series/" + url.PathEscape(sourceID)
		}
	case sourceXduanju:
		route = "/ju/" + url.PathEscape(sourceID)
	case sourceHuangtao:
		route = "/video/" + url.PathEscape(sourceID) + "/"
	case sourceYesheai:
		route = "/detail/" + url.PathEscape(sourceID) + ".html"
	case sourceCrdj:
		// /duanju/{id}/ 会 301 到 /{真实类型}/{id}-{slug}/，不必记住类型与 slug。
		route = "/duanju/" + url.PathEscape(sourceID) + "/"
	case sourceHeitao:
		route = "/index.php/vod/detail/id/" + url.PathEscape(sourceID) + ".html"
	case sourceGuipian:
		route = "/nv/" + url.PathEscape(sourceID) + ".html"
	case sourceHanxiaoquan:
		route = "/hanxiaoquan/" + url.PathEscape(sourceID) + ".html"
	case sourceYeguo:
		route = "/drama/video/" + url.PathEscape(sourceID) + "/"
	default:
		return ""
	}
	site := d.providerBaseURL(source)
	d.providerMu.Lock()
	if host := d.providerHosts[source]; host != "" {
		site = host
	}
	d.providerMu.Unlock()
	site = strings.TrimRight(strings.TrimSpace(site), "/")
	if site == "" {
		return ""
	}
	return site + route
}
