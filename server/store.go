// SQLite 存储层：激活码 / 设备 / token（只存 hash，不存明文）
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// maxDevicesPerCode 每个激活码最多绑定的设备数
const maxDevicesPerCode = 2

type Store struct {
	db *sql.DB
}

type ActivationCode struct {
	Code      string
	Status    string // unused / active / revoked
	ExpiresAt int64  // unix 秒，0 = 永久
	CreatedAt int64
	Note      string
}

type Device struct {
	DeviceID   string
	Code       string
	TokenHash  string
	BoundAt    int64
	LastSeenAt int64
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// 单写者，关闭连接池避免锁
	db.SetMaxOpenConns(1)

	schema := `
CREATE TABLE IF NOT EXISTS activation_codes (
	code       TEXT PRIMARY KEY,
	status     TEXT NOT NULL DEFAULT 'unused',
	expires_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	note       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS devices (
	device_id   TEXT NOT NULL,
	code        TEXT NOT NULL,
	token_hash  TEXT NOT NULL,
	bound_at    INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (device_id, code)
);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// hashToken 只存 sha256(token)，DB 泄露也无法伪造签名
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// GenToken 生成 32 字节随机 token
func GenToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenCode 生成人类可读激活码：DREC-XXXX-XXXX-XXXX（去易混淆字符）
func GenCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 无 I/O/0/1
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return fmt.Sprintf("DREC-%s-%s-%s", b[0:4], b[4:8], b[8:12]), nil
}

// CreateCode 创建激活码。days<=0 表示永久有效。
func (s *Store) CreateCode(code string, days int, note string) error {
	var expires int64
	if days > 0 {
		expires = time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	}
	_, err := s.db.Exec(
		`INSERT INTO activation_codes (code, status, expires_at, created_at, note) VALUES (?, 'unused', ?, ?, ?)`,
		code, expires, time.Now().Unix(), note,
	)
	return err
}

func (s *Store) GetCode(code string) (*ActivationCode, error) {
	row := s.db.QueryRow(`SELECT code, status, expires_at, created_at, note FROM activation_codes WHERE code = ?`, code)
	c := &ActivationCode{}
	if err := row.Scan(&c.Code, &c.Status, &c.ExpiresAt, &c.CreatedAt, &c.Note); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) MarkCodeUsed(code string) error {
	_, err := s.db.Exec(`UPDATE activation_codes SET status = 'active' WHERE code = ?`, code)
	return err
}

func (s *Store) CountDevices(code string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM devices WHERE code = ?`, code).Scan(&n)
	return n, err
}

// AddDevice 绑定设备（新绑定或换绑 token），返回新 token。
// 已存在该设备 → 轮换 token；不存在 → 检查设备数上限后插入。
func (s *Store) AddDevice(code, deviceID string) (string, error) {
	token, err := GenToken()
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM devices WHERE device_id = ? AND code = ?`, deviceID, code).Scan(&exists); err != nil {
		return "", err
	}

	if exists > 0 {
		// 换绑：轮换 token，让旧 token 立即失效
		if _, err := tx.Exec(`UPDATE devices SET token_hash = ?, bound_at = ? WHERE device_id = ? AND code = ?`,
			hashToken(token), now, deviceID, code); err != nil {
			return "", err
		}
	} else {
		// 用事务内连接计数，避免与连接池（上限1）死锁
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM devices WHERE code = ?`, code).Scan(&n); err != nil {
			return "", err
		}
		if n >= maxDevicesPerCode {
			return "", fmt.Errorf("设备数已达上限（每码最多 %d 台）", maxDevicesPerCode)
		}
		if _, err := tx.Exec(`INSERT INTO devices (device_id, code, token_hash, bound_at) VALUES (?, ?, ?, ?)`,
			deviceID, code, hashToken(token), now); err != nil {
			return "", err
		}
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

// GetDeviceByTokenHash 根据 token hash 查设备
func (s *Store) GetDeviceByTokenHash(tokenHash string) (*Device, error) {
	row := s.db.QueryRow(`SELECT device_id, code, token_hash, bound_at, last_seen FROM devices WHERE token_hash = ?`, tokenHash)
	d := &Device{}
	if err := row.Scan(&d.DeviceID, &d.Code, &d.TokenHash, &d.BoundAt, &d.LastSeenAt); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Store) TouchDevice(deviceID, code string) error {
	_, err := s.db.Exec(`UPDATE devices SET last_seen = ? WHERE device_id = ? AND code = ?`, time.Now().Unix(), deviceID, code)
	return err
}

// IsCodeValid 校验激活码是否可用（未吊销、未过期）
func (c *ActivationCode) IsValid() bool {
	if c.Status == "revoked" {
		return false
	}
	if c.ExpiresAt > 0 && time.Now().Unix() > c.ExpiresAt {
		return false
	}
	return true
}
