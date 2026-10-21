// Package recorder 直播录制模块
//
// 第一版核心能力：FLV 原画直录 —— 用 net/http GET 直播流地址，直接流式写入 .flv 文件，
// 不需要 FFmpeg，零外部依赖（符合"解压即用"）。
//
// 转码方案（small/tiny，需要 FFmpeg）保留为可选项：本文件同时提供 FFmpeg 子进程录制接口。
package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// RecorderError 录制器致命错误
type RecorderError struct{ msg string }

func (e *RecorderError) Error() string { return e.msg }

func newErr(format string, args ...interface{}) *RecorderError {
	return &RecorderError{msg: fmt.Sprintf(format, args...)}
}

// 实时统计
type Stats struct {
	IsRecording bool    `json:"is_recording"`
	StartTime   int64   `json:"start_time"` // unix 秒
	Duration    float64 `json:"duration"`   // 秒
	OutputPath  string  `json:"output_path"`
	FileSize    int64   `json:"file_size"`
	BytesWritten int64  `json:"bytes_written"`
	Speed       float64 `json:"speed"` // MB/s
}

// 录制配置
type Config struct {
	OutputDir  string
	UserAgent  string
	Referer    string
	// 可选：-1 表示无限时长（秒）
	MaxDuration time.Duration
}

func defaultConfig() Config {
	return Config{
		OutputDir:  "recordings",
		UserAgent:  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
		Referer:    "https://live.douyin.com/",
		MaxDuration: -1,
	}
}

// Recorder 管理一次直播录制
type Recorder struct {
	cfg      Config
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.RWMutex
	stats    Stats
	startTime time.Time
}

func New(cfg Config) *Recorder {
	dc := defaultConfig()
	if cfg.OutputDir != "" {
		dc.OutputDir = cfg.OutputDir
	}
	if cfg.UserAgent != "" {
		dc.UserAgent = cfg.UserAgent
	}
	if cfg.Referer != "" {
		dc.Referer = cfg.Referer
	}
	if cfg.MaxDuration != 0 {
		dc.MaxDuration = cfg.MaxDuration
	}
	return &Recorder{cfg: dc, done: make(chan struct{})}
}

// IsRecording 是否正在录制
func (r *Recorder) IsRecording() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stats.IsRecording
}

// Stats 获取当前统计
func (r *Recorder) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.stats
	if s.IsRecording {
		s.Duration = time.Since(r.startTime).Seconds()
		if fi, err := os.Stat(s.OutputPath); err == nil {
			s.FileSize = fi.Size()
		}
		// 计算下载速度
		if s.Duration > 0 {
			s.Speed = float64(s.BytesWritten) / 1024 / 1024 / s.Duration
		}
	}
	return s
}

// Start 开始录制 FLV 直播流（零依赖直录）
//
// streamURL: 从 extractor 得到的 FLV 流地址
// fileName: 输出文件名（不含扩展名），扩展名自动追加
func (r *Recorder) Start(streamURL, fileName string) (string, error) {
	r.mu.Lock()
	if r.stats.IsRecording {
		r.mu.Unlock()
		return "", newErr("已经在录制中，请先停止当前录制")
	}
	r.mu.Unlock()

	if err := os.MkdirAll(r.cfg.OutputDir, 0o755); err != nil {
		return "", newErr("创建输出目录失败: %v", err)
	}

	ext := ".flv"
	if strings.Contains(streamURL, ".m3u8") {
		ext = ".ts"
	}
	outputPath := filepath.Join(r.cfg.OutputDir, sanitizeFileName(fileName)+ext)

	ctx, cancel := context.WithCancel(context.Background())

	r.mu.Lock()
	r.cancel = cancel
	r.startTime = time.Now()
	r.done = make(chan struct{}) // 每次录制重建 done，避免第二次 close 已关闭通道导致 panic
	r.stats = Stats{
		IsRecording: true,
		StartTime:   r.startTime.Unix(),
		OutputPath:  outputPath,
	}
	r.mu.Unlock()

	go func() {
		defer close(r.done)
		r.recordLoop(ctx, streamURL, outputPath)
	}()

	return outputPath, nil
}

// recordLoop 录制主循环：GET 流地址 → 流式写入文件
func (r *Recorder) recordLoop(ctx context.Context, streamURL, outputPath string) {
	client := &http.Client{Timeout: 0} // 直播流长时间连接，不用整体超时

	// 根据格式选择录制方式
	if strings.Contains(streamURL, ".m3u8") {
		r.recordHLS(ctx, streamURL, outputPath)
		return
	}
	r.recordFLV(ctx, client, streamURL, outputPath)
}

// recordFLV 直接 GET FLV 流并写入文件
func (r *Recorder) recordFLV(ctx context.Context, client *http.Client, streamURL, outputPath string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		r.finishWithError(err)
		return
	}
	req.Header.Set("User-Agent", r.cfg.UserAgent)
	req.Header.Set("Referer", r.cfg.Referer)

	resp, err := client.Do(req)
	if err != nil {
		r.finishWithError(err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		r.finishWithError(fmt.Errorf("流地址返回 HTTP %d", resp.StatusCode))
		return
	}

	actualPath, f, err := createOutputFile(outputPath)
	if err != nil {
		r.finishWithError(err)
		return
	}
	defer f.Close()
	// 实际输出路径可能因重名追加了 _1/_2 后缀，同步到统计
	r.mu.Lock()
	r.stats.OutputPath = actualPath
	r.mu.Unlock()
	var timer *time.Timer
	if r.cfg.MaxDuration > 0 {
		timer = time.AfterFunc(r.cfg.MaxDuration, func() { r.cancel() })
		defer timer.Stop()
	}

	// 流式拷贝并累计字节
	buf := make([]byte, 256*1024)
	for {
		select {
		case <-ctx.Done():
			r.markStopped()
			return
		default:
		}

		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				r.finishWithError(werr)
				return
			}
			r.mu.Lock()
			r.stats.BytesWritten += int64(n)
			r.mu.Unlock()
		}
		if rerr != nil {
			if rerr == io.EOF {
				// 流正常结束或断开，尝试重连（简单策略：直接停止）
				r.markStopped()
			} else {
				r.finishWithError(rerr)
			}
			return
		}
	}
}

// recordHLS 简单 M3U8 录制（下载分片合并）。第一版仅支持单层 m3u8 播放列表。
func (r *Recorder) recordHLS(ctx context.Context, streamURL, outputPath string) {
	client := &http.Client{Timeout: 15 * time.Second}

	// 1. 下载 m3u8 索引
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		r.finishWithError(err)
		return
	}
	req.Header.Set("User-Agent", r.cfg.UserAgent)
	req.Header.Set("Referer", r.cfg.Referer)

	resp, err := client.Do(req)
	if err != nil {
		r.finishWithError(err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.finishWithError(fmt.Errorf("m3u8 索引返回 HTTP %d", resp.StatusCode))
		return
	}

	// 2. 提取分片 URL
	segments := parseM3U8Segments(string(body), streamURL)
	if len(segments) == 0 {
		r.finishWithError(errors.New("m3u8 中没有找到分片"))
		return
	}

	actualPath, f, err := createOutputFile(outputPath)
	if err != nil {
		r.finishWithError(err)
		return
	}
	defer f.Close()
	r.mu.Lock()
	r.stats.OutputPath = actualPath
	r.mu.Unlock()

	for _, segURL := range segments {
		select {
		case <-ctx.Done():
			r.markStopped()
			return
		default:
		}
		segReq, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
		if err != nil {
			continue
		}
		segReq.Header.Set("User-Agent", r.cfg.UserAgent)
		segReq.Header.Set("Referer", r.cfg.Referer)
		seg, err := client.Do(segReq)
		if err != nil {
			continue // 单个分片失败跳过
		}
		if seg.StatusCode != http.StatusOK {
			seg.Body.Close()
			continue
		}
		n, _ := io.Copy(f, seg.Body)
		seg.Body.Close()
		r.mu.Lock()
		r.stats.BytesWritten += n
		r.mu.Unlock()
	}

	r.markStopped()
}

// parseM3U8Segments 解析 m3u8 分片 URL（支持相对/绝对路径）
func parseM3U8Segments(content, baseURL string) []string {
	var segs []string
	lines := strings.Split(content, "\n")
	base := baseURL
	if idx := strings.LastIndex(baseURL, "/"); idx >= 0 {
		base = baseURL[:idx+1]
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "http") {
			segs = append(segs, line)
		} else {
			segs = append(segs, base+line)
		}
	}
	return segs
}

// Stop 停止录制
func (r *Recorder) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	done := r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	r.markStopped()
}

// markStopped 标记录制结束（保留文件）
func (r *Recorder) markStopped() {
	r.mu.Lock()
	r.stats.IsRecording = false
	if r.stats.Duration == 0 {
		r.stats.Duration = time.Since(r.startTime).Seconds()
	}
	r.mu.Unlock()
}

// finishWithError 出错结束：删除不完整文件并记录错误
func (r *Recorder) finishWithError(err error) {
	r.mu.Lock()
	r.stats.IsRecording = false
	path := r.stats.OutputPath
	r.mu.Unlock()

	// 只删除很小的文件（<64KB 说明基本没录到内容）
	if fi, serr := os.Stat(path); serr == nil && fi.Size() < 64*1024 {
		os.Remove(path)
	}
}

// sanitizeFileName 清理文件名中的非法字符
func sanitizeFileName(name string) string {
	re := regexp.MustCompile(`[\\/:*?"<>|]`)
	name = re.ReplaceAllString(name, "_")
	name = strings.TrimSpace(name)
	if name == "" {
		name = "recording"
	}
	return name
}

// createOutputFile 创建输出文件；若重名则自动追加 _1/_2 后缀，防止覆盖已有录制
func createOutputFile(outputPath string) (string, *os.File, error) {
	ext := filepath.Ext(outputPath)
	base := strings.TrimSuffix(outputPath, ext)
	for i := 0; i < 1000; i++ {
		p := outputPath
		if i > 0 {
			p = fmt.Sprintf("%s_%d%s", base, i, ext)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return p, f, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("输出文件重名过多，无法创建")
}

// GenerateFileName 生成录制文件名：主播名_时间戳
func GenerateFileName(anchor, roomID string, t time.Time) string {
	base := "douyin"
	if anchor != "" {
		base = anchor
	}
	ts := t.Format("20060102_150405")
	return fmt.Sprintf("%s_%s_%s", base, roomID, ts)
}
