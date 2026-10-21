package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"douyin-recorder-v3/client/internal/apiclient"
	"douyin-recorder-v3/client/internal/recorder"
)

// 默认服务器地址（可在激活面板修改）
const defaultServerURL = "http://111.170.152.215:18080"

// App 持有后端状态，方法会被自动绑定到前端
type App struct {
	ctx      context.Context
	api      *apiclient.Client
	rec      *recorder.Recorder
	config   *apiclient.Config
	lastInfo *apiclient.StreamInfo
	lastURL  string // 最近一次成功解析的 URL（用于复用解析结果）
}

func NewApp() *App {
	cfg, err := apiclient.LoadConfig()
	if err != nil {
		log.Printf("[警告] 读取本地配置失败（视为未激活，请重新激活）: %v", err)
		cfg = nil
	}
	var api *apiclient.Client
	if cfg != nil && cfg.Token != "" && cfg.ServerURL != "" {
		api = apiclient.New(cfg.ServerURL, cfg.DeviceID, cfg.Token)
	}
	return &App{
		api:    api,
		rec:    recorder.New(recorder.Config{}),
		config: cfg,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// ensureConfig 确保 config 已加载
func (a *App) ensureConfig() {
	if a.config == nil {
		a.config, _ = apiclient.LoadConfig()
	}
	if a.config == nil {
		a.config = &apiclient.Config{}
	}
}

// GetActivationStatus 返回激活状态（前端启动时调用）
func (a *App) GetActivationStatus() map[string]interface{} {
	a.ensureConfig()
	activated := a.api != nil && a.config.Token != ""
	serverURL := a.config.ServerURL
	if serverURL == "" {
		serverURL = defaultServerURL
	}
	return map[string]interface{}{
		"activated":  activated,
		"server_url": serverURL,
		"device_id":  a.config.DeviceID,
	}
}

// Activate 激活：激活码 → 服务器注册 → 保存 token → 初始化 api 客户端
// 设备 ID 首次生成后固定（同一台机器重新激活绑定同一设备）
func (a *App) Activate(code, serverURL string) error {
	a.ensureConfig()
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return fmt.Errorf("请输入激活码")
	}
	serverURL = strings.TrimSpace(serverURL)
	if serverURL == "" {
		serverURL = defaultServerURL
	}

	// 复用已有设备ID，避免换绑产生新设备
	deviceID := a.config.DeviceID
	if deviceID == "" {
		var err error
		deviceID, err = apiclient.NewDeviceID()
		if err != nil {
			return err
		}
	}

	client := apiclient.New(serverURL, deviceID, "")
	resp, err := client.Register(code)
	if err != nil {
		return err
	}

	a.config = &apiclient.Config{
		ServerURL: serverURL,
		DeviceID:  resp.DeviceID,
		Token:     resp.Token,
	}
	if err := a.config.Save(); err != nil {
		return err
	}
	a.api = apiclient.New(serverURL, resp.DeviceID, resp.Token)
	return nil
}

// Parse 通过服务器解析直播间（本地不再解析）
func (a *App) Parse(url string) (*apiclient.StreamInfo, error) {
	if a.api == nil {
		return nil, fmt.Errorf("未激活，请先输入激活码")
	}
	info, err := a.api.Parse(url)
	if err != nil {
		return nil, err
	}
	a.lastInfo = info
	a.lastURL = url
	return info, nil
}

// StartRecord 开始录制指定画质（qualityKey 为空则用默认最佳）
// 流地址由服务器解析返回，录制仍在本地 FLV 直录
func (a *App) StartRecord(url, qualityKey string) (string, error) {
	if a.api == nil {
		return "", fmt.Errorf("未激活，请先输入激活码")
	}
	// 复用最近一次解析结果：同一 URL 不重复调服务器（省公网往返 + 限流配额）
	var info *apiclient.StreamInfo
	if a.lastInfo != nil && strings.TrimSpace(a.lastURL) == strings.TrimSpace(url) {
		info = a.lastInfo
	} else {
		var err error
		info, err = a.api.Parse(url)
		if err != nil {
			return "", err
		}
		a.lastInfo = info
		a.lastURL = url
	}
	a.lastInfo = info

	streamURL := info.StreamURL
	if qualityKey != "" {
		for _, q := range info.QualityOptions {
			if q.QualityKey == qualityKey {
				streamURL = q.URL
				break
			}
		}
	}
	if streamURL == "" {
		return "", fmt.Errorf("未找到可用的流地址")
	}

	fileName := recorder.GenerateFileName(info.AnchorName, info.RoomID, time.Now())
	output, err := a.rec.Start(streamURL, fileName)
	if err != nil {
		return "", err
	}
	return output, nil
}

// StopRecord 停止录制
func (a *App) StopRecord() {
	a.rec.Stop()
}

// GetStatus 获取当前录制状态（前端轮询用）
func (a *App) GetStatus() recorder.Stats {
	return a.rec.Stats()
}

// GetLastInfo 获取最近一次解析结果（用于切换画质）
func (a *App) GetLastInfo() *apiclient.StreamInfo {
	return a.lastInfo
}
