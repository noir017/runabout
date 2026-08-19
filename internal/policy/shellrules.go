package policy

import (
	"strings"
)

// cmdRule 是一条针对单条命令的规则。match 返回 ActionAllow 表示未命中。
type cmdRule struct {
	id     string
	reason string
	hint   string
	match  func(c *Cmd, g *ShellGuard) (Action, string)
}

// pipeRule 是一条针对整条管道的规则。
type pipeRule struct {
	id     string
	reason string
	hint   string
	match  func(p *Pipeline, g *ShellGuard) (Action, string)
}

// textRule 在 AST 之外补一层原始文本匹配，用于兜住语法层面难表达的模式
// （fork bomb）以及解析失败时的降级检查。
type textRule struct {
	id     string
	reason string
	hint   string
	action Action
	match  func(raw string) bool
}

func in(s string, set ...string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

func hasPrefixAny(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// builtinCmdRules 是内置的单命令规则。顺序无关，命中即累加 finding。
func builtinCmdRules() []cmdRule {
	return []cmdRule{
		{
			id:     "rm-recursive",
			reason: "递归删除高风险路径",
			hint:   "删除请指向具体子目录，例如 rm -rf ./build 而不是 rm -rf * 或 rm -rf /path",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if c.Name() != "rm" {
					return ActionAllow, ""
				}
				if !c.HasFlag('r', "recursive") && !c.HasFlag('R') {
					return ActionAllow, ""
				}
				targets, unres := c.Targets()
				act, hit := worstTarget(targets, unres, g.home)
				if act == ActionAllow {
					return ActionAllow, ""
				}
				return act, "删除目标: " + hit
			},
		},
		{
			id:     "rm-critical-file",
			reason: "删除关键系统文件",
			hint:   "确认这个文件真的可以没有；系统账号、fstab、sshd_config 之类删掉会导致无法登录",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if !in(c.Name(), "rm", "unlink", "shred") {
					return ActionAllow, ""
				}
				targets, _ := c.Targets()
				for _, t := range targets {
					if isCriticalFile(t, g.home) {
						return ActionConfirm, "目标文件: " + t
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "disk-format",
			reason: "格式化或直接改写磁盘",
			hint:   "这类操作不可撤销，且会连带毁掉宿主机上其他服务的数据",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				name := c.Name()
				formatters := hasPrefixAny(name, "mkfs", "mkswap", "mke2fs") ||
					in(name, "wipefs", "fdisk", "sfdisk", "cfdisk", "sgdisk", "parted", "gdisk",
						"badblocks", "hdparm", "blkdiscard", "cryptsetup", "zpool", "lvremove",
						"vgremove", "pvremove", "nvme")
				if !formatters {
					return ActionAllow, ""
				}
				targets, _ := c.Targets()
				for _, t := range targets {
					if isBlockDevice(t) {
						return ActionDeny, "设备: " + t
					}
				}
				if in(name, "zpool", "lvremove", "vgremove", "pvremove") &&
					(c.HasArg("destroy") || name != "zpool") {
					return ActionConfirm, "卷管理销毁操作"
				}
				if in(name, "cryptsetup", "nvme") &&
					(c.HasArg("luksFormat") || c.HasArg("erase") || c.HasArg("format")) {
					return ActionConfirm, "加密卷/固件级擦除"
				}
				return ActionConfirm, "磁盘工具: " + name
			},
		},
		{
			id:     "dd-to-device",
			reason: "dd 写入设备或系统路径",
			hint:   "确认 of= 指向的是文件而不是整块磁盘；写错一次盘上数据就没了",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if c.Name() != "dd" {
					return ActionAllow, ""
				}
				for _, a := range c.Args() {
					if !strings.HasPrefix(a, "of=") {
						continue
					}
					target := strings.TrimPrefix(a, "of=")
					if isBlockDevice(target) {
						return ActionDeny, "输出设备: " + target
					}
					if act := classifyPath(target, g.home); act != ActionAllow {
						return ActionConfirm, "输出路径: " + target
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "chmod-chown-recursive",
			reason: "递归改动系统目录的权限或属主",
			hint:   "chmod -R 777 / 会让 sshd/sudo 直接拒绝工作；请把范围限定到项目目录",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if !in(c.Name(), "chmod", "chown", "chgrp", "setfacl") {
					return ActionAllow, ""
				}
				targets, unres := c.Targets()
				// 第一个参数是权限/属主本身，不是路径。
				if len(targets) > 1 {
					targets, unres = targets[1:], unres[1:]
				}
				act, hit := worstTarget(targets, unres, g.home)
				if act == ActionAllow {
					return ActionAllow, ""
				}
				if !c.HasFlag('R', "recursive") {
					// 非递归时降一级：改单个文件的权限影响有限。
					if act == ActionDeny {
						act = ActionConfirm
					} else {
						return ActionAllow, ""
					}
				}
				return act, "目标: " + hit
			},
		},
		{
			id:     "power-control",
			reason: "关机、重启或切换运行级别",
			hint:   "机器一旦重启，这个 MCP 服务和当前会话都会断开",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				name := c.Name()
				if in(name, "shutdown", "reboot", "poweroff", "halt", "telinit") {
					return ActionConfirm, name
				}
				if name == "init" {
					for _, a := range c.Args() {
						if in(a, "0", "1", "6") {
							return ActionConfirm, "init " + a
						}
					}
				}
				if in(name, "systemctl", "loginctl") {
					for _, a := range c.Args() {
						if in(a, "reboot", "poweroff", "halt", "kexec", "emergency", "rescue") {
							return ActionConfirm, name + " " + a
						}
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "firewall-flush",
			reason: "清空防火墙规则或关闭防火墙",
			hint:   "清规则通常会立刻切断 SSH 与反代入口，把自己关在门外",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				switch c.Name() {
				case "iptables", "ip6tables", "iptables-legacy", "iptables-nft", "ebtables", "arptables":
					if c.HasFlag('F', "flush") || c.HasFlag('X', "delete-chain") || c.HasFlag('P', "policy") {
						return ActionConfirm, "iptables 清链/改默认策略"
					}
				case "nft":
					if c.HasArg("flush") || c.HasArg("delete") {
						return ActionConfirm, "nft flush/delete"
					}
				case "ufw":
					for _, a := range c.Args() {
						if in(a, "disable", "reset") {
							return ActionConfirm, "ufw " + a
						}
					}
				case "firewall-cmd":
					for _, a := range c.Args() {
						if in(a, "--panic-on", "--complete-reload") {
							return ActionConfirm, "firewall-cmd " + a
						}
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "mass-kill",
			reason: "批量杀进程",
			hint:   "kill -1 / killall5 会把整台机器的进程打光；请指定具体 PID 或进程名",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				name := c.Name()
				if name == "killall5" {
					return ActionDeny, "killall5"
				}
				if name == "kill" {
					targets, _ := c.Targets()
					for _, t := range targets {
						if t == "-1" {
							return ActionDeny, "kill -1（杀掉所有可杀进程）"
						}
					}
					// -9 -1 会被当成选项，单独兜一层。
					for _, a := range c.Args() {
						if a == "-1" {
							return ActionDeny, "kill -1（杀掉所有可杀进程）"
						}
					}
				}
				if in(name, "pkill", "killall") {
					targets, _ := c.Targets()
					for _, t := range targets {
						if in(t, "systemd", "init", "sshd", "dockerd", "containerd", "kubelet") {
							return ActionConfirm, "目标进程: " + t
						}
						for _, self := range g.selfNames {
							if self != "" && strings.Contains(t, self) {
								return ActionConfirm, "目标进程是本 MCP 服务自身: " + t
							}
						}
					}
					if c.HasFlag('u', "user") || len(targets) == 0 {
						return ActionConfirm, "按用户批量杀进程"
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "service-stop-self",
			reason: "停用可能承载本服务的系统单元或容器",
			hint:   "停掉之后这个 MCP 连接就没了，需要你手动去机器上拉起来",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				name := c.Name()
				if !in(name, "systemctl", "service", "docker", "podman", "nerdctl") {
					return ActionAllow, ""
				}
				verb := ""
				for _, a := range c.Args() {
					if in(a, "stop", "disable", "mask", "kill", "rm", "down", "restart") {
						verb = a
						break
					}
				}
				if verb == "" {
					return ActionAllow, ""
				}
				for _, a := range c.Args() {
					for _, self := range g.selfNames {
						if self != "" && strings.Contains(a, self) {
							return ActionConfirm, name + " " + verb + " " + a
						}
					}
					if in(a, "sshd", "ssh", "docker", "containerd", "nginx", "cloudflared", "frpc", "frps") {
						return ActionConfirm, name + " " + verb + " " + a
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "kernel-module",
			reason: "卸载内核模块",
			hint:   "卸载正在被使用的模块（binder、overlay、网卡驱动等）会直接把机器搞挂",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				switch c.Name() {
				case "rmmod":
					return ActionConfirm, "rmmod " + strings.Join(c.Args(), " ")
				case "modprobe":
					if c.HasFlag('r', "remove") {
						return ActionConfirm, "modprobe -r"
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "read-credentials",
			reason: "读取凭据类文件",
			hint: "私钥、云凭据、gh token 一旦进入对话就等于泄露给了模型服务方；" +
				"如果只是要用密钥（git push、ssh），直接执行命令即可，不需要 cat 出来",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				readers := in(c.Name(), "cat", "bat", "less", "more", "head", "tail", "nl",
					"base64", "xxd", "od", "strings", "openssl", "gpg", "cp", "scp", "rsync",
					"tar", "zip", "curl", "wget", "python3", "python")
				if !readers {
					return ActionAllow, ""
				}
				targets, _ := c.Targets()
				for _, t := range targets {
					if hit, ok := g.matchSecret(t); ok {
						return ActionConfirm, "涉及 " + t + "（命中 " + hit + "）"
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "crontab-wipe",
			reason: "清空 crontab",
			hint:   "crontab -r 会删掉当前用户全部定时任务且不可恢复；先 crontab -l 备份",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if c.Name() == "crontab" && c.HasFlag('r', "remove") {
					return ActionConfirm, "crontab -r"
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "container-prune",
			reason: "批量清理容器资源",
			hint:   "prune 带 --volumes / -a 会连带删掉其他项目的数据卷和镜像",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if !in(c.Name(), "docker", "podman", "nerdctl") {
					return ActionAllow, ""
				}
				args := c.Args()
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "prune") {
					if c.HasFlag('a', "all") || strings.Contains(joined, "--volumes") ||
						strings.Contains(joined, "volume prune") || strings.Contains(joined, "system prune") {
						return ActionConfirm, "docker " + joined
					}
				}
				if strings.Contains(joined, "volume rm") {
					return ActionConfirm, "删除数据卷: " + joined
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "move-system-path",
			reason: "移动或覆盖系统目录",
			hint:   "把 /etc、/usr 之类挪走等价于删除；请确认源和目标都对",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if !in(c.Name(), "mv", "cp", "rsync", "install", "ln") {
					return ActionAllow, ""
				}
				targets, unres := c.Targets()
				if len(targets) < 2 {
					return ActionAllow, ""
				}
				// 源与目标都看，覆盖 /etc 和搬走 /etc 都危险。
				act, hit := worstTarget(targets, unres, g.home)
				if act == ActionAllow {
					return ActionAllow, ""
				}
				if act == ActionDeny {
					act = ActionConfirm
				}
				return act, "涉及路径: " + hit
			},
		},
		{
			id:     "log-truncate",
			reason: "清空系统日志",
			hint:   "删日志会让事后排查无从下手；确认这是运维需要而不是掩盖问题",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				name := c.Name()
				if in(name, "truncate", "shred", "logrotate") || (name == "journalctl" &&
					strings.Contains(strings.Join(c.Args(), " "), "--vacuum")) {
					targets, _ := c.Targets()
					if name == "journalctl" {
						return ActionConfirm, "journalctl --vacuum"
					}
					for _, t := range targets {
						if strings.HasPrefix(cleanTarget(t, g.home), "/var/log") {
							return ActionConfirm, "目标: " + t
						}
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "find-delete",
			reason: "find 批量删除",
			hint:   "先去掉 -delete/-exec 跑一遍确认命中的文件列表，再执行删除",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if c.Name() != "find" {
					return ActionAllow, ""
				}
				args := c.Args()
				destructive := false
				for i, a := range args {
					if a == "-delete" {
						destructive = true
					}
					if (a == "-exec" || a == "-execdir" || a == "-ok") && i+1 < len(args) &&
						in(baseName(args[i+1]), "rm", "shred", "unlink", "truncate", "dd") {
						destructive = true
					}
				}
				if !destructive {
					return ActionAllow, ""
				}
				// 有 -name / -mtime 之类收窄条件时，命中范围可控，只有搜索根本身
				// 就危险（/、/etc、家目录）才拦；完全没有筛选条件则一律确认。
				narrowed := false
				for _, a := range args {
					if in(a, "-name", "-iname", "-path", "-ipath", "-regex", "-iregex",
						"-mtime", "-mmin", "-newer", "-size", "-samefile", "-links") {
						narrowed = true
						break
					}
				}
				targets, unres := c.Targets()
				root, rootUnres := ".", false
				if len(targets) > 0 {
					root, rootUnres = targets[0], unres[0]
				}
				rootRisk := worstOne(root, rootUnres, g.home)
				if narrowed {
					if rootRisk == ActionDeny {
						return ActionConfirm, "搜索根: " + root
					}
					return ActionAllow, ""
				}
				if rootRisk != ActionAllow {
					return ActionConfirm, "搜索根: " + root
				}
				return ActionConfirm, "find 无筛选条件批量删除"
			},
		},
		{
			id:     "user-management",
			reason: "改动系统账号",
			hint:   "锁掉或删掉账号可能让你再也登不上这台机器",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				name := c.Name()
				if in(name, "userdel", "deluser", "groupdel", "chpasswd", "vipw", "vigr") {
					return ActionConfirm, name
				}
				if name == "usermod" && (c.HasFlag('L', "lock") || c.HasFlag('e', "expiredate")) {
					return ActionConfirm, "usermod 锁定账号"
				}
				if name == "passwd" {
					targets, _ := c.Targets()
					if len(targets) > 0 {
						return ActionConfirm, "修改 " + targets[0] + " 的密码"
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "mount-remount",
			reason: "改动挂载或交换空间",
			hint:   "把根挂成只读、或 umount -a 会让系统立刻不可用",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				switch c.Name() {
				case "umount":
					if c.HasFlag('a', "all") {
						return ActionConfirm, "umount -a"
					}
					targets, _ := c.Targets()
					for _, t := range targets {
						if cleanTarget(t, g.home) == "/" {
							return ActionConfirm, "umount /"
						}
					}
				case "mount":
					joined := strings.Join(c.Args(), " ")
					if strings.Contains(joined, "remount") {
						return ActionConfirm, "mount remount: " + joined
					}
				case "swapoff":
					if c.HasFlag('a', "all") {
						return ActionConfirm, "swapoff -a"
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "eval-dynamic",
			reason: "执行内容无法静态确定的动态命令",
			hint:   "把要执行的命令直接写出来，拦截规则才能真正起作用",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				if !in(c.Name(), "eval", "source", ".") {
					return ActionAllow, ""
				}
				_, unres := c.Effective()
				for i, u := range unres {
					if i > 0 && u {
						return ActionConfirm, "动态求值: " + c.Text
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "redirect-danger",
			reason: "重定向写入设备或关键文件",
			hint:   "确认 > 右边的目标；写块设备等于抹盘，写 /etc/passwd 会破坏登录",
			match: func(c *Cmd, g *ShellGuard) (Action, string) {
				for _, r := range c.Redirects {
					if !strings.HasPrefix(r.Op, ">") {
						continue
					}
					if isBlockDevice(r.Target) {
						return ActionDeny, "重定向到块设备 " + r.Target
					}
					if isCriticalFile(r.Target, g.home) {
						return ActionConfirm, "重定向覆盖 " + r.Target
					}
					if strings.HasPrefix(cleanTarget(r.Target, g.home), "/var/log/") && r.Op == ">" {
						return ActionConfirm, "清空日志 " + r.Target
					}
				}
				// tee 也是一种写入。
				if c.Name() == "tee" {
					targets, _ := c.Targets()
					for _, t := range targets {
						if isBlockDevice(t) {
							return ActionDeny, "tee 写入块设备 " + t
						}
						if isCriticalFile(t, g.home) {
							return ActionConfirm, "tee 覆盖 " + t
						}
					}
				}
				return ActionAllow, ""
			},
		},
	}
}

func worstOne(target string, unresolved bool, home string) Action {
	act := classifyPath(target, home)
	if unresolved && riskyUnresolved(target) {
		act = worse(act, ActionConfirm)
	}
	return act
}

func builtinPipeRules() []pipeRule {
	return []pipeRule{
		{
			id:     "remote-pipe-shell",
			reason: "把网络下载的内容直接交给解释器执行",
			hint:   "先 curl -o /tmp/x.sh 落盘、看一眼内容再执行；管道直跑等于闭眼运行陌生代码",
			match: func(p *Pipeline, g *ShellGuard) (Action, string) {
				fetchers := map[string]bool{"curl": true, "wget": true, "fetch": true, "aria2c": true, "http": true, "httpie": true}
				shells := map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
					"python": true, "python3": true, "perl": true, "ruby": true, "node": true, "php": true}
				fetchIdx := -1
				for i, c := range p.Cmds {
					if fetchers[c.Name()] {
						fetchIdx = i
						break
					}
				}
				if fetchIdx < 0 {
					return ActionAllow, ""
				}
				for _, c := range p.Cmds[fetchIdx+1:] {
					if shells[c.Name()] {
						return ActionConfirm, p.Cmds[fetchIdx].Text + " | … | " + c.Name()
					}
				}
				return ActionAllow, ""
			},
		},
		{
			id:     "yes-pipe-destructive",
			reason: "用 yes 绕过删除/格式化的交互确认",
			hint:   "自动应答会让本来能救你一次的提示失效",
			match: func(p *Pipeline, g *ShellGuard) (Action, string) {
				if len(p.Cmds) < 2 || p.Cmds[0].Name() != "yes" {
					return ActionAllow, ""
				}
				for _, c := range p.Cmds[1:] {
					if in(c.Name(), "rm", "shred") || hasPrefixAny(c.Name(), "mkfs") {
						return ActionConfirm, "yes | " + c.Text
					}
				}
				return ActionAllow, ""
			},
		},
	}
}

func builtinTextRules() []textRule {
	return []textRule{
		{
			id:     "fork-bomb",
			reason: "fork 炸弹",
			hint:   "这条命令唯一的作用就是打满进程表，没有正当用途",
			action: ActionDeny,
			match: func(raw string) bool {
				compact := strings.Join(strings.Fields(raw), "")
				return strings.Contains(compact, ":(){:|:&};:") ||
					strings.Contains(compact, ":(){:|:&};") ||
					strings.Contains(compact, "(){$0|$0&};$0")
			},
		},
		{
			id:     "history-tamper",
			reason: "清除 shell 历史",
			hint:   "这台机器上的操作记录对排障有用；确认确实需要清",
			action: ActionConfirm,
			match: func(raw string) bool {
				compact := strings.Join(strings.Fields(raw), " ")
				return strings.Contains(compact, "history -c") ||
					strings.Contains(compact, "unset HISTFILE") ||
					strings.Contains(compact, "HISTFILE=/dev/null")
			},
		},
	}
}
