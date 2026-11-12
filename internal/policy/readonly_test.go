package policy

import "testing"

// TestReadOnlyGuardRejectsSecrets 确认 query 不会成为绕开"读凭据需确认"的通道。
func TestReadOnlyGuardRejectsSecrets(t *testing.T) {
	g := newTestGuard(t)
	if err := g.CheckReadOnly("cat ~/.ssh/id_rsa"); err == nil {
		t.Error("读取私钥应被拒绝")
	}
	if err := g.CheckReadOnly("grep root /etc/shadow"); err == nil {
		t.Error("凭据类路径即使不是 cat 也应被拒绝")
	}
	if err := g.CheckReadOnly("systemctl status nginx"); err != nil {
		t.Errorf("普通只读命令不应受影响: %v", err)
	}
}

func TestReadOnlyAllows(t *testing.T) {
	allowed := []string{
		"ls -la /var/log",
		"cat /etc/os-release",
		"head -n 50 /var/log/syslog",
		"tail -n 100 /var/log/syslog",
		"grep -rn ERROR /var/log",
		"rg -n TODO ./internal",
		"find /var/log -name '*.log' -type f",
		"find /var/log -name '*.log' -printf '%p\\n'",
		"df -h",
		"du -sh /var/log",
		"free -m",
		"ps aux",
		"uptime",
		"ss -lntp",
		"ip addr show",
		"ip -br link",
		"ip route get 1.1.1.1",
		"ip -4 neigh",
		"ip -timestamp addr show",
		"journalctl -u nginx -n 200 --no-pager",
		"systemctl status nginx",
		"systemctl is-active nginx",
		"systemctl list-units --failed",
		"systemctl --no-pager --type service list-units",
		"systemctl --failed",
		"docker ps -a",
		"docker logs --tail 50 myapp",
		"docker inspect myapp",
		"docker images",
		"docker compose ps",
		"docker compose -f docker-compose.yml logs --tail 20",
		"docker container ls",
		"docker system df",
		"docker volume ls",
		"stat /etc/hosts",
		"file /etc/hosts",
		"wc -l /var/log/syslog",
		"sort /etc/hosts",
		"uname -a",
		"hostname -f",
		"id",
		"whoami",
		"date +%F",
		"date -d yesterday +%F",
		"cat /etc/os-release 2>&1 | head -3",
		"ls -la | grep log | head",
		"sudo systemctl status nginx",
		"timeout 5 ls /tmp",
		"env ls /tmp",
		"ls && df -h",
		"cat /etc/hosts; df -h",
	}
	for _, cmd := range allowed {
		if err := CheckReadOnly(cmd); err != nil {
			t.Errorf("本应放行: %q\n  %v", cmd, err)
		}
	}
}

// TestReadOnlyRejects 覆盖所有会产生副作用或无法静态证明的写法。
func TestReadOnlyRejects(t *testing.T) {
	denied := []string{
		"rm -rf /tmp/x",
		"mv /tmp/a /tmp/b",
		"touch /tmp/x",
		"ls > /tmp/out",
		"ls >> /tmp/out",
		"ls &> /tmp/out",
		"ls 2>/dev/null",
		"cat /etc/hosts > /dev/null",
		"echo hi | tee /tmp/out",
		"cat $(rm -rf /tmp/x)",
		"cat $FILE",
		"cat <(rm -rf /tmp/x)",
		"find . -delete",
		"find . -exec rm {} ;",
		"find . -fprint /tmp/out",
		"systemctl restart nginx",
		"systemctl stop nginx",
		"systemctl --user start nginx",
		"docker rm myapp",
		"docker compose down",
		"docker volume rm data",
		"docker system prune -a",
		"ip link set eth0 down",
		"ip addr add 1.2.3.4/24 dev eth0",
		"ip netns exec ns rm -rf /",
		"ip -b /tmp/commands",
		"journalctl --vacuum-time=1d",
		"journalctl --rotate",
		"bash -c 'ls'",
		"sh -c 'cat /etc/hosts'",
		"ssh somehost ls",
		"python3 -c 'print(1)'",
		"unknown-cmd --x",
		"ls && rm x",
		"ls || rm x",
		"ls; rm x",
		"for f in *; do rm $f; done",
		"if true; then rm x; fi",
		"while true; do rm x; done",
		"eval ls",
		"sed -i 's/a/b/' file",
		"ls | xargs rm",
		"sort -o /tmp/out /etc/hosts",
		"rg --pre 'rm x' foo",
		"ss -K dst 1.2.3.4",
		"file -C -m magic",
		"hostname newname",
		"hostname -F /etc/hostname",
		"date -s '2026-01-01'",
		"date 010100002026",
		"cat <<EOF\n$(rm -rf /tmp/x)\nEOF",
		"> /tmp/out",
		// 选项缩写/前缀不能绕过长选项白名单。
		"sort --out=/tmp/x file",
		"sort --output=/tmp/x file",
		"sort --temporary-directory=/etc /etc/hosts",
		"sort -T /etc /etc/hosts",
		"journalctl --vacuum-t 1d",
		"journalctl --rot",
		"journalctl --se=2026-01-01",
		"journalctl --fil=/tmp/x",
		"ss --kil dst 1.2.3.4",
		"file --comp magic",
		"file --compile magic",
		// uniq 的第二个位置参数是输出文件。
		"uniq in.txt pwned.txt",
		// 这些命令本身不带写操作，但能通过选项或路径前缀绕过判定。
		"ss -D",
		"/bin/ls /tmp",
		"./ls /tmp",
		"PATH=/tmp ls",
		"LD_PRELOAD=/tmp/x.so ls",
		"env PATH=/tmp ls",
		"env LD_PRELOAD=/tmp/x.so ls",
		"find . -name x -fprintf /tmp/out %p",
		"find . -fls /tmp/out",
	}
	for _, cmd := range denied {
		if err := CheckReadOnly(cmd); err == nil {
			t.Errorf("本应拒绝却放行: %q", cmd)
		}
	}
}
