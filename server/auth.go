// 激活码注册 + 设备绑定 + token 颁发
package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

type registerRequest struct {
	Code     string `json:"code"`
	DeviceID string `json:"device_id"`
}

type registerResponse struct {
	Token     string `json:"token"`
	DeviceID  string `json:"device_id"`
	ExpiresAt int64  `json:"expires_at"` // 0 = 永久
	Code      string `json:"code"`
}

// handleRegister 激活码注册：校验激活码 → 绑定设备 → 颁发 token
func handleRegister(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		// 注册接口按 IP 限流：10 次/分
		ip := clientIP(r)
		if !registerLimiter.Allow("reg:"+ip) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求过于频繁，请稍后再试"})
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效请求体"})
			return
		}
		req.Code = strings.ToUpper(strings.TrimSpace(req.Code))
		req.DeviceID = strings.TrimSpace(req.DeviceID)
		if req.Code == "" || req.DeviceID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 code 或 device_id"})
			return
		}
		if len(req.DeviceID) > 128 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id 过长"})
			return
		}
		// device_id 仅允许 [a-zA-Z0-9_-]（客户端生成的是 32 位 hex，防注入/异常字符）
		for _, c := range req.DeviceID {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id 含非法字符"})
				return
			}
		}

		code, err := store.GetCode(req.Code)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "激活码无效"})
			return
		}
		if !code.IsValid() {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "激活码已失效（过期或已吊销）"})
			return
		}

		token, err := store.AddDevice(req.Code, req.DeviceID)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		_ = store.MarkCodeUsed(req.Code)

		writeJSON(w, http.StatusOK, registerResponse{
			Token:     token,
			DeviceID:  req.DeviceID,
			ExpiresAt: code.ExpiresAt,
			Code:      req.Code,
		})
	}
}

// handleAdminGenCode 管理接口：生成激活码（需 X-Admin-Key 头）
func handleAdminGenCode(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		// 管理接口按 IP 限流：30 次/分（防 X-Admin-Key 暴力破解）
		if !adminLimiter.Allow("admin:"+clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求过于频繁，请稍后再试"})
			return
		}
		if !checkAdminKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "管理密钥错误"})
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req struct {
			Days int    `json:"days"` // 0 = 永久
			Note string `json:"note"`
			N    int    `json:"n"` // 生成数量，默认 1
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效请求体"})
			return
		}
		if req.N <= 0 || req.N > 100 {
			req.N = 1
		}
		codes := make([]string, 0, req.N)
		for i := 0; i < req.N; i++ {
			c, err := GenCode()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成失败"})
				return
			}
			if err := store.CreateCode(c, req.Days, req.Note); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "写入失败"})
				return
			}
			codes = append(codes, c)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"codes": codes})
	}
}

// handleAdminList 管理接口：查看激活码与绑定设备（需 X-Admin-Key 头）
func handleAdminList(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 管理接口按 IP 限流：30 次/分（防 X-Admin-Key 暴力破解）
		if !adminLimiter.Allow("admin:"+clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求过于频繁，请稍后再试"})
			return
		}
		if !checkAdminKey(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "管理密钥错误"})
			return
		}
		// 单条 JOIN 查询，避免打开 rows 后再发起子查询（MaxOpenConns=1 会死锁）
		rows, err := store.db.Query(`SELECT c.code, c.status, c.expires_at, c.created_at, c.note, COUNT(d.device_id)
			FROM activation_codes c LEFT JOIN devices d ON d.code = c.code
			GROUP BY c.code ORDER BY c.created_at DESC LIMIT 200`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
			return
		}
		defer rows.Close()

		type codeRow struct {
			Code      string `json:"code"`
			Status    string `json:"status"`
			ExpiresAt int64  `json:"expires_at"`
			CreatedAt int64  `json:"created_at"`
			Note      string `json:"note"`
			Devices   int    `json:"devices"`
		}
		codes := []codeRow{}
		for rows.Next() {
			var c codeRow
			if err := rows.Scan(&c.Code, &c.Status, &c.ExpiresAt, &c.CreatedAt, &c.Note, &c.Devices); err != nil {
				continue
			}
			codes = append(codes, c)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"codes": codes})
	}
}
