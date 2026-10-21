// Package apiclient 版本3客户端 —— 访问服务器 API 的安全客户端
//
// 职责：
//  1. 激活：POST /api/register（激活码 + 设备ID → token）
//  2. 解析：POST /api/parse（HMAC 签名 + 防重放头）
//  3. token / device_id / server 地址的本地持久化
//
// 签名算法与服务端 signature.go 严格一致：
//   msg  = method\npath\ntimestamp\nnonce\nsha256hex(body)
//   sign = HMAC-SHA256(key=token, msg)
package apiclient

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ---- 与服务器 /api/parse 返回结构对齐 ----

type QualityOption struct {
	Label      string `json:"label"`
	QualityKey string `json:"quality_key"`
	URL        string `json:"url"`
	Format     string `json:"format"`
}

type StreamInfo struct {
	StreamURL      string          `json:"stream_url"`
	StreamFormat   string          `json:"stream_format"`
	RoomID         string          `json:"room_id"`
	AnchorName     string          `json:"anchor_name"`
	Title          string          `json:"title"`
	IsLive         bool            `json:"is_live"`
	QualityOptions []QualityOption `json:"quality_options"`
}

// ---- 本地持久化配置 ----

const configFile = "v3-client-config.json"

type Config struct {
	ServerURL string `json:"server_url"`
	DeviceID  string `json:"device_id"`
	Token     string `json:"token"`
}

// configPath 配置文件放在程序所在目录（解压即用场景）
func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return configFile
	}
	return filepath.Join(filepath.Dir(exe), configFile)
}

// LoadConfig 读取本地配置；不存在则返回空 Config（未激活）
func LoadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return &Config{}, nil // 首次运行：未激活
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("配置文件损坏: %v", err)
	}
	return &c, nil
}

func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0o600)
}

// NewDeviceID 生成设备指纹：32 字节随机 hex（首次运行生成，随后固定）
func NewDeviceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---- 客户端 ----

type Client struct {
	ServerURL string
	Token     string
	DeviceID  string
	http      *http.Client
}

func New(serverURL, deviceID, token string) *Client {
	return &Client{
		ServerURL: stringsTrimSlash(serverURL),
		Token:     token,
		DeviceID:  deviceID,
		http:      &http.Client{Timeout: 45 * time.Second},
	}
}

func stringsTrimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// ---- 签名 ----

// sign 计算 HMAC 签名（与服务端 signature.go 一致）
func (c *Client) sign(method, path, ts, nonce string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	msg := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", method, path, ts, nonce, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, []byte(c.Token))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// signedRequest 组装带签名头的请求
func (c *Client) signedRequest(method, path string, payload interface{}) (*http.Request, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	ts := fmt.Sprintf("%d", time.Now().Unix())
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(nonceBytes)

	req, err := http.NewRequest(method, c.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Token", c.Token)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Nonce", nonce)
	req.Header.Set("X-Sign", c.sign(method, path, ts, nonce, body))
	return req, nil
}

// do 执行请求并解析 JSON 响应
func (c *Client) do(req *http.Request, out interface{}) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接服务器: %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			msg = string(data)
		}
		switch resp.StatusCode {
		case http.StatusForbidden:
			return errors.New(msg) // 激活码失效/设备数上限
		case http.StatusUnauthorized:
			return errors.New("授权校验失败: " + msg)
		case http.StatusTooManyRequests:
			return errors.New(msg)
		case http.StatusBadGateway:
			return errors.New(msg)
		default:
			return fmt.Errorf("服务器返回 %d: %s", resp.StatusCode, msg)
		}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ---- 激活 ----

type registerRequest struct {
	Code     string `json:"code"`
	DeviceID string `json:"device_id"`
}

type registerResponse struct {
	Token     string `json:"token"`
	DeviceID  string `json:"device_id"`
	ExpiresAt int64  `json:"expires_at"`
	Code      string `json:"code"`
}

// Register 激活：激活码 + 设备ID → token。激活成功后客户端应持有返回的 token。
func (c *Client) Register(code string) (*registerResponse, error) {
	req, err := http.NewRequest(http.MethodPost, c.ServerURL+"/api/register",
		bytes.NewReader(mustJSON(registerRequest{Code: code, DeviceID: c.DeviceID})))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	var out registerResponse
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- 解析 ----

// Parse 调服务器 /api/parse 解析直播间（带签名），返回流信息。
// 服务器解析成功 = 拿到了真实流地址；客户端据此本地直录。
func (c *Client) Parse(url string) (*StreamInfo, error) {
	req, err := c.signedRequest(http.MethodPost, "/api/parse", map[string]string{"url": url})
	if err != nil {
		return nil, err
	}
	var info StreamInfo
	if err := c.do(req, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
