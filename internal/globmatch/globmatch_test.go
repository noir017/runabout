package globmatch

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"**/*.go", "cmd/a/main.go", true},
		{"**/*.go", "main.go", true},
		{"/proc/**", "/proc/1/status", true},
		{"/proc/**", "/proc", false},
		{"~/.ssh/id_*", "~/.ssh/id_rsa", true},
		{"internal/{mcp,auth}/*.go", "internal/auth/oauth.go", true},
		{"internal/{mcp,auth}/*.go", "internal/tools/shell.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"*_test.go", "shell_test.go", true},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, 期望 %v", c.pattern, c.name, got, c.want)
		}
	}
}
