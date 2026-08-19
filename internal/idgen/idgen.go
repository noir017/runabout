// Package idgen 生成带前缀的随机标识符与高熵密钥。
package idgen

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// New 返回形如 "prefix_<32 hex>" 的标识符。
func New(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("idgen: 系统随机源不可用: " + err.Error())
	}
	if prefix == "" {
		return hex.EncodeToString(b)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// Secret 返回 n 字节熵的 URL-safe 随机串，用于令牌与授权码。
func Secret(n int) string {
	if n <= 0 {
		n = 32
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("idgen: 系统随机源不可用: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Hash 返回值的 SHA-256 十六进制摘要；令牌只以摘要形式落盘。
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
