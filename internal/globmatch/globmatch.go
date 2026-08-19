// Package globmatch 实现支持 ** 的路径通配匹配，语义与 gitignore / ripgrep 的
// --glob 大体一致：* 与 ? 不跨越路径分隔符，** 可以跨越任意层级。
package globmatch

import (
	"path"
	"path/filepath"
	"strings"
)

// Match 判断 name（斜杠分隔的路径）是否匹配 pattern。
//
//   - 匹配单层内任意字符
//     ?      匹配单层内一个字符
//     [a-z]  字符类，语义同 path.Match
//     **     匹配任意层级（含零层）
//     {a,b}  分支，任一命中即匹配
func Match(pattern, name string) bool {
	pattern = filepath.ToSlash(pattern)
	name = filepath.ToSlash(name)
	for _, p := range expandBraces(pattern) {
		if matchSegments(strings.Split(p, "/"), strings.Split(name, "/")) {
			return true
		}
	}
	return false
}

// MatchAny 判断 name 是否匹配任一 pattern，并返回命中的 pattern。
func MatchAny(patterns []string, name string) (string, bool) {
	for _, p := range patterns {
		if Match(p, name) {
			return p, true
		}
	}
	return "", false
}

func matchSegments(pat, seg []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// 尾部的 ** 吃掉剩余全部层级，但要求至少还剩一层：
			// 语义与 gitignore 一致，"/proc/**" 匹配 /proc 里的东西而不是 /proc 本身。
			if len(pat) == 1 {
				return len(seg) > 0
			}
			for i := 0; i <= len(seg); i++ {
				if matchSegments(pat[1:], seg[i:]) {
					return true
				}
			}
			return false
		}
		if len(seg) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], seg[0])
		if err != nil || !ok {
			return false
		}
		pat, seg = pat[1:], seg[1:]
	}
	return len(seg) == 0
}

// expandBraces 把 a{b,c}d 展开为 [abd acd]；不含花括号时返回原串。
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	depth, close := 0, -1
	for i := open; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				close = i
			}
		}
		if close >= 0 {
			break
		}
	}
	if close < 0 {
		return []string{pattern}
	}
	prefix, suffix := pattern[:open], pattern[close+1:]
	var out []string
	for _, alt := range splitTopLevel(pattern[open+1 : close]) {
		out = append(out, expandBraces(prefix+alt+suffix)...)
	}
	return out
}

func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
