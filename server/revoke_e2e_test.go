// revoke e2e 验证：本地测试服务器（DREC_TEST_SERVER，默认 localhost:18081）
// 链路：发码 → 注册 → 禁用 → 已激活设备 parse 403 → 同码再注册被拒
// 运行：go test ./ -run TestRevokeE2E -v -timeout 90s
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRevokeE2E(t *testing.T) {
	serverURL := os.Getenv("DREC_TEST_SERVER")
	if serverURL == "" {
		serverURL = "http://localhost:18081"
	}
	adminKey := os.Getenv("DREC_TEST_ADMIN_KEY")
	if adminKey == "" {
		adminKey = "test-admin-key-123"
	}

	// helper：POST JSON；token 非空时附加 HMAC 签名（与服务端 verifySignature 一致）
	post := func(path, token string, body map[string]interface{}) (int, map[string]interface{}) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
		req, err := http.NewRequest(http.MethodPost, serverURL+path, &buf)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Admin-Key", adminKey)
		if token != "" {
			req.Header.Set("X-Token", token)
			ts := fmt.Sprintf("%d", time.Now().Unix())
			nonce := fmt.Sprintf("%d", time.Now().UnixNano())
			bodyHash := sha256.Sum256(buf.Bytes())
			msg := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", http.MethodPost, path, ts, nonce, hex.EncodeToString(bodyHash[:]))
			mac := hmac.New(sha256.New, []byte(token))
			mac.Write([]byte(msg))
			req.Header.Set("X-Timestamp", ts)
			req.Header.Set("X-Nonce", nonce)
			req.Header.Set("X-Sign", hex.EncodeToString(mac.Sum(nil)))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := map[string]interface{}{}
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	// 1. 发码
	status, out := post("/api/admin/gencode", "", map[string]interface{}{"days": 0, "note": "revoke-e2e", "n": 1})
	if status != http.StatusOK {
		t.Fatalf("❌ gencode HTTP %d: %v", status, out)
	}
	codes, ok := out["codes"].([]interface{})
	if !ok || len(codes) == 0 {
		t.Fatalf("❌ gencode 无返回码: %v", out)
	}
	code := codes[0].(string)
	t.Logf("✅ 发码成功: %s", code)

	// 2. 注册（绑定设备）
	status, out = post("/api/register", "", map[string]interface{}{"code": code, "device_id": "test-device-revoke-e2e"})
	if status != http.StatusOK {
		t.Fatalf("❌ register HTTP %d: %v", status, out)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatalf("❌ register 无 token: %v", out)
	}
	t.Logf("✅ 注册成功 token=%s...", token[:8])

	// 3. 禁用激活码
	status, out = post("/api/admin/revoke", "", map[string]interface{}{"code": code})
	if status != http.StatusOK {
		t.Fatalf("❌ revoke HTTP %d: %v", status, out)
	}
	if out["revoked"] != code {
		t.Fatalf("❌ revoke 返回不符: %v", out)
	}
	t.Logf("✅ 禁用成功: %s", out["revoked"])

	// 4. 已激活设备调用 parse → 应 403 授权已失效
	status, out = post("/api/parse", token, map[string]interface{}{"url": "https://live.douyin.com/985706671742"})
	if status != http.StatusForbidden {
		t.Fatalf("❌ revoke 后 parse 应 403，实际 HTTP %d: %v", status, out)
	}
	msg := strings.ToLower(fmt.Sprint(out["error"]))
	if !strings.Contains(msg, "失效") && !strings.Contains(msg, "revok") {
		t.Logf("⚠️ 403 但错误文案: %v", out)
	}
	t.Logf("✅ 禁用后已激活设备 parse 被拒 (403): %v", out["error"])

	// 5. 同码再注册 → 应被拒
	status, out = post("/api/register", "", map[string]interface{}{"code": code, "device_id": "test-device-revoke-e2e-2"})
	if status == http.StatusOK {
		t.Fatalf("❌ 禁用后同码仍能注册: %v", out)
	}
	t.Logf("✅ 禁用后同码再注册被拒 (HTTP %d): %v", status, out["error"])

	t.Log("🎉 revoke 全链路 PASS")
}
