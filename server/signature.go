// HMAC 签名校验 + 防重放（时间戳窗口 + nonce 一次性）
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxTimestampSkew = 5 * 60 // 时间戳允许偏差：5 分钟

// 防重放存储上限：超过则拒绝新 nonce（防内存撑爆）
const maxNonceEntries = 100000

// nonceStore 防重放：记录已用过的 nonce，超过 TTL 自动清理
type nonceStore struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

func newNonceStore(ttl time.Duration) *nonceStore {
	ns := &nonceStore{seen: map[string]time.Time{}, ttl: ttl}
	go func() {
		ticker := time.NewTicker(ttl / 2)
		defer ticker.Stop()
		for range ticker.C {
			ns.cleanup()
		}
	}()
	return ns
}

func (ns *nonceStore) checkAndSet(nonce string) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if _, ok := ns.seen[nonce]; ok {
		return false // 已用过 = 重放
	}
	if len(ns.seen) >= maxNonceEntries {
		return false // 存储已满：拒绝新 nonce（防内存撑爆）
	}
	ns.seen[nonce] = time.Now()
	return true
}

func (ns *nonceStore) cleanup() {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	cutoff := time.Now().Add(-ns.ttl)
	for k, v := range ns.seen {
		if v.Before(cutoff) {
			delete(ns.seen, k)
		}
	}
}

// verifySignature 校验请求签名。
// 客户端签名内容：HMAC-SHA256(key=token, msg=method\npath\ntimestamp\nnonce\nbodySHA256)
// 签名正确且时间戳在窗口内且 nonce 未被用过 → 通过。
// 校验通过后恢复 r.Body（后续 handler 仍需读取）。
// 顺序：时间戳窗口 → 读 body（限 1MB）→ 验签 → 登记 nonce（仅验签通过的请求占用 nonce，避免无效请求污染防重放存储）。
func verifySignature(w http.ResponseWriter, r *http.Request, token string) (bool, string) {
	ts := r.Header.Get("X-Timestamp")
	nonce := r.Header.Get("X-Nonce")
	sign := r.Header.Get("X-Sign")
	if ts == "" || nonce == "" || sign == "" {
		return false, "缺少签名头 (X-Timestamp/X-Nonce/X-Sign)"
	}

	// 1. 时间戳窗口
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false, "时间戳非法"
	}
	now := time.Now().Unix()
	if now-t > maxTimestampSkew || t-now > maxTimestampSkew {
		return false, "时间戳超出窗口"
	}

	// 2. 读取请求体（限制 1MB，防大 body 内存压力）
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, "读取请求体失败"
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	bodyHash := sha256.Sum256(body)
	msg := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", r.Method, r.URL.Path, ts, nonce, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sign)) {
		return false, "签名不匹配"
	}

	// 3. 防重放：仅对验签通过的请求登记 nonce
	if !globalNonceStore.checkAndSet(nonce) {
		return false, "重放请求"
	}
	return true, ""
}
