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
	if err := g.CheckReadOnly("cat < /etc/shadow"); err == nil {
		t.Error("从凭据类路径读入也应被拒绝")
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
		"ip -br addr",
		"ip -n netns0 addr",
		"ip -tshort link",
		"lsof -nP -i",
		"lsof -p 123",
		"netstat -tlnp",
		"lsblk -f",
		"lsblk -o NAME,SIZE",
		"blkid -s TYPE -o value /dev/sda1",
		"lscpu -e",
		"who -a",
		"last -n 10",
		"md5sum /etc/hosts",
		"sha256sum /etc/hosts",
		"base64 /etc/hosts",
		"cut -d: -f1 /etc/passwd",
		"tr a-z A-Z",
		"dig +short example.com",
		"realpath -e /etc/hosts",
		"readlink -f /etc/hosts",
		"dirname /etc/hosts",
		"basename /etc/hosts",
		"logread -l 20",
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
		"journalctl --since '-1 hour'",
		"date -d '-1 day' +%F",
		"cat /etc/os-release 2>&1 | head -3",
		"ls -la | grep log | head",
		"sudo systemctl status nginx",
		"sudo ls /tmp",
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
		"cat < $FILE",
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
		// 独立赋值、声明、函数、子 shell、算术、test、流程控制。
		"PATH=/tmp/evil; ls",
		"export PATH=/tmp/evil; ls",
		"declare -x PATH=/tmp/evil; ls",
		"readonly PATH=/tmp/evil; ls",
		"local PATH=/tmp/evil; ls",
		"f() { ls; }; f",
		"f() { ls; }",
		"ls() { /tmp/evil/ls; }; ls",
		"(ls)",
		"{ ls; }",
		"((1+1)); ls",
		"[[ -f /etc/passwd ]]; ls",
		"if ls; then ls; fi",
		"for x in a; do ls; done",
		"while ls; do ls; done",
		"case x in a) ls;; esac",
		"time ls",
		"coproc ls",
		"let x=1; ls",
		"ls &",
		// 包装命令只允许不带选项的 sudo。
		"timeout 5 ls /tmp",
		"timeout 5 env PATH=/tmp/evil ls",
		"nice env LD_PRELOAD=/tmp/x.so ls",
		"sudo env PATH=/tmp/evil ls",
		"sudo -u user ls",
		"sudo -n ls",
		"command ls",
		"xargs ls",
		"nohup ls",
		"env ls /tmp",
		// ip 的全局选项也必须走允许列表。
		"ip -batch addr",
		"ip -batch=/tmp/evil addr",
		"ip --batch addr",
		"ip -force addr",
		// 新加回命令里会写缓存/设备的选项。
		"blkid -c /dev/null /dev/sda1",
		"blkid -g",
		"blkid -w",
		"blkid -S 1M /dev/sda1",
	}
	for _, cmd := range denied {
		if err := CheckReadOnly(cmd); err == nil {
			t.Errorf("本应拒绝却放行: %q", cmd)
		}
	}
}
