package policy

import (
	"fmt"
	"os"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// CheckReadOnly 判断一条命令是否完全由白名单里的只读命令组成。
// 只接受能静态证明的命令：解析失败、命令名不确定、参数含变量或命令替换、
// 写重定向、白名单外的命令一律拒绝。query 工具靠它保证"不产生副作用"。
func CheckReadOnly(command string) error {
	home, _ := os.UserHomeDir()
	if err := checkReadOnlyAST(command); err != nil {
		return err
	}
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

// checkReadOnlyAST 按节点类型做允许列表：只允许简单命令、管道、&&、||、;。
// 赋值、声明、函数、子 shell、算术、test、if/for/while/case 等结构直接拒绝，
// 避免 shellparse.go 只给 CallExpr 建 Cmd 留下的空档。
func checkReadOnlyAST(src string) error {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(strings.NewReader(src), "cmd")
	if err != nil {
		return fmt.Errorf("命令语法无法解析：%v", err)
	}
	var bad string
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil || bad != "" {
			return false
		}
		switch n := node.(type) {
		case *syntax.File, *syntax.Word, *syntax.Lit, *syntax.DblQuoted,
			*syntax.ParamExp, *syntax.Redirect, *syntax.Comment:
			return true
		case *syntax.SglQuoted:
			if n.Dollar {
				bad = "ANSI-C 转义词 $'...'"
				return false
			}
			return true
		case *syntax.Stmt:
			if n.Cmd == nil {
				bad = "只有重定向、没有命令的语句"
				return false
			}
			if n.Background || n.Coprocess || n.Disown {
				bad = "后台或协程语句"
				return false
			}
			return true
		case *syntax.CallExpr:
			return true
		case *syntax.BinaryCmd:
			switch n.Op {
			case syntax.AndStmt, syntax.OrStmt, syntax.Pipe, syntax.PipeAll:
				return true
			}
			bad = "不支持的运算符 " + n.Op.String()
			return false
		default:
			bad = fmt.Sprintf("%T", node)
			return false
		}
	})
	if bad != "" {
		return fmt.Errorf("只读查询不接受这种 shell 结构（%s）", bad)
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
		for _, r := range c.Redirects {
			if r.Op != "<" {
				continue
			}
			if hit, ok := g.matchSecret(r.Target); ok {
				return fmt.Errorf("命令从凭据类路径 %s 读取（命中 %s），只读查询不允许", r.Target, hit)
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
	// 用原始 argv，而不是 Effective：query 只允许不带选项的 sudo，
	// env/timeout/nice/command/xargs/nohup 等包装一律拒绝。
	argv, unres := c.Argv, c.Unresolved
	if len(argv) == 0 {
		return fmt.Errorf("空命令")
	}
	if baseName(argv[0]) == "sudo" {
		if len(argv) < 2 || strings.HasPrefix(argv[1], "-") {
			return fmt.Errorf("sudo 只允许不带选项地直接跟只读命令")
		}
		argv, unres = argv[1:], unres[1:]
	}
	if strings.ContainsRune(argv[0], '/') {
		return fmt.Errorf("命令名 %q 带路径，只读查询只接受白名单里的命令名", argv[0])
	}
	for _, a := range argv {
		if badTilde(a) {
			return fmt.Errorf("参数 %q 用了无法展开的 ~ 用户家目录写法，只读查询不允许", a)
		}
	}
	for _, r := range c.Redirects {
		if badTilde(r.Target) {
			return fmt.Errorf("重定向目标 %q 用了无法展开的 ~ 用户家目录写法，只读查询不允许", r.Target)
		}
	}
	if len(c.Assigns) > 0 {
		return fmt.Errorf("命令带变量赋值前缀（%s=...），会改变被调命令的行为，只读查询不允许",
			c.Assigns[0])
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

// badTilde 拒绝 ~user 这类 ExpandHome 不处理的写法；~ 和 ~/ 仍放行。
func badTilde(s string) bool {
	return strings.HasPrefix(s, "~") && s != "~" && !strings.HasPrefix(s, "~/")
}

// readOnlyRedirect 只放行读入和文件描述符复制；>、>>、&> 等写重定向一律拒绝。
func readOnlyRedirect(r Redir) bool {
	switch r.Op {
	case "<":
		// 读入也要能静态确定路径，否则 cat < $FILE 无法证明安全。
		return !r.Unresolved
	case "<<", "<<-", "<<<":
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
	longValue  map[string]bool
	short      string
	shortValue string
	// shortRequired 是必须带值的短选项；没有紧贴值时跳过下一个 token。
	shortRequired string
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
	for i := 0; i < len(args); i++ {
		a := args[i]
		if end {
			continue
		}
		if a == "--" {
			end = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			name := strings.TrimPrefix(a, "--")
			attached := false
			if j := strings.IndexByte(name, '='); j >= 0 {
				name = name[:j]
				attached = true
			}
			if name == "" || !spec.long[name] {
				return fmt.Errorf("不支持的选项 %q", a)
			}
			// 需要值的选项把下一个 token 当值跳过，值以 - 开头也合法。
			if !attached && spec.longValue[name] && i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			letters := a[1:]
			for j := 0; j < len(letters); j++ {
				ch := letters[j]
				if !strings.ContainsRune(spec.short, rune(ch)) {
					return fmt.Errorf("不支持的短选项 %q", a)
				}
				if strings.ContainsRune(spec.shortValue, rune(ch)) {
					if j == len(letters)-1 && strings.ContainsRune(spec.shortRequired, rune(ch)) &&
						i+1 < len(args) {
						i++
					}
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
		short:         "1AaBbCcDdFfGgHhiIklLmMnNopQqRrSsTtUuVvWwXxZ",
		shortValue:    "ITw",
		longValue:     set(`block-size format hide ignore indicator-style quoting-style sort time time-style width tabsize`),
		shortRequired: "ITw",
	},
	"cat": {
		long:  set(`show-all number-nonblank number squeeze-blank show-ends show-tabs show-nonprinting help version`),
		short: "AbeEnstTuv",
	},
	"head": {
		long:          set(`bytes lines quiet silent verbose zero-terminated help version`),
		short:         "0123456789cnqvz",
		shortValue:    "cn",
		longValue:     set(`bytes lines`),
		shortRequired: "cn",
	},
	"tail": {
		long:          set(`bytes lines follow retry pid sleep-interval max-unchanged-stats quiet silent verbose zero-terminated help version`),
		short:         "0123456789cFfnqsvz",
		shortValue:    "cns",
		longValue:     set(`bytes lines pid sleep-interval max-unchanged-stats`),
		shortRequired: "cns",
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
		longValue: set(`regexp file max-count after-context before-context context label include
			exclude exclude-from exclude-dir directories devices binary-files group-separator`),
		shortRequired: "ABCDefm",
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
		longValue: set(`type type-not type-add type-clear glob iglob max-count max-depth
			max-filesize context after-context before-context context-separator
			field-context-separator field-match-separator threads encoding engine
			regex-size-limit dfa-size-limit ignore-file replace sort sortr path-separator
			pre-glob colors`),
		shortRequired: "ABCdeEfgjmMrtT",
	},
	"find": {custom: checkFind},
	"df": {
		long: set(`all block-size human-readable si inodes local no-sync output portability
			print-type type exclude help version`),
		short:         "aBhiHklPtTvx",
		shortValue:    "Btx",
		longValue:     set(`block-size output type exclude`),
		shortRequired: "Btx",
	},
	"du": {
		long: set(`all apparent-size block-size bytes count-links dereference dereference-args
			human-readable si inodes max-depth null one-file-system summarize total threshold
			time time-style exclude help version`),
		short:         "abBcDhHklLmPsStTx",
		shortValue:    "Bdt",
		longValue:     set(`block-size max-depth threshold time-style exclude`),
		shortRequired: "Bdt",
	},
	"free": {
		long:          set(`bytes kilo mega giga tera peta human si wide low high total count seconds lohi help version`),
		short:         "bkmghHwcltsv",
		shortValue:    "cs",
		longValue:     set(`count seconds`),
		shortRequired: "cs",
	},
	"ps": {
		long: set(`sort format forest headers no-headers cols columns rows width context
			deselect ppid pid sid tty user group Group quick-pid info personality help version`),
		short:      "AadefgGhHlLoOpqrsStTuUvwxyzZcCjMnNVWX",
		shortValue: "gGopOstuUC",
		longValue: set(`sort format cols columns rows width context ppid pid sid tty user group
			Group quick-pid personality`),
		shortRequired: "gGopqOstuUC",
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
		short:         "hVnraloempsitu46012389bcEfFgMNPqTwxyzZAd",
		shortValue:    "AfFN",
		longValue:     set(`state exclude family address`),
		shortRequired: "AfFN",
	},
	"ip": {custom: checkIP},
	"journalctl": {
		long: set(`system user global no-pager no-hostname no-full full lines output output-fields
			reverse show-cursor cursor after-cursor since until unit identifier priority grep
			case-sensitive dmesg merge catalog quiet verify verify-key disk-usage header
			list-catalogs dump-catalog fields field list-fields synced namespace root directory
			file machine boot list-boots utc no-utc help version`),
		short: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
		longValue: set(`since until unit identifier priority grep lines output output-fields
			cursor after-cursor directory file root namespace machine field`),
		shortValue:    "cCDgnopStuU",
		shortRequired: "cCDgnopStuU",
	},
	"systemctl": {
		long: set(`system user global no-pager no-legend no-block quiet full value recursive
			reverse show-types job-mode state type property all failed lines output plain
			timestamp host machine root image runtime no-reload kill-whom signal what now force
			help version no-ask-password message check-inhibitors legend no-hostname
			with-dependencies`),
		short: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
		longValue: set(`job-mode state type property lines output timestamp host machine root
			image drop-in signal kill-whom what message`),
		shortValue:    "tpnoHMs",
		shortRequired: "tpnoHMs",
		check:         checkSystemctl,
	},
	"docker": {
		long: set(`all filter format last latest no-trunc quiet size details follow since tail
			timestamps until digests tree no-reset no-stream one-shot human limit file
			project-name project-directory env-file profile ansi progress parallel host context
			config log-level tls tlscacert tlscert tlskey tlsverify no-color debug api-version
			config-file help version`),
		short: "afqstlHcvnp",
		longValue: set(`filter format last since tail until file project-name project-directory
			env-file profile ansi progress parallel host context config log-level tlscacert
			tlscert tlskey api-version config-file`),
		shortValue:    "Hcln",
		shortRequired: "Hcln",
		check:         checkDocker,
	},
	"stat": {
		long:          set(`format printf file-system dereference terse help version`),
		short:         "cfLt",
		shortValue:    "cf",
		longValue:     set(`format printf`),
		shortRequired: "cf",
	},
	"file": {
		long: set(`brief checking-printout debug dereference exclude exclude-quiet extension help
			keep-going list magic-file mime mime-encoding mime-type no-buffer no-pad no-sandbox
			preserve-date print0 raw special-files uncompress uncompress-noreport version
			parameter separator apple`),
		short:         "abcdeEfhiklLmNnpPrsSvz0",
		shortValue:    "efFmP",
		longValue:     set(`exclude magic-file parameter separator extension`),
		shortRequired: "efFmP",
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
		short:         "bcCdfghikmMnrRsStuzV",
		shortValue:    "kSt",
		longValue:     set(`key field-separator buffer-size random-source files0-from batch-size parallel`),
		shortRequired: "kSt",
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
		long:          set(`date file iso-8601 reference rfc-2822 rfc-3339 universal utc debug help version`),
		short:         "dfIruR",
		shortValue:    "dfIr",
		longValue:     set(`date file reference`),
		shortRequired: "dfr",
		check:         checkDate,
	},
	"lsof": {
		long:          set(`help version`),
		short:         "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ?",
		shortValue:    "cCdFfgGikmMnoOpPrRsSTuxXzZ",
		shortRequired: "cdFgkmopu",
	},
	"netstat": {
		long: set(`all numeric tcp udp raw unix listening program statistics route interfaces
			extend wide timers masquerade fib groups help version`),
		short:         "aAcCdefFgGhHilMnNoOprRsStTuUvVwWxXZ",
		shortValue:    "AF",
		longValue:     set(`family protocol`),
		shortRequired: "AF",
	},
	"lsblk": {
		long: set(`all bytes discard dedup nodeps fs help include inverse json list merge
			noempty output output-all pairs paths perms raw resolve scsi sort sysroot topology
			tree version width zoned`),
		short:         "AabDdEefhiIJlMmMnOoPpPrsStTuvwxz",
		shortValue:    "EeIoTw",
		longValue:     set(`dedup include exclude output sort sysroot width`),
		shortRequired: "EeIow",
	},
	"blkid": {
		long: set(`no-encoding help info list-one label list-names output offset probe
			match-tag match-token uuid usage version hint`),
		short:         "dhilLnOopstuUvVH",
		shortValue:    "LoOstuH",
		longValue:     set(`label output offset match-tag match-token uuid hint`),
		shortRequired: "LoOstuH",
	},
	"lscpu": {
		long: set(`all online bytes caches offline json extended parse sysroot hex physical
			output-all help version`),
		short:         "abBCcJespxy",
		shortValue:    "Ceps",
		longValue:     set(`sysroot`),
		shortRequired: "s",
	},
	"who": {
		long: set(`all boot dead heading login lookup mesg message process count runlevel short
			time users help version`),
		short: "abHdlmpqrstTu",
	},
	"last": {
		long: set(`hostlast dns file fulltimes ip limit nohostname since until present
			fullnames system help version`),
		short:         "0123456789adfFinRstpwx",
		shortValue:    "fnstp",
		longValue:     set(`file limit since until present`),
		shortRequired: "fnstp",
	},
	"md5sum": {
		long:  set(`binary check tag text warn strict status quiet ignore-missing zero help version`),
		short: "bctwz",
	},
	"sha256sum": {
		long:  set(`binary check tag text warn strict status quiet ignore-missing zero help version`),
		short: "bctwz",
	},
	"base64": {
		long:          set(`decode ignore-garbage wrap help version`),
		short:         "diw",
		shortValue:    "w",
		longValue:     set(`wrap`),
		shortRequired: "w",
	},
	"cut": {
		long: set(`bytes characters delimiter fields only-delimited complement
			output-delimiter zero-terminated help version`),
		short:         "bcdfnsz",
		shortValue:    "bcdf",
		longValue:     set(`bytes characters delimiter fields output-delimiter`),
		shortRequired: "bcdf",
	},
	"tr": {
		long:  set(`complement delete squeeze-repeats truncate-set1 help version`),
		short: "cCdst",
	},
	"dig": {
		long:          set(`help version`),
		short:         "46bcdfhkmnpqtuvxy",
		shortValue:    "bcfkpqtxy",
		shortRequired: "bcfkpqtxy",
	},
	"realpath": {
		long: set(`canonicalize-existing canonicalize-missing logical physical quiet strip
			no-symlinks zero relative-to relative-base help version`),
		short:     "emLPqsz",
		longValue: set(`relative-to relative-base`),
	},
	"readlink": {
		long: set(`canonicalize canonicalize-existing canonicalize-missing no-newline quiet
			silent verbose zero help version`),
		short: "femnqsvz",
	},
	"dirname": {
		long:  set(`zero help version`),
		short: "z",
	},
	"basename": {
		long:          set(`multiple suffix zero help version`),
		short:         "asz",
		shortValue:    "s",
		longValue:     set(`suffix`),
		shortRequired: "s",
	},
	"logread": {
		long:          set(`help version`),
		short:         "flzsprFSeh",
		shortValue:    "lsp",
		shortRequired: "lsp",
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

var ipOptionNames = map[string]bool{
	"s": true, "stats": true, "statistics": true,
	"d": true, "details": true,
	"l": true, "loops": true,
	"o": true, "oneline": true,
	"r": true, "resolve": true,
	"n": true, "netns": true,
	"N": true, "Numeric": true,
	"f": true, "family": true,
	"4": true, "6": true, "0": true,
	"M": true, "B": true,
	"br": true, "brief": true,
	"j": true, "json": true,
	"p": true, "pretty": true,
	"c": true, "color": true,
	"t": true, "timestamp": true,
	"ts": true, "tshort": true,
	"rc": true, "rcvbuf": true,
	"a": true, "all": true,
	"h": true, "human": true,
	"iec": true,
	"V":   true, "Version": true,
}

var ipOptionValue = map[string]bool{
	"l": true, "loops": true, "f": true, "family": true,
	"n": true, "netns": true, "rc": true, "rcvbuf": true,
}

var ipObjects = map[string]bool{
	"addr": true, "address": true, "a": true,
	"link": true, "l": true,
	"route": true, "r": true,
	"neigh": true, "neighbor": true, "n": true,
	"rule": true, "tunnel": true, "maddr": true, "netns": true,
}

var ipReadVerbs = map[string]bool{
	"show": true, "list": true, "ls": true, "get": true,
}

// checkIP 先按允许列表解析全局选项，再检查对象和动词。
// ip 的单横线长选项不能当成短选项字母集合，否则 -batch 会被放过。
func checkIP(args []string) error {
	pos, err := parseIPOptions(args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("ip 缺少对象，例如 ip addr show")
	}
	obj := pos[0]
	if !ipObjects[obj] {
		return fmt.Errorf("ip %s 不是只读对象", obj)
	}
	if len(pos) > 1 && !ipReadVerbs[pos[1]] {
		return fmt.Errorf("ip %s %s 会修改网络配置，只读查询不允许", obj, pos[1])
	}
	return nil
}

func parseIPOptions(args []string) ([]string, error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimPrefix(a, "-")
		if strings.HasPrefix(name, "-") {
			name = strings.TrimPrefix(name, "-")
		}
		val := ""
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			val = name[eq+1:]
			name = name[:eq]
		}
		if name == "b" || name == "batch" || name == "force" {
			return nil, fmt.Errorf("ip -%s 会执行文件里的命令或强制操作，只读查询不允许", name)
		}
		if !ipOptionNames[name] {
			return nil, fmt.Errorf("不支持的 ip 选项 %q", a)
		}
		if ipOptionValue[name] && val == "" {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("ip -%s 缺少参数", name)
			}
			i++
		}
	}
	return pos, nil
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
