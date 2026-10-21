// 端到端验证：模拟版本3客户端真实调用链
// 激活（/api/register）→ 签名解析（/api/parse）
// 运行：go test ./internal/apiclient/ -run TestE2E -v -timeout 90s
package apiclient

import (
	"os"
	"strings"
	"testing"
)

func TestE2E(t *testing.T) {
	code := os.Getenv("DREC_TEST_CODE")
	if code == "" {
		t.Skip("未设置 DREC_TEST_CODE，跳过端到端验证")
	}
	serverURL := os.Getenv("DREC_TEST_SERVER")
	if serverURL == "" {
		serverURL = "http://localhost:8080"
	}
	url := "https://live.douyin.com/985706671742"

	// 1. 激活（模拟客户端首次运行）
	deviceID, err := NewDeviceID()
	if err != nil {
		t.Fatalf("生成设备ID失败: %v", err)
	}
	c := New(serverURL, deviceID, "")
	resp, err := c.Register(code)
	if err != nil {
		t.Fatalf("❌ 激活失败: %v", err)
	}
	if resp.Token == "" || resp.DeviceID == "" {
		t.Fatalf("❌ 激活响应缺少 token/device_id")
	}
	t.Logf("✅ 激活成功: token=%s... 设备=%s 到期=%d", resp.Token[:8], resp.DeviceID, resp.ExpiresAt)

	// 2. 用返回的 token 构造已激活客户端，签名解析
	ac := New(serverURL, resp.DeviceID, resp.Token)
	info, err := ac.Parse(url)
	if err != nil {
		msg := err.Error()
		// 签名鉴权已通过、但房间未开播：服务器返回 502 + 业务错误（属正常判断）
		if strings.Contains(msg, "已结束") || strings.Contains(msg, "未开播") || strings.Contains(msg, "直播") {
			t.Logf("⚠️ 签名解析已通过鉴权，但房间当前未开播（业务判断正常）: %v", err)
			return
		}
		t.Fatalf("❌ 解析失败（鉴权或网络）: %v", err)
	}
	if info.RoomID == "" {
		t.Fatalf("❌ 解析结果缺少 room_id")
	}
	t.Logf("✅ 签名解析成功: 房间=%s 主播=%s 开播=%v 格式=%s", info.RoomID, info.AnchorName, info.IsLive, info.StreamFormat)
	if len(info.QualityOptions) > 0 {
		t.Logf("   画质 %d 个，最佳: %s", len(info.QualityOptions), info.QualityOptions[0].Label)
	}

	// 3. 未激活客户端解析应被拒（401）
	anon := New(serverURL, deviceID, "wrong-token")
	if _, err := anon.Parse(url); err == nil {
		t.Fatalf("❌ 错误 token 竟解析成功，鉴权失效")
	} else {
		t.Logf("✅ 错误 token 被拒绝: %v", err)
	}
}
