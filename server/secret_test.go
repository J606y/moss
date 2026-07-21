package main

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestSecretRoundTrip(t *testing.T) {
	secretKeyBytes = sha256.Sum256([]byte("test-master-key"))

	cases := []string{"", "mk_short", `{"type":"service_account","private_key":"-----BEGIN..."}`}
	for _, plain := range cases {
		enc := encryptSecret(plain)
		if plain != "" && !strings.HasPrefix(enc, encPrefix) {
			t.Fatalf("密文应带前缀 %q，得到 %q", encPrefix, enc)
		}
		if plain != "" && enc == plain {
			t.Fatalf("密文不应等于明文: %q", plain)
		}
		if got := decryptSecret(enc); got != plain {
			t.Fatalf("往返不一致: 明文 %q → 解出 %q", plain, got)
		}
	}
}

func TestDecryptPlaintextPassthrough(t *testing.T) {
	secretKeyBytes = sha256.Sum256([]byte("test-master-key"))
	// 历史明文（无前缀）应原样透传，保证升级零迁移。
	legacy := `{"type":"service_account"}`
	if got := decryptSecret(legacy); got != legacy {
		t.Fatalf("历史明文应透传，得到 %q", got)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	secretKeyBytes = sha256.Sum256([]byte("key-A"))
	enc := encryptSecret("sensitive")
	secretKeyBytes = sha256.Sum256([]byte("key-B"))
	if got := decryptSecret(enc); got != "" {
		t.Fatalf("换密钥后应解不出（空串），得到 %q", got)
	}
}
