package policy

import (
	"fmt"
	"os"
	"strings"
)

// CheckReadOnly 判断一条命令是否完全由白名单里的只读命令组成。
// 只接受能静态证明的命令：解析失败、命令名不确定、参数含变量或命令替换、
// 写重定向、白名单外的命令一律拒绝。query 工具靠它保证"不产生副作用"。
func CheckReadOnly(command string) error {
	home, _ := os.UserHomeDir()
	script := ParseShell(command, home)
	if script.ParseErr != nil {
		return fmt.Errorf("命令语法无法解析：%v", script.ParseErr)
	}
	if len(script.Cmds) == 0 {
		return fmt.Errorf("没有识别出可执行的命令")
	}
	for _, c := range script.Cmds {
		if err := checkReadOnlyCmd(c); err != nil {
			return err
		}
	}
	return nil
}

// CheckReadOnly 在只读白名单之上再套一层现有 shell 规则：shell 里需要确认的
// 命令不能走 query，否则 query 会成为绕开确认的通道（例如读取私钥）。
func (g *ShellGuard) CheckReadOnly(command string) error {
	if err := CheckReadOnly(command); err != nil {
		return err
	}
	if v := g.Inspect(command); v.Action != ActionAllow {
		return fmt.Errorf("命令命中安全规则，需要确认或禁止：%s", v.Explain())
	}
	return nil
}

func checkReadOnlyCmd(c *Cmd) error {
	for _, r := range c.Redirects {
		if !readOnlyRedirect(r) {
			return fmt.Errorf("包含写重定向或无法确认的重定向 %q", r.Op)
		}
	}
	argv, unres := c.Effective()
	if len(argv) == 0 {
		return fmt.Errorf("空命令")
	}
	for i, u := range unres {
		if u {
			return fmt.Errorf("参数 %q 含变量、命令替换或进程替换，无法确认是只读", argv[i])
		}
	}
	name := baseName(argv[0])
	check, ok := readonlyCommands[name]
	if !ok {
		return fmt.Errorf("命令 %q 不在只读白名单里", name)
	}
	if check != nil {
		return check(argv[1:])
	}
	return nil
}

// readOnlyRedirect 只放行读入和文件描述符复制；>、>>、&> 等写重定向一律拒绝。
func readOnlyRedirect(r Redir) bool {
	switch r.Op {
	case "<", "<<", "<<-", "<<<":
		return true
	case ">&", "<&":
		return !r.Unresolved && isDigits(r.Target)
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// readonlyCommands 是只读命令白名单。值为 nil 表示命令本身无副作用；
// 非 nil 的检查函数用来挡住同一个命令里会写文件的参数。
var readonlyCommands = map[string]func([]string) error{
	"ls": nil, "cat": nil, "head": nil, "tail": nil,
	"grep": nil, "rg": checkRipgrep, "find": checkFind,
	"df": nil, "du": nil, "free": nil, "ps": nil, "uptime": nil,
	"ss": checkSS, "ip": checkIP,
	"journalctl": checkJournalctl, "systemctl": checkSystemctl, "docker": checkDocker,
	"stat": nil, "file": checkFile, "wc": nil, "sort": checkSort, "uniq": nil,
	"cut": nil, "tr": nil, "base64": nil,
	"md5sum": nil, "sha1sum": nil, "sha256sum": nil,
	"lsblk": nil, "blkid": nil, "lsof": nil, "netstat": nil,
	"uname": nil, "hostname": checkHostname, "id": nil, "whoami": nil, "groups": nil,
	"date": checkDate, "env": nil, "printenv": nil, "echo": nil, "printf": nil, "pwd": nil,
	"which": nil, "type": nil, "readlink": nil, "realpath": nil,
	"dirname": nil, "basename": nil, "nproc": nil, "lscpu": nil, "lsmod": nil,
	"who": nil, "w": nil, "last": nil, "getent": nil,
	"dig": nil, "nslookup": nil, "host": nil, "logread": nil,
}

func checkFind(args []string) error {
	for _, a := range args {
		switch a {
		case "-exec", "-execdir", "-ok", "-okdir", "-delete",
			"-fprint", "-fprint0", "-fprintf", "-fls":
			return fmt.Errorf("find 的 %s 会修改文件，只读查询不允许", a)
		}
	}
	return nil
}

func checkRipgrep(args []string) error {
	for _, a := range args {
		if a == "--pre" || strings.HasPrefix(a, "--pre=") ||
			a == "--hostname-bin" || strings.HasPrefix(a, "--hostname-bin=") {
			return fmt.Errorf("rg 的 %s 会执行外部命令，只读查询不允许", a)
		}
	}
	return nil
}

func checkSS(args []string) error {
	for _, a := range args {
		if a == "--kill" || strings.HasPrefix(a, "--kill=") {
			return fmt.Errorf("ss %s 会关闭连接，只读查询不允许", a)
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 'K') {
			return fmt.Errorf("ss %s 会关闭连接，只读查询不允许", a)
		}
	}
	return nil
}

func checkFile(args []string) error {
	for _, a := range args {
		if a == "-C" || a == "--compile" || strings.HasPrefix(a, "--compile=") {
			return fmt.Errorf("file %s 会写编译后的 magic 文件，只读查询不允许", a)
		}
	}
	return nil
}

func checkSort(args []string) error {
	for _, a := range args {
		if a == "--output" || strings.HasPrefix(a, "--output=") ||
			a == "--compress-program" || strings.HasPrefix(a, "--compress-program=") {
			return fmt.Errorf("sort %s 会写文件或执行外部程序，只读查询不允许", a)
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 'o') {
			return fmt.Errorf("sort %s 会写文件，只读查询不允许", a)
		}
	}
	return nil
}

func checkHostname(args []string) error {
	for _, a := range args {
		switch {
		case a == "-F" || a == "--file" || strings.HasPrefix(a, "--file="):
			return fmt.Errorf("hostname %s 会修改主机名，只读查询不允许", a)
		case a == "-b" || a == "--boot":
			return fmt.Errorf("hostname %s 会修改主机名，只读查询不允许", a)
		}
	}
	if i, ok := firstPositionalIndex(args, nil); ok {
		return fmt.Errorf("hostname %s 会修改主机名，只读查询不允许", args[i])
	}
	return nil
}

func checkDate(args []string) error {
	for _, a := range args {
		if a == "-s" || a == "--set" || strings.HasPrefix(a, "--set=") {
			return fmt.Errorf("date %s 会修改系统时间，只读查询不允许", a)
		}
	}
	if i, ok := firstPositionalIndex(args, dateValueOpts); ok && !strings.HasPrefix(args[i], "+") {
		return fmt.Errorf("date %s 会修改系统时间，只读查询不允许", args[i])
	}
	return nil
}

var dateValueOpts = map[string]bool{
	"-d": true, "--date": true, "-f": true, "--file": true,
	"-r": true, "--reference": true,
}

func checkJournalctl(args []string) error {
	writeOpts := []string{
		"--vacuum-size", "--vacuum-time", "--vacuum-files",
		"--rotate", "--flush", "--sync", "--relinquish-var",
		"--smart-relinquish-var", "--setup-keys", "--update-catalog",
	}
	for _, a := range args {
		for _, w := range writeOpts {
			if a == w || strings.HasPrefix(a, w+"=") {
				return fmt.Errorf("journalctl %s 会修改日志，只读查询不允许", a)
			}
		}
	}
	return nil
}

var systemctlValueOpts = map[string]bool{
	"-t": true, "--type": true, "--state": true,
	"-p": true, "--property": true,
	"-n": true, "--lines": true, "-o": true, "--output": true,
	"-H": true, "--host": true, "-M": true, "--machine": true,
	"--signal": true, "--kill-whom": true, "--root": true, "--image": true,
	"--drop-in": true, "--job-mode": true, "--timestamp": true,
}

var systemctlReadOnly = map[string]bool{
	"status": true, "is-active": true, "is-enabled": true, "is-failed": true,
	"is-system-running": true, "get-default": true, "show-environment": true,
	"list-units": true, "list-unit-files": true, "list-timers": true,
	"list-sockets": true, "list-jobs": true, "list-machines": true,
	"show": true, "cat": true, "help": true,
}

func checkSystemctl(args []string) error {
	i, ok := firstPositionalIndex(args, systemctlValueOpts)
	if !ok {
		// 只带全局选项（systemctl、systemctl --failed）时是列单元，只读。
		return nil
	}
	if !systemctlReadOnly[args[i]] {
		return fmt.Errorf("systemctl %s 会改变系统状态，只读查询不允许", args[i])
	}
	return nil
}

var dockerValueOpts = map[string]bool{
	"-H": true, "--host": true, "-c": true, "--context": true,
	"--config": true, "-l": true, "--log-level": true,
	"--tlscacert": true, "--tlscert": true, "--tlskey": true,
}

var composeValueOpts = map[string]bool{
	"-f": true, "--file": true, "-p": true, "--project-name": true,
	"--project-directory": true, "--env-file": true, "--profile": true,
	"--ansi": true, "--progress": true, "--parallel": true,
}

var dockerTopReadOnly = map[string]bool{
	"ps": true, "images": true, "logs": true, "inspect": true, "stats": true,
	"version": true, "info": true, "top": true, "port": true, "diff": true,
	"events": true, "history": true, "search": true,
}

// dockerObjects 是 "docker <对象> <动词>" 形式的只读动词表。
var dockerObjects = map[string]map[string]bool{
	"container": {"ls": true, "ps": true, "inspect": true, "logs": true,
		"stats": true, "top": true, "port": true, "diff": true},
	"image":   {"ls": true, "inspect": true, "history": true},
	"network": {"ls": true, "inspect": true},
	"volume":  {"ls": true, "inspect": true},
	"system":  {"df": true, "info": true, "events": true},
	"node":    {"ls": true, "ps": true, "inspect": true},
	"service": {"ls": true, "ps": true, "inspect": true, "logs": true},
	"context": {"ls": true, "inspect": true, "show": true},
	"compose": {"ps": true, "logs": true, "config": true, "top": true,
		"images": true, "ls": true, "version": true},
}

func checkDocker(args []string) error {
	i, ok := firstPositionalIndex(args, dockerValueOpts)
	if !ok {
		return fmt.Errorf("docker 缺少子命令")
	}
	sub, rest := args[i], args[i+1:]
	if verbs, ok := dockerObjects[sub]; ok {
		opts := dockerValueOpts
		if sub == "compose" {
			opts = composeValueOpts
		}
		j, ok := firstPositionalIndex(rest, opts)
		if !ok {
			return nil // 只打帮助，无副作用
		}
		if !verbs[rest[j]] {
			return fmt.Errorf("docker %s %s 会改变状态，只读查询不允许", sub, rest[j])
		}
		return nil
	}
	if !dockerTopReadOnly[sub] {
		return fmt.Errorf("docker %s 不在只读白名单里", sub)
	}
	return nil
}

var ipValueOpts = map[string]bool{
	"-n": true, "-netns": true, "-f": true, "-family": true,
	"-l": true, "-loops": true, "-rc": true, "-rcvbuf": true,
	"-t": true, "-timestamp": true,
}

var ipObjects = map[string]bool{
	"addr": true, "address": true, "a": true,
	"link": true, "l": true,
	"route": true, "r": true,
	"neigh": true, "neighbor": true, "n": true,
	"rule": true, "tunnel": true, "maddr": true, "netns": true,
}

var ipReadVerbs = map[string]bool{
	"show": true, "list": true, "get": true, "help": true, "ls": true,
	"identify": true, "pids": true,
}

func checkIP(args []string) error {
	for _, a := range args {
		if a == "-b" || a == "--batch" || strings.HasPrefix(a, "--batch=") {
			return fmt.Errorf("ip %s 会执行文件里的任意 ip 命令，只读查询不允许", a)
		}
	}
	i, ok := firstPositionalIndex(args, ipValueOpts)
	if !ok {
		return fmt.Errorf("ip 缺少对象，例如 ip addr show")
	}
	obj := args[i]
	if !ipObjects[obj] {
		return fmt.Errorf("ip %s 不是只读对象", obj)
	}
	if j, ok := firstPositionalIndex(args[i+1:], ipValueOpts); ok {
		verb := args[i+1+j]
		if !ipReadVerbs[verb] {
			return fmt.Errorf("ip %s %s 会修改网络配置，只读查询不允许", obj, verb)
		}
	}
	return nil
}

// firstPositionalIndex 返回第一个位置参数的索引，跳过选项和已知带值的选项。
func firstPositionalIndex(args []string, valueOpts map[string]bool) (int, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return i + 1, true
			}
			return 0, false
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if strings.Contains(a, "=") {
				continue
			}
			if valueOpts[a] {
				i++
			}
			continue
		}
		return i, true
	}
	return 0, false
}
