package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleConfigParses 保证仓库里的示例配置与结构体不脱节。
// 配置字段是严格模式解析的，示例里写错一个字段名会让人在部署时才发现。
func TestExampleConfigParses(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("找不到示例配置: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("示例配置解析失败: %v", err)
	}
	if cfg.Server.BaseURL == "" || cfg.Auth.AccessTokenTTL == 0 {
		t.Error("示例配置没有填上关键字段")
	}
	if len(cfg.Policy.ReadDenyPaths) == 0 || len(cfg.Policy.WriteDenyPaths) == 0 {
		t.Error("示例配置应带上路径黑名单")
	}
}

func TestBaseURLRequired(t *testing.T) {
	cfg := Default()
	cfg.Server.BaseURL = ""
	if err := cfg.Normalize(); err == nil {
		t.Error("base_url 为空时应报错")
	}
	cfg = Default()
	cfg.Server.BaseURL = "不是网址"
	if err := cfg.Normalize(); err == nil {
		t.Error("base_url 非绝对地址时应报错")
	}
}

func TestAuthRequiresCredentials(t *testing.T) {
	cfg := Default()
	cfg.Auth.Enabled = true
	cfg.Auth.Users = nil
	cfg.Auth.StaticTokens = nil
	err := cfg.Normalize()
	if err == nil {
		t.Fatal("开了鉴权却没有任何凭据时应报错")
	}
	// 报错信息要能直接告诉人下一步做什么
	if want := "hash-password"; !contains(err.Error(), want) {
		t.Errorf("错误信息应提示怎么生成密码哈希，实际: %v", err)
	}
}

func TestDataDirIsProtected(t *testing.T) {
	cfg := Default()
	cfg.Server.DataDir = "/var/lib/atm"
	cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range cfg.Policy.WriteDenyPaths {
		if p == "/var/lib/atm/**" {
			found = true
		}
	}
	if !found {
		t.Error("data_dir 必须自动进入写黑名单，否则 agent 能改令牌库")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
