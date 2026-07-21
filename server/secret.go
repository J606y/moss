package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// encPrefix 标记 settings 表中已加密的敏感值。历史明文无此前缀，读时透传，
// 下次写入自动升级为密文（见 decryptSecret）。
const encPrefix = "enc:v1:"

// secretKeyBytes 进程级主密钥，initSecret 启动时初始化一次，之后只读。
var secretKeyBytes [32]byte

// initSecret 派生主密钥：优先 MOSS_SECRET_KEY 环境变量；否则回退到 dataDir/secret.key
// （0600，首次自动生成）。后者至少让「仅拿到 moss.db」的人解不开敏感列。
func initSecret(dataDir string) {
	if v := os.Getenv("MOSS_SECRET_KEY"); v != "" {
		secretKeyBytes = sha256.Sum256([]byte(v))
		return
	}
	p := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 16 {
		secretKeyBytes = sha256.Sum256(b)
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		log.Fatalf("生成主密钥失败: %v", err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		log.Fatalf("写入 %s 失败: %v", p, err)
	}
	log.Printf("已生成 %s（自动密钥）；生产环境建议改用 MOSS_SECRET_KEY 环境变量，以便密钥与数据库备份分离", p)
	secretKeyBytes = sha256.Sum256(raw)
}

// encryptSecret 用 AES-256-GCM 加密敏感明文，输出带 encPrefix 的 base64。空串原样返回。
func encryptSecret(plain string) string {
	if plain == "" {
		return ""
	}
	block, err := aes.NewCipher(secretKeyBytes[:])
	if err != nil {
		log.Printf("加密敏感值失败，退回明文存储: %v", err)
		return plain
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		log.Printf("加密敏感值失败，退回明文存储: %v", err)
		return plain
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		log.Printf("加密敏感值失败，退回明文存储: %v", err)
		return plain
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct)
}

// decryptSecret 解密 encryptSecret 的输出；无前缀＝历史明文，原样透传；解密失败返回空串。
func decryptSecret(stored string) string {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored
	}
	data, err := base64.StdEncoding.DecodeString(stored[len(encPrefix):])
	if err != nil {
		return ""
	}
	block, err := aes.NewCipher(secretKeyBytes[:])
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	if len(data) < gcm.NonceSize() {
		return ""
	}
	pt, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return ""
	}
	return string(pt)
}
