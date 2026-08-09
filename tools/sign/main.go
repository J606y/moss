// 发布签名工具：用 ed25519 私钥给 SHA256SUMS 签名，供 agent 自升级校验。
//
// 只在 CI 的发布流程里跑（见 .github/workflows/release.yml），不进任何发布产物。
// 自己写而不引第三方签名工具，是为了让密钥格式与 agent 侧的校验代码严格对齐——
// 两边各自解读一份「约定」是这类问题最常见的出错方式。
//
// 生成密钥对：go run ./tools/sign -genkey
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	genkey := flag.Bool("genkey", false, "生成一对新密钥并打印，不写文件")
	keyPath := flag.String("key", "", "私钥文件路径（base64 编码的 ed25519 私钥）")
	in := flag.String("in", "", "待签名文件")
	out := flag.String("out", "", "签名输出路径（base64）")
	flag.Parse()

	if *genkey {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			log.Fatalf("生成密钥失败: %v", err)
		}
		fmt.Println("把下面两个值分别配到 GitHub 仓库设置里：")
		fmt.Println()
		fmt.Println("Secrets → MOSS_RELEASE_PRIVKEY（私钥，绝不能外泄）:")
		fmt.Println(base64.StdEncoding.EncodeToString(priv))
		fmt.Println()
		fmt.Println("Variables → MOSS_RELEASE_PUBKEY（公钥，会编进 agent）:")
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
		return
	}

	if *keyPath == "" || *in == "" || *out == "" {
		log.Fatal("用法: sign -key <私钥> -in <文件> -out <签名>，或 sign -genkey")
	}

	rawKey, err := os.ReadFile(*keyPath)
	if err != nil {
		log.Fatalf("读取私钥失败: %v", err)
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(rawKey)))
	if err != nil {
		log.Fatalf("私钥不是合法的 base64: %v", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		log.Fatalf("私钥长度应为 %d 字节，实际 %d", ed25519.PrivateKeySize, len(priv))
	}

	data, err := os.ReadFile(*in)
	if err != nil {
		log.Fatalf("读取待签名文件失败: %v", err)
	}
	// 对文件原始字节签名，不做任何规范化：agent 侧校验的也是它下载到的原始字节，
	// 任何一侧多一次 trim 或换行转换，签名就对不上了。
	sig := ed25519.Sign(ed25519.PrivateKey(priv), data)

	if err := os.WriteFile(*out, []byte(base64.StdEncoding.EncodeToString(sig)), 0o644); err != nil {
		log.Fatalf("写入签名失败: %v", err)
	}
}
