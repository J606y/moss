package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// encPrefix 标记 settings 表中已加密的敏感值。历史明文无此前缀，读时透传，
// 下次写入自动升级为密文（见 decryptSecret）。
const encPrefix = "enc:v1:"

// minSecretKeyLen secret.key 的最小可接受长度。低于此值不是「格式旧」，是文件坏了。
const minSecretKeyLen = 16

// secretKeyBytes 进程级主密钥，initSecret 启动时初始化一次，之后只读。
var secretKeyBytes [32]byte

// secretRandRead 是 crypto/rand.Read 的可替换入口，只为让测试能构造出
// 「熵源异常」这条现实中极罕见、却决定 fail-open/fail-closed 走向的分支。
var secretRandRead = rand.Read

// errSecretUndecryptable 密文存在但解不开。
//
// 调用方必须据此把「解不开」与「没配」区分开：两者都当成空值的话，面板只显示
// 「未配置」，运维的第一反应是重新填一遍——而重新填一遍会用当前密钥覆盖掉原密文，
// 于是「找回旧密钥就能救回」的局面被亲手变成永久丢失。
var errSecretUndecryptable = errors.New("敏感值解密失败：主密钥与密文不匹配（MOSS_SECRET_KEY 或 secret.key 是否变动过）")

// initSecret 初始化进程主密钥；失败直接终止启动。
//
// 不做「失败就换一把新密钥继续跑」的兜底：那等于在启动瞬间销毁全部既有密文。
func initSecret(dataDir string) {
	key, err := loadMasterKey(dataDir)
	if err != nil {
		log.Fatalf("初始化主密钥失败: %v", err)
	}
	secretKeyBytes = key
}

// loadMasterKey 派生主密钥：优先 MOSS_SECRET_KEY 环境变量；否则回退到 dataDir/secret.key
// （0600，首次自动生成）。后者至少让「仅拿到 moss.db」的人解不开敏感列。
//
// 只有在 secret.key 确认不存在时才生成新密钥。文件存在但读不出或长度不足时一律报错，
// 绝不覆写：那份文件是它加密过的所有敏感值的唯一钥匙，被随机数覆盖后密文永久不可解。
// 「从备份恢复了一个被截断的 secret.key」是真实会发生的场景，静默覆盖会把一次可修复的
// 文件损坏升级成不可逆的数据销毁，而日志上只留下一句「已生成（自动密钥）」。
func loadMasterKey(dataDir string) ([32]byte, error) {
	var zero [32]byte
	if v := os.Getenv("MOSS_SECRET_KEY"); v != "" {
		return sha256.Sum256([]byte(v)), nil
	}
	p := filepath.Join(dataDir, "secret.key")
	b, err := os.ReadFile(p)
	switch {
	case err == nil:
		if len(b) < minSecretKeyLen {
			return zero, fmt.Errorf("%s 已存在但只有 %d 字节（至少 %d 字节），文件可能被截断或损坏；"+
				"它是既有密文的唯一钥匙，因此不会自动重建——请从备份恢复完整文件，"+
				"或在确认无需解密任何既有敏感值后手动删除它再启动", p, len(b), minSecretKeyLen)
		}
		return sha256.Sum256(b), nil
	case !errors.Is(err, os.ErrNotExist):
		// 权限不足、路径是目录等情形同样不能当作「没有这个文件」去覆写，理由同上。
		return zero, fmt.Errorf("读取 %s 失败: %w", p, err)
	}
	raw := make([]byte, 32)
	if _, err := secretRandRead(raw); err != nil {
		return zero, fmt.Errorf("生成主密钥失败: %w", err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return zero, fmt.Errorf("写入 %s 失败: %w", p, err)
	}
	log.Printf("已生成 %s（自动密钥）；生产环境建议改用 MOSS_SECRET_KEY 环境变量，以便密钥与数据库备份分离", p)
	return sha256.Sum256(raw), nil
}

// encryptSecret 用 AES-256-GCM 加密敏感明文，输出带 encPrefix 的 base64。空串原样返回。
//
// 加密失败必须返回错误、由调用方拒绝保存（fail-closed）。曾经的做法是记一条日志后退回
// 明文入库，但那份明文不带 encPrefix，与「历史明文」完全无法区分，decryptSecret 会照常
// 透传，读写都正常——于是这次降级既不会被发现，也永远不会被下次保存自动修复，
// GCP 服务账号私钥就此长期躺在明文列里。宁可保存失败让人当场看见。
func encryptSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	block, err := aes.NewCipher(secretKeyBytes[:])
	if err != nil {
		return "", fmt.Errorf("加密敏感值失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("加密敏感值失败: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := secretRandRead(nonce); err != nil {
		return "", fmt.Errorf("加密敏感值失败（随机数不可用）: %w", err)
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

// decryptSecretValue 解密 encryptSecret 的输出；无前缀＝历史明文，原样透传（升级零迁移）。
// 解不开时返回空串与 errSecretUndecryptable 包装的错误，调用方据此区分「解不开」与「没配」。
func decryptSecretValue(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	data, err := base64.StdEncoding.DecodeString(stored[len(encPrefix):])
	if err != nil {
		return "", fmt.Errorf("%w: base64 解码失败: %v", errSecretUndecryptable, err)
	}
	block, err := aes.NewCipher(secretKeyBytes[:])
	if err != nil {
		return "", fmt.Errorf("%w: %v", errSecretUndecryptable, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errSecretUndecryptable, err)
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: 密文长度不足", errSecretUndecryptable)
	}
	pt, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("%w: 认证校验未通过", errSecretUndecryptable)
	}
	return string(pt), nil
}

// decryptSecret 是 decryptSecretValue 的便捷包装：解不开时记日志并返回空串。
//
// 日志是这里唯一的止损点。只返回空串的话，「主密钥变了」在面板上和「凭证没填」
// 长得一模一样，排查方向会被直接带偏；能拿到 error 的调用点应当用
// decryptSecretValue 把原因暴露给用户，而不是用这个包装。
func decryptSecret(stored string) string {
	v, err := decryptSecretValue(stored)
	if err != nil {
		log.Printf("%v", err)
		return ""
	}
	return v
}
