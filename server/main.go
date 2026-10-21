// 抖音直播录制 SaaS 服务端（版本3）
// 职责：激活码/设备授权 + HMAC 签名校验 + 限流 + 流地址解析（解析复用 internal/extractor）
// 录制本身在客户端完成，服务器只做轻活，避免带宽成本。
package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"douyin-recorder-v3/server/internal/extractor"
)

// 全局限流器与防重放存储
var (
	globalNonceStore = newNonceStore(15 * time.Minute)
	registerLimiter  = newRateLimiter(10, time.Minute)   // 注册：每 IP 10 次/分
	parseLimiter     = newRateLimiter(60, time.Minute)   // 解析：每设备 60 次/分
	parseIPLimiter   = newRateLimiter(120, time.Minute)  // 解析：每 IP 120 次/分（伪造 token 也先受限）
	adminLimiter     = newRateLimiter(30, time.Minute)   // 管理接口：每 IP 30 次/分（防 X-Admin-Key 暴力破解）
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON encode error: %v", err)
	}
}

func clientIP(r *http.Request) string {
	// 信任 X-Forwarded-For 仅在部署于反向代理后时启用；当前直接取 RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func checkAdminKey(r *http.Request) bool {
	key := os.Getenv("DREC_ADMIN_KEY")
	if key == "" {
		return false
	}
	got := r.Header.Get("X-Admin-Key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

type parseRequest struct {
	URL string `json:"url"`
}

// requireAuth 保护解析接口：token 校验 → 授权状态 → 签名校验 → 限流
func requireAuth(store *Store, next func(w http.ResponseWriter, r *http.Request, dev *Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 限流（IP 级）：伪造 token 也先受限，避免无谓的 DB 查询
		if !parseIPLimiter.Allow("ip:"+clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求过于频繁，请稍后再试"})
			return
		}
		token := strings.TrimSpace(r.Header.Get("X-Token"))
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "缺少 token"})
			return
		}
		dev, err := store.GetDeviceByTokenHash(hashToken(token))
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token 无效，请重新激活"})
			return
		}
		// 授权状态：激活码不能吊销/过期
		code, err := store.GetCode(dev.Code)
		if err != nil || !code.IsValid() {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "授权已失效，请重新激活"})
			return
		}
		// 签名校验（含防重放）
		ok, msg := verifySignature(w, r, token)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msg})
			return
		}
		// 限流：按设备
		if !parseLimiter.Allow("dev:"+dev.DeviceID) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求过于频繁，请稍后再试"})
			return
		}
		_ = store.TouchDevice(dev.DeviceID, dev.Code)
		next(w, r, dev)
	}
}

func main() {
	// 允许通过环境变量指定管理密钥（生成激活码用）
	adminKey := os.Getenv("DREC_ADMIN_KEY")
	if adminKey == "" {
		log.Println("[警告] 未设置 DREC_ADMIN_KEY，管理接口（生成激活码）将不可用")
	}

	store, err := OpenStore("server.db")
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer store.Close()

	ext := extractor.New()

	mux := http.NewServeMux()

	// 公开：健康检查
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// 公开（限流）：激活码注册
	mux.HandleFunc("/api/register", handleRegister(store))

	// 受保护：解析直播间（token + 签名 + 限流）
	mux.HandleFunc("/api/parse", requireAuth(store, func(w http.ResponseWriter, r *http.Request, dev *Device) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		var req parseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url required"})
			return
		}
		info, err := ext.Extract(strings.TrimSpace(req.URL))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, info)
	}))

	// 管理接口（需 X-Admin-Key）：生成激活码 / 查看列表
	mux.HandleFunc("/api/admin/gencode", handleAdminGenCode(store))
	mux.HandleFunc("/api/admin/list", handleAdminList(store))
	mux.HandleFunc("/api/admin/revoke", handleAdminRevoke(store))

	// 监听端口支持环境变量 DREC_ADDR（默认 :8080；公网部署可用高位端口绕开 ICP 拦截）
	addr := os.Getenv("DREC_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("API server listening on %s", addr)
	log.Printf("数据库: server.db | 管理密钥: %s", map[bool]string{true: "已配置", false: "未配置"}[adminKey != ""])

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
