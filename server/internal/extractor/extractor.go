// Package extractor 移植自 Python 版 douyin-recorder 的 stream_extractor.py
// 核心逻辑：HTTP 抓取抖音直播页面 → 正则提取 FLV/M3U8 流地址 → 按画质排序 → 提取元数据
// 零第三方依赖，仅用 net/http + regexp
package extractor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 常见错误
var (
	ErrRoomNotFound = errors.New("直播间不存在或已结束 (404)。请检查 URL 是否正确")
	ErrForbidden    = errors.New("访问被拒绝 (403)。可能需要提供 Cookie 或使用代理")
	ErrNoStream     = errors.New("未找到可用的流地址。该直播间可能未开播或需要登录")
	ErrLiveEnded    = errors.New("该直播已结束")
	ErrBadURL       = errors.New("URL 格式不正确，请使用 https://live.douyin.com/ROOM_ID")
)

// 画质排序：数值越大画质越好
var qualityRank = map[string]int{
	"or4":           10, // 原画
	"origin":        10,
	"uhd5":          11, // 蓝光/超高清（抖音最新画质标识，码率最高）
	"uhd":           9,  // 超清
	"FULL_HD1":      9,
	"Stage0T000hd":  8,
	"hd":            7,
	"hd5":           6,
	"Stage0T000ld5": 6,
	"standard":      5, // 标准
	"md":            5,
	"ld5":           5, // 流畅+
	"sd":            4,
	"Stage0T000sd":  4,
	"Stage0T000sd5": 4,
	"sd5":           4, // 标清+
	"ld":            3,
	"Stage0T000ld":  3,
	"audio_only":    0,
}

// 画质中文标签
var qualityLabels = map[string]string{
	"or4":           "原画",
	"Stage0T000hd":  "超清(HD)",
	"hd":            "高清",
	"hd5":           "高清+",
	"Stage0T000ld5": "流畅+",
	"md":            "中清",
	"sd":            "标清",
	"Stage0T000sd":  "标清(SD)",
	"Stage0T000sd5": "标清+",
	"ld":            "流畅",
	"Stage0T000ld":  "流畅(LD)",
	"audio_only":    "仅音频",
}

// 浏览器 UA，避免被反爬拦截
var defaultHeaders = map[string]string{
	"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Referer":         "https://live.douyin.com/",
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
}

// 一个画质选项
type QualityOption struct {
	Label      string `json:"label"`
	QualityKey string `json:"quality_key"`
	URL        string `json:"url"`
	Format     string `json:"format"` // flv / m3u8
}

// 解析结果
type StreamInfo struct {
	StreamURL      string          `json:"stream_url"`
	StreamFormat   string          `json:"stream_format"` // flv / m3u8
	RoomID         string          `json:"room_id"`
	AnchorName     string          `json:"anchor_name"`
	Title          string          `json:"title"`
	IsLive         bool            `json:"is_live"`
	QualityOptions []QualityOption `json:"quality_options"`
}

// 提取器
type Extractor struct {
	CookiesPath string
	Proxy       string
	Timeout     time.Duration
}

func New() *Extractor {
	return &Extractor{Timeout: 30 * time.Second}
}

// Extract 从页面 HTML 提取流地址
func (e *Extractor) Extract(urlStr string) (*StreamInfo, error) {
	roomID := extractRoomID(urlStr)
	if roomID == "" {
		return nil, ErrBadURL
	}

	// 规范化抓取 URL：只访问 live.douyin.com 房间页，杜绝 SSRF（防止抓取任意主机/内网地址）
	canonicalURL := "https://live.douyin.com/" + roomID
	html, err := e.fetchPage(canonicalURL)
	if err != nil {
		return nil, err
	}

	// 规范化 HTML：抖音 SSR JSON 使用转义引号 \" 与 \u0026，先还原成常规形式再匹配
	clean := normalizeHTML(html)

	flvURLs := extractFLVURLs(clean)
	hlsURLs := extractHLSURLs(clean)

	if len(flvURLs) == 0 && len(hlsURLs) == 0 {
		// 页面提示直播结束或需要登录
		if strings.Contains(clean, "直播已结束") || strings.Contains(strings.ToLower(clean), "live ended") {
			return nil, ErrLiveEnded
		}
		if liveEnded(clean) {
			return nil, ErrLiveEnded
		}
		return nil, ErrNoStream
	}

	// 构建画质选项
	var options []QualityOption
	for qk, u := range flvURLs {
		options = append(options, QualityOption{
			Label:      labelOf(qk),
			QualityKey: qk,
			URL:        u,
			Format:     "flv",
		})
	}
	for qk, u := range hlsURLs {
		// 避免重复：FLV 优先
		if _, ok := flvURLs[qk]; ok {
			continue
		}
		options = append(options, QualityOption{
			Label:      labelOf(qk),
			QualityKey: qk,
			URL:        u,
			Format:     "m3u8",
		})
	}

	// 画质降序排序（原画优先），同画质 FLV 优先 —— 保证前端默认选中最佳画质
	// 注意：flvURLs/hlsURLs 是 map，遍历顺序随机，必须显式排序
	sort.Slice(options, func(i, j int) bool {
		ri, rj := qualityRank[options[i].QualityKey], qualityRank[options[j].QualityKey]
		if ri != rj {
			return ri > rj
		}
		if options[i].Format != options[j].Format {
			return options[i].Format == "flv"
		}
		return options[i].QualityKey < options[j].QualityKey
	})

	bestURL, bestFormat := selectBestURL(flvURLs, hlsURLs)
	meta := extractMetadata(clean)

	return &StreamInfo{
		StreamURL:      bestURL,
		StreamFormat:   bestFormat,
		RoomID:         roomID,
		AnchorName:     meta["anchor_name"],
		Title:          meta["title"],
		IsLive:         meta["is_live"] == "1" || len(flvURLs)+len(hlsURLs) > 0,
		QualityOptions: options,
	}, nil
}

// normalizeHTML 规范化抖音 SSR HTML：还原转义引号与 \u0026，便于正则匹配
func normalizeHTML(html string) string {
	s := strings.ReplaceAll(html, `\"`, `"`)
	s = strings.ReplaceAll(s, `\u0026`, "&")
	return s
}

// liveEnded 判定直播间是否已结束：抖音 SSR JSON 中 status=4 或出现结束标记
func liveEnded(clean string) bool {
	patterns := []string{
		`"status":4`, `"status": 4`,
		`"status_str":"4"`, `"status_str": "4"`,
		`"web_stream_url":null`, `"web_stream_url": null`,
		`"liveStatus":"not_living"`, `"liveStatus":"ended"`,
		`"is_live_end"`, `"is_living":false`, `"is_living": false`,
	}
	for _, p := range patterns {
		if strings.Contains(clean, p) {
			return true
		}
	}
	return false
}

// fetchPage 抓取抖音直播页面
func (e *Extractor) fetchPage(urlStr string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	if err != nil {
		return "", fmt.Errorf("构造请求失败: %w", err)
	}
	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}

	transport := &http.Transport{}
	// 可选代理
	if e.Proxy != "" {
		if proxyURL, perr := url.Parse(e.Proxy); perr == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	client := &http.Client{
		Timeout:   e.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("重定向次数过多")
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("网络请求失败: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 404:
		return "", ErrRoomNotFound
	case 403:
		return "", ErrForbidden
	case 200:
		// OK
	default:
		return "", fmt.Errorf("HTTP 错误 %d", resp.StatusCode)
	}

	// 限制响应体大小（10MB），防恶意大响应造成内存压力
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	return string(body), nil
}

// extractRoomID 从 URL 提取房间号
// 只接受 live.douyin.com 域名下的房间号（不再从任意路径提取数字，配合规范化抓取杜绝 SSRF）
func extractRoomID(rawURL string) string {
	re := regexp.MustCompile(`live\.douyin\.com/(\d+)`)
	if m := re.FindStringSubmatch(rawURL); m != nil {
		return m[1]
	}
	return ""
}

// extractFLVURLs 从 HTML 提取 FLV 流地址，返回 map[qualityKey]url
func extractFLVURLs(html string) map[string]string {
	urls := make(map[string]string)
	// HTML 中 & 被转义为 \u0026，先还原
	clean := strings.ReplaceAll(html, `\u0026`, "&")

	// 主 FLV 流
	re := regexp.MustCompile(`(https?://pull-[^"\s<>]+\.douyincdn\.com[^"\s<>]*\.flv[^"\s<>]*)`)
	for _, m := range re.FindAllStringSubmatch(clean, -1) {
		qk := qualityFromURL(m[1])
		if _, ok := urls[qk]; !ok {
			urls[qk] = m[1]
		}
	}

	// 仅音频流
	audioRe := regexp.MustCompile(`(https?://pull-flv[^"\s<>]*only_audio=1[^"\s<>]*\.flv[^"\s<>]*)`)
	for _, m := range audioRe.FindAllStringSubmatch(clean, -1) {
		if _, ok := urls["audio_only"]; !ok {
			urls["audio_only"] = m[1]
		}
	}
	return urls
}

// extractHLSURLs 从 HTML 提取 M3U8 流地址
func extractHLSURLs(html string) map[string]string {
	urls := make(map[string]string)
	clean := strings.ReplaceAll(html, `\u0026`, "&")
	re := regexp.MustCompile(`(https?://pull-[^"\s<>]+\.douyincdn\.com[^"\s<>]*\.m3u8[^"\s<>]*)`)
	for _, m := range re.FindAllStringSubmatch(clean, -1) {
		qk := qualityFromURL(m[1])
		if _, ok := urls[qk]; !ok {
			urls[qk] = m[1]
		}
	}
	return urls
}

// qualityFromURL 从 URL 提取画质 key
func qualityFromURL(u string) string {
	// stream-<数字>_<后缀>.flv/m3u8
	re := regexp.MustCompile(`stream-\d+_([a-zA-Z0-9]+)\.(?:flv|m3u8)`)
	if m := re.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	// stream-<数字>.flv/m3u8 → standard
	re2 := regexp.MustCompile(`stream-(\d+)\.(?:flv|m3u8)`)
	if re2.MatchString(u) {
		return "standard"
	}
	return "unknown"
}

// selectBestURL 选择最佳流：优先 FLV，其次高画质
func selectBestURL(flv, hls map[string]string) (url, format string) {
	bestURL, bestFormat := "", "flv"
	bestRank := -1

	type candidate struct {
		m   map[string]string
		fmt string
	}
	cands := []candidate{{flv, "flv"}, {hls, "m3u8"}}
	for _, c := range cands {
		for qk, u := range c.m {
			rank := qualityRank[qk]
			if rank > bestRank || (rank == bestRank && c.fmt == "flv") {
				bestRank, bestURL, bestFormat = rank, u, c.fmt
			}
		}
	}
	return bestURL, bestFormat
}

// extractMetadata 从页面 HTML 提取主播名/标题/直播状态
func extractMetadata(html string) map[string]string {
	meta := map[string]string{"anchor_name": "", "title": "", "is_live": ""}

	// 1. <title> 标签
	titleRe := regexp.MustCompile(`<title>(.*?)</title>`)
	if m := titleRe.FindStringSubmatch(html); m != nil {
		pageTitle := m[1]
		if strings.Contains(pageTitle, "抖音直播") {
			main := strings.Trim(strings.Replace(pageTitle, "抖音直播", "", -1), " -–—")
			if strings.Contains(main, "的直播") {
				meta["anchor_name"] = strings.Trim(strings.Replace(main, "的直播", "", -1), " ")
			} else {
				meta["title"] = main
			}
		}
	}

	// 2. 内嵌 JSON 的 nickname（跳过 undefined 等无效值，取第一个有效主播名）
	nickRe := regexp.MustCompile(`"nickname"\s*:\s*"([^"]*)"`)
	for _, m := range nickRe.FindAllStringSubmatch(html, -1) {
		name := strings.TrimSpace(m[1])
		if name == "" || name == "undefined" || name == "$undefined" {
			continue
		}
		meta["anchor_name"] = name
		break
	}

	// 3. 内嵌 JSON 的 title（同样跳过无效值）
	rtRe := regexp.MustCompile(`"title"\s*:\s*"([^"]*)"`)
	for _, m := range rtRe.FindAllStringSubmatch(html, -1) {
		t := strings.TrimSpace(m[1])
		if t == "" || t == "undefined" || t == "$undefined" || t == meta["anchor_name"] {
			continue
		}
		if meta["title"] == "" {
			meta["title"] = t
			break
		}
	}

	// 4. 直播状态：页面存在流地址即认为在直播
	hasStreamRe := regexp.MustCompile(`https?://pull-[^"\s<>]+\.(?:flv|m3u8)`)
	if hasStreamRe.MatchString(html) {
		meta["is_live"] = "1"
	}

	// 5. 兜底：出现 nickname 也认为页面正常加载了直播间
	if meta["is_live"] == "" && strings.Contains(html, `"nickname"`) {
		meta["is_live"] = "1"
	}

	return meta
}

func labelOf(qk string) string {
	if l, ok := qualityLabels[qk]; ok {
		return l
	}
	return qk
}
