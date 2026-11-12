package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noir017/runabout/internal/config"
)

// 远端路径按字面匹配：不展开 ~、不解析符号链接。
func TestCheckReadLiteral(t *testing.T) {
	g := NewPathGuard(config.Default().Policy, "/work")
	for _, p := range []string{"~/.ssh/id_rsa", "/home/alice/.ssh/id_ed25519", "/etc/shadow"} {
		if _, err := g.CheckReadLiteral(p); err == nil {
			t.Errorf("%s 应被拒绝", p)
		}
	}
	if _, err := g.CheckReadLiteral("/home/alice/.ssh/known_hosts"); err != nil {
		t.Errorf("非凭据文件不该被拒绝: %v", err)
	}
}

// 这是已知边界：远端黑名单不解析符号链接，只按字面比对。
func TestCheckReadLiteralDoesNotResolveSymlinks(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "shadow-link")
	if err := os.Symlink("/etc/shadow", link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	g := NewPathGuard(config.Default().Policy, dir)
	if _, err := g.CheckReadLiteral(link); err != nil {
		t.Errorf("字面匹配不应解开符号链接: %v", err)
	}
	// 本机路径解析会拦住它，两者行为差异是设计的一部分。
	if _, err := g.CheckRead(link); err == nil {
		t.Error("本机解析软链接后应命中 /etc/shadow")
	}
}

func TestCheckWriteLiteral(t *testing.T) {
	g := NewPathGuard(config.Default().Policy, "/work")
	if _, err := g.CheckWriteLiteral("~/.ssh/authorized_keys"); err == nil {
		t.Error("远端写 authorized_keys 应被拒绝")
	}
	if _, err := g.CheckWriteLiteral("/home/alice/.ssh/id_rsa"); err == nil {
		t.Error("远端写私钥应被拒绝")
	}
}
