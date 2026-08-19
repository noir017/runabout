package policy

import (
	"path/filepath"
	"strings"
)

// Action 是策略判定结果。
type Action string

const (
	ActionAllow   Action = "allow"
	ActionConfirm Action = "confirm"
	ActionDeny    Action = "deny"
)

func (a Action) severity() int {
	switch a {
	case ActionDeny:
		return 2
	case ActionConfirm:
		return 1
	default:
		return 0
	}
}

// worse 返回两个动作中更严格的那个。
func worse(a, b Action) Action {
	if b.severity() > a.severity() {
		return b
	}
	return a
}

// criticalDirs 是一旦被递归删除/改权限就基本等于毁掉系统的目录。
var criticalDirs = []string{
	"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/boot",
	"/proc", "/sys", "/dev", "/var",
}

// criticalFiles 是单个文件被覆盖/删除就会出大问题的目标。
var criticalFiles = []string{
	"/etc/passwd", "/etc/shadow", "/etc/group", "/etc/gshadow", "/etc/fstab",
	"/etc/sudoers", "/etc/hosts", "/etc/resolv.conf", "/etc/ssh/sshd_config",
	"/etc/nsswitch.conf", "/etc/crypttab",
}

// sensitiveParents 的直接子目录值得确认一次：/home/bob 是别人的整个家目录，
// /srv/app 往往是一个完整服务的全部数据。
var sensitiveParents = []string{"/home", "/root", "/srv", "/opt", "/mnt", "/media", "/data", "/backup"}

// blockDevPrefixes 匹配整块磁盘/分区设备，写进去等于抹盘。
var blockDevPrefixes = []string{
	"/dev/sd", "/dev/nvme", "/dev/vd", "/dev/hd", "/dev/xvd", "/dev/mmcblk",
	"/dev/dm-", "/dev/mapper/", "/dev/loop", "/dev/md", "/dev/sr", "/dev/nbd",
	"/dev/disk/",
}

// cleanTarget 归一化一个路径目标：展开 ~/$HOME、去掉尾部斜杠。
func cleanTarget(p, home string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = ExpandHome(p, home)
	// 先归一化 .. 与重复斜杠，"$HOME/.." 这类写法才不会漏判。
	if suffix := "/*"; strings.HasSuffix(p, suffix) {
		base := strings.TrimSuffix(p, suffix)
		if base == "" {
			base = "/"
		}
		if base = filepath.Clean(base); base == "/" {
			p = "/*"
		} else {
			p = base + suffix
		}
	} else {
		p = filepath.Clean(p)
	}
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// classifyPath 判断"对这个路径做破坏性操作"的风险等级。
//
// 判定阶梯：根目录/家目录/关键系统目录 → deny；关键目录的子目录、任意一级
// 目录、裸通配符与当前目录 → confirm；其余（项目内的具体路径）→ allow。
func classifyPath(raw, home string) Action {
	p := cleanTarget(raw, home)
	if p == "" {
		return ActionAllow
	}

	switch p {
	case "/", "/*", "/**", "/.", "/./*":
		return ActionDeny
	}
	if home != "" {
		switch p {
		case home, home + "/*", home + "/.*", home + "/**":
			return ActionDeny
		}
	}
	for _, d := range criticalDirs {
		if p == d || p == d+"/*" || p == d+"/**" {
			return ActionDeny
		}
	}
	for _, f := range criticalFiles {
		if p == f {
			return ActionConfirm
		}
	}
	for _, d := range criticalDirs {
		if strings.HasPrefix(p, d+"/") {
			return ActionConfirm
		}
	}
	// 裸通配符 / 当前目录 / 上级目录：等于"清空我现在所在的地方"。
	switch p {
	case "*", ".*", ".", "./", "./*", "..", "../", "../*", "-r":
		return ActionConfirm
	}
	if strings.HasPrefix(p, "/") {
		// 一级绝对路径（/data、/srv……）都值得确认一次。
		rest := strings.Trim(p, "/")
		if rest != "" && !strings.Contains(rest, "/") {
			return ActionConfirm
		}
		for _, parent := range sensitiveParents {
			if !strings.HasPrefix(p, parent+"/") {
				continue
			}
			// 恰好一层（/home/bob、/home/bob/*）确认；更深的具体路径放行。
			sub := strings.TrimPrefix(p, parent+"/")
			sub = strings.TrimSuffix(sub, "/*")
			if sub != "" && !strings.Contains(sub, "/") {
				return ActionConfirm
			}
		}
	}
	return ActionAllow
}

// isBlockDevice 报告路径是否指向块设备。
func isBlockDevice(p string) bool {
	p = filepath.Clean(p)
	for _, pre := range blockDevPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// isCriticalFile 报告路径是否为关键系统文件。
func isCriticalFile(p, home string) bool {
	p = cleanTarget(p, home)
	for _, f := range criticalFiles {
		if p == f {
			return true
		}
	}
	if home != "" && p == filepath.Join(home, ".ssh", "authorized_keys") {
		return true
	}
	return strings.HasPrefix(p, "/boot/")
}

// riskyUnresolved 判断一个"没解析出来"的目标是否值得警惕。
//
// "$DIR" 这种整条路径都来自变量的最危险；"$TMP/build" 后面还跟着字面段，
// 删掉的最多是某个子目录，放行以免噪音过大。
func riskyUnresolved(raw string) bool {
	if !strings.Contains(raw, "${") {
		return false
	}
	idx := strings.LastIndex(raw, "}")
	if idx < 0 {
		return true
	}
	tail := raw[idx+1:]
	trimmed := strings.Trim(tail, "/")
	return trimmed == "" || trimmed == "*"
}

// worstTarget 返回一批目标里最高的风险等级及对应目标。
func worstTarget(targets []string, unresolved []bool, home string) (Action, string) {
	act, hit := ActionAllow, ""
	for i, t := range targets {
		a := classifyPath(t, home)
		if i < len(unresolved) && unresolved[i] && riskyUnresolved(t) {
			a = worse(a, ActionConfirm)
		}
		if a.severity() > act.severity() {
			act, hit = a, t
		}
	}
	return act, hit
}
