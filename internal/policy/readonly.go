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
	script := ParseShell(command, g.home)
	for _, c := range script.Cmds {
		targets, _ := c.Targets()
		for _, t := range targets {
			if hit, ok := g.matchSecret(t); ok {
				return fmt.Errorf("命令涉及凭据类路径 %s（命中 %s），只读查询不允许", t, hit)
			}
		}
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
	if strings.ContainsRune(argv[0], '/') {
		return fmt.Errorf("命令名 %q 带路径，只读查询只接受白名单里的命令名", argv[0])
	}
	if len(c.Assigns) > 0 {
		return fmt.Errorf("命令带变量赋值前缀（%s=...），会改变被调命令的行为，只读查询不允许",
			c.Assigns[0])
	}
	if hasEnvAssignment(c.Argv) {
		return fmt.Errorf("env 带变量赋值，会改变被调命令的行为，只读查询不允许")
	}
	for i, u := range unres {
		if u {
			return fmt.Errorf("参数 %q 含变量、命令替换或进程替换，无法确认是只读", argv[i])
		}
	}
	name := baseName(argv[0])
	spec, ok := readonlySpecs[name]
	if !ok {
		return fmt.Errorf("命令 %q 不在只读白名单里", name)
	}
	if spec.custom != nil {
		if err := spec.custom(argv[1:]); err != nil {
			return err
		}
	} else if err := checkOptions(argv[1:], spec); err != nil {
		return err
	}
	if spec.check != nil {
		return spec.check(argv[1:])
	}
	return nil
}

// hasEnvAssignment 识别 env PATH=... ls 这种写法。Effective 会把 env 的
// 赋值参数当成包装跳过，只读判定必须自己拦。
func hasEnvAssignment(argv []string) bool {
	if len(argv) == 0 || baseName(argv[0]) != "env" {
		return false
	}
	for _, a := range argv[1:] {
		if strings.Contains(a, "=") {
			return true
		}
	}
	return false
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

// cmdSpec 是一条只读命令的选项白名单。长选项按完整名字精确匹配，
// 短选项按字母判定；不在列表里的一律拒绝。
type cmdSpec struct {
	long       map[string]bool
	short      string
	shortValue string
	// custom 非空时替代通用选项检查（find 的谓词是单横线长词）。
	custom func([]string) error
	// check 在选项检查之后做子命令/位置参数判定。
	check func([]string) error
}

func set(names string) map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(names) {
		m[n] = true
	}
	return m
}

// checkOptions 校验所有选项都在白名单里。--name=value 只取 name 精确匹配，
// 所以 --out= 不会因为像 --output 而被放行。
func checkOptions(args []string, spec cmdSpec) error {
	end := false
	for _, a := range args {
		if end {
			continue
		}
		if a == "--" {
			end = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			name := strings.TrimPrefix(a, "--")
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			if name == "" || !spec.long[name] {
				return fmt.Errorf("不支持的选项 %q", a)
			}
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			letters := a[1:]
			for i := 0; i < len(letters); i++ {
				ch := letters[i]
				if !strings.ContainsRune(spec.short, rune(ch)) {
					return fmt.Errorf("不支持的短选项 %q", a)
				}
				if strings.ContainsRune(spec.shortValue, rune(ch)) {
					break // 后面的部分是选项值
				}
			}
		}
	}
	return nil
}

var readonlySpecs = map[string]cmdSpec{
	"ls": {
		long: set(`all almost-all author block-size color colour directory dired escape
			file-type format full-time group-directories-first hide human-readable ignore
			inode indicator-style kibibytes literal numeric-uid-gid quoting-style reverse
			recursive size sort time time-style version help classify dereference-command-line
			dereference-command-line-symlink-to-dir dereference no-group width tabsize zero
			context si hyperlink`),
		short:      "1AaBbCcDdFfGgHhiIklLmMnNopQqRrSsTtUuVvWwXxZ",
		shortValue: "ITw",
	},
	"cat": {
		long:  set(`show-all number-nonblank number squeeze-blank show-ends show-tabs show-nonprinting help version`),
		short: "AbeEnstTuv",
	},
	"head": {
		long:       set(`bytes lines quiet silent verbose zero-terminated help version`),
		short:      "0123456789cnqvz",
		shortValue: "cn",
	},
	"tail": {
		long:       set(`bytes lines follow retry pid sleep-interval max-unchanged-stats quiet silent verbose zero-terminated help version`),
		short:      "0123456789cFfnqsvz",
		shortValue: "cns",
	},
	"grep": {
		long: set(`basic-regexp extended-regexp fixed-strings perl-regexp regexp file
			ignore-case no-ignore-case invert-match word-regexp line-regexp count color colour
			files-with-matches files-without-match only-matching quiet silent no-messages
			byte-offset with-filename no-filename label line-number initial-tab null null-data
			recursive dereference-recursive directories devices binary-files text
			after-context before-context context group-separator no-group-separator binary
			unix-byte-offsets help version include exclude exclude-from exclude-dir max-count`),
		short:      "ABCDEFGHILPTUabcdefhilmnoqrsuvwxyz",
		shortValue: "ABCDefm",
	},
	"rg": {
		long: set(`type type-not type-add type-clear type-list glob iglob glob-case-insensitive
			no-glob-case-insensitive files files-with-matches files-without-match count
			count-matches json line-number column no-column heading no-heading with-filename
			no-filename color colors context after-context before-context context-separator
			field-context-separator field-match-separator max-count max-depth max-filesize
			mmap no-mmap hidden no-hidden ignore-file ignore-file-case-insensitive no-ignore
			no-ignore-vcs no-ignore-parent no-ignore-global no-ignore-dot no-ignore-exclude
			no-ignore-files follow no-follow multiline no-multiline multiline-dotall
			no-multiline-dotall pcre2 no-pcre2 engine fixed-strings no-fixed-strings
			word-regexp no-word-regexp line-regexp no-line-regexp invert-match case-sensitive
			ignore-case smart-case regex-size-limit dfa-size-limit threads encoding binary
			no-binary text no-text search-zip no-search-zip stats no-stats quiet no-messages
			debug trace version help sort sortr trim no-trim crlf no-crlf null null-data
			one-file-system no-one-file-system passthru no-passthru pretty no-require-git
			require-git stop-on-nonmatch no-stop-on-nonmatch unrestricted no-unrestricted
			vimgrep no-vimgrep replace no-replace path-separator no-path-separator pre-glob`),
		short:      "0123456789aAbBcCdEeFfghHiIjLlMmNnoPpqrRsStTuUvVwxz",
		shortValue: "ABCdeEfgjmMrtT",
	},
	"find": {custom: checkFind},
	"df": {
		long: set(`all block-size human-readable si inodes local no-sync output portability
			print-type type exclude help version`),
		short:      "aBhiHklPtTvx",
		shortValue: "Btx",
	},
	"du": {
		long: set(`all apparent-size block-size bytes count-links dereference dereference-args
			human-readable si inodes max-depth null one-file-system summarize total threshold
			time time-style exclude help version`),
		short:      "abBcDhHklLmPsStTx",
		shortValue: "Bdt",
	},
	"free": {
		long:       set(`bytes kilo mega giga tera peta human si wide low high total count seconds lohi help version`),
		short:      "bkmghHwcltsv",
		shortValue: "cs",
	},
	"ps": {
		long: set(`sort format forest headers no-headers cols columns rows width context
			deselect ppid pid sid tty user group Group quick-pid info personality help version`),
		short:      "AadefgGhHlLoOpqrsStTuUvwxyzZcCjMnNVWX",
		shortValue: "gGopOstuUC",
	},
	"uptime": {
		long:  set(`pretty since help version`),
		short: "hpsV",
	},
	"ss": {
		long: set(`all tcp udp raw unix packet dccp sctp mptcp netlink tipc xdp vsock bluetooth
			listening not-listed established syn-sent syn-recv fin-wait-1 fin-wait-2 time-wait
			closed close-wait last-ack closing state exclude processes info memory options timer
			summary numeric resolve family address no-header oneline extended ipv4 ipv6 wide
			bytes help version`),
		short:      "hVnraloempsitu46012389bcEfFgMNPqTwxyzZAd",
		shortValue: "AfFNP",
	},
	"ip": {
		long: set(`brief statistics json oneline resolve details family netns loops rcvbuf
			timestamp timestampns force human pretty version help all numeric protocol scope
			to from table vrf name group dev via src metric mtu advmss rtt rttvar window cwnd
			ssthresh realms hoplimit encap iif oif mark priority pref proto nhid tun_id remote
			local ttl tos dsfield key seq ack`),
		short: "abcdefghijklmnopqrstuvwxyzV46",
		check: checkIP,
	},
	"journalctl": {
		long: set(`system user global no-pager no-hostname no-full full lines output output-fields
			reverse show-cursor cursor after-cursor since until unit identifier priority grep
			case-sensitive dmesg merge catalog quiet verify verify-key disk-usage header
			list-catalogs dump-catalog fields field list-fields synced namespace root directory
			file machine boot list-boots utc no-utc help version`),
		short: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
	},
	"systemctl": {
		long: set(`system user global no-pager no-legend no-block quiet full value recursive
			reverse show-types job-mode state type property all failed lines output plain
			timestamp host machine root image runtime no-reload kill-whom signal what now force
			help version no-ask-password message check-inhibitors legend no-hostname
			with-dependencies`),
		short: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
		check: checkSystemctl,
	},
	"docker": {
		long: set(`all filter format last latest no-trunc quiet size details follow since tail
			timestamps until digests tree no-reset no-stream one-shot human limit file
			project-name project-directory env-file profile ansi progress parallel host context
			config log-level tls tlscacert tlscert tlskey tlsverify no-color debug api-version
			config-file help version`),
		short: "afqstlHcvnp",
		check: checkDocker,
	},
	"stat": {
		long:       set(`format printf file-system dereference terse help version`),
		short:      "cfLt",
		shortValue: "cf",
	},
	"file": {
		long: set(`brief checking-printout debug dereference exclude exclude-quiet extension help
			keep-going list magic-file mime mime-encoding mime-type no-buffer no-pad no-sandbox
			preserve-date print0 raw special-files uncompress uncompress-noreport version
			parameter separator apple`),
		short:      "abcdeEfhiklLmNnpPrsSvz0",
		shortValue: "efFmP",
	},
	"wc": {
		long:  set(`bytes chars lines max-line-length words help version`),
		short: "clmwL",
	},
	"sort": {
		long: set(`ignore-leading-blanks check debug dictionary-order general-numeric-sort help
			ignore-case key merge month-sort numeric-sort reverse random-sort random-source sort
			stable buffer-size field-separator unique zero-terminated version parallel
			files0-from batch-size`),
		short:      "bcCdfghikmMnrRsStuzV",
		shortValue: "kSt",
	},
	"uname": {
		long:  set(`all kernel-name nodename kernel-release kernel-version machine processor hardware-platform operating-system help version`),
		short: "asnrvmpio",
	},
	"hostname": {
		long:  set(`alias domain fqdn long short ip-address all-ip-addresses yp nodename help version`),
		short: "adfiIsy",
		check: checkHostname,
	},
	"id": {
		long:  set(`user group groups name real zero context help version`),
		short: "aZgnGruz",
	},
	"whoami": {
		long: set(`help version`),
	},
	"pwd": {
		long:  set(`logical physical help version`),
		short: "LP",
	},
	"which": {
		long: set(`all skip-dot skip-tilde show-dot show-tilde tty-only read-alias skip-alias
			read-functions skip-functions help version`),
		short: "as",
	},
	"echo": {
		long:  set(`help version`),
		short: "neE",
	},
	"printf": {
		long: set(`help version`),
	},
	"printenv": {
		long:  set(`help version null zero`),
		short: "0",
	},
	"nproc": {
		long: set(`all ignore help version`),
	},
	"date": {
		long:       set(`date file iso-8601 reference rfc-2822 rfc-3339 universal utc debug help version`),
		short:      "dfIruR",
		shortValue: "dfIr",
		check:      checkDate,
	},
}

// checkFind 只放行只读的 find 谓词。-delete、-exec*、-ok*、-fprint*、-fls
// 都不在列表里；每个带值的谓词会跳过它的值（-mtime -7 里的 -7 不是选项）。
func checkFind(args []string) error {
	valuePreds := map[string]bool{
		"name": true, "iname": true, "path": true, "ipath": true, "regex": true, "iregex": true,
		"type": true, "xtype": true, "size": true, "mtime": true, "atime": true, "ctime": true,
		"mmin": true, "amin": true, "cmin": true, "newer": true, "anewer": true, "cnewer": true,
		"user": true, "group": true, "uid": true, "gid": true, "perm": true, "links": true,
		"inum": true, "samefile": true, "fstype": true, "maxdepth": true, "mindepth": true,
		"regextype": true, "files0-from": true, "printf": true, "D": true,
	}
	flags := map[string]bool{
		"H": true, "L": true, "P": true, "O0": true, "O1": true, "O2": true, "O3": true,
		"depth": true, "d": true, "follow": true, "noleaf": true, "warn": true, "nowarn": true,
		"empty": true, "nouser": true, "nogroup": true, "readable": true, "writable": true,
		"executable": true, "true": true, "false": true, "print": true, "print0": true,
		"ls": true, "prune": true, "quit": true, "a": true, "and": true, "o": true,
		"or": true, "not": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "!" || a == "(" || a == ")" || a == "," || a == "--" {
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue // 搜索起点或谓词的值
		}
		name := strings.TrimPrefix(a, "-")
		if strings.HasPrefix(name, "newer") && len(name) > len("newer") {
			i++ // -newerXY 的值
			continue
		}
		if valuePreds[name] {
			if i+1 >= len(args) {
				return fmt.Errorf("find 的 -%s 缺少参数", name)
			}
			i++
			continue
		}
		if !flags[name] {
			return fmt.Errorf("find 的谓词 -%s 不在只读白名单里", name)
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

func checkHostname(args []string) error {
	if i, ok := firstPositionalIndex(args, nil); ok {
		return fmt.Errorf("hostname %s 会修改主机名，只读查询不允许", args[i])
	}
	return nil
}

func checkDate(args []string) error {
	if i, ok := firstPositionalIndex(args, dateValueOpts); ok && !strings.HasPrefix(args[i], "+") {
		return fmt.Errorf("date %s 会修改系统时间，只读查询不允许", args[i])
	}
	return nil
}

var dateValueOpts = map[string]bool{
	"-d": true, "--date": true, "-f": true, "--file": true,
	"-r": true, "--reference": true,
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
