// agent-tools-mcp 把一台 Linux 机器的基础操作能力（shell、文件读写、检索）
// 通过 MCP over Streamable HTTP 暴露出去，自带 OAuth 2.1 授权服务器，
// 可直接作为 ChatGPT 的自定义连接器使用。
//
// Copyright (C) 2026 noir017
// 本程序依 GNU Affero General Public License v3 或更新版本授权，详见 LICENSE。
// 因为它是网络服务，AGPL 第 13 条要求：修改后对外提供服务时，必须让使用者
// 能拿到对应源码——服务根页面的源码链接由 server.source_url 控制。
//
// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/noir017/agent-tools-mcp/internal/app"
	"github.com/noir017/agent-tools-mcp/internal/config"
	"github.com/noir017/agent-tools-mcp/internal/idgen"
	"github.com/noir017/agent-tools-mcp/internal/policy"
)

const usage = `agent-tools-mcp —— 给 agent 用的服务器基础工具 MCP 服务

用法：
  agent-tools-mcp serve [-c 配置文件]      启动服务（默认子命令）
  agent-tools-mcp hash-password [密码]     生成 bcrypt 密码哈希，填到 auth.users
  agent-tools-mcp gen-token                生成一个随机静态令牌，填到 auth.static_tokens
  agent-tools-mcp check-policy '命令'      查看某条 shell 命令会被策略如何处理
  agent-tools-mcp version                  打印版本与许可信息

环境变量（覆盖配置文件）：
  ATM_LISTEN, ATM_BASE_URL, ATM_DATA_DIR, ATM_USER, ATM_PASSWORD_HASH,
  ATM_STATIC_TOKEN, ATM_AUTH_DISABLED, ATM_LOG_LEVEL, ATM_LOG_FORMAT
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return cmdServe(args)
	case "hash-password":
		return cmdHashPassword(args)
	case "gen-token":
		fmt.Println(idgen.Secret(32))
		return nil
	case "check-policy":
		return cmdCheckPolicy(args)
	case "version":
		fmt.Printf("agent-tools-mcp %s\n", app.Version)
		fmt.Println("Copyright (C) 2026 noir017")
		fmt.Println("License AGPL-3.0-or-later: GNU AGPL version 3 or later <https://gnu.org/licenses/agpl.html>")
		fmt.Println("源码: " + config.DefaultSourceURL)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("未知子命令 %q", cmd)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("c", "", "配置文件路径（YAML）；不指定则全部用默认值加环境变量")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		if p := os.Getenv("ATM_CONFIG"); p != "" {
			*cfgPath = p
		} else if _, err := os.Stat("/etc/agent-tools-mcp/config.yaml"); err == nil {
			*cfgPath = "/etc/agent-tools-mcp/config.yaml"
		}
	}
	return app.Run(*cfgPath)
}

func cmdHashPassword(args []string) error {
	var pw string
	switch {
	case len(args) > 0:
		pw = args[0]
	case !term.IsTerminal(int(os.Stdin.Fd())):
		var line string
		if _, err := fmt.Scanln(&line); err != nil {
			return fmt.Errorf("从标准输入读取密码失败: %w", err)
		}
		pw = line
	default:
		fmt.Fprint(os.Stderr, "请输入密码（不回显）: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "再输入一次: ")
		b2, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		if string(b) != string(b2) {
			return errors.New("两次输入不一致")
		}
		pw = string(b)
	}
	if len(pw) < 8 {
		return errors.New("密码至少 8 位；这个口令等于服务器 shell 权限，别图省事")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}

func cmdCheckPolicy(args []string) error {
	fs := flag.NewFlagSet("check-policy", flag.ContinueOnError)
	cfgPath := fs.String("c", "", "配置文件路径，用于加载自定义规则")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("用法: agent-tools-mcp check-policy '要检查的命令'")
	}
	command := strings.Join(rest, " ")

	// 只为加载策略而读配置：这里不需要用户和令牌，临时关掉鉴权校验。
	cfg := config.Default()
	if *cfgPath != "" {
		loaded, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		cfg = loaded
	}
	guard, err := policy.NewShellGuard(cfg.Policy, []string{"agent-tools-mcp"})
	if err != nil {
		return err
	}
	v := guard.Inspect(command)
	fmt.Printf("命令: %s\n判定: %s\n", command, v.Action)
	if v.ParseError != "" {
		fmt.Printf("语法解析失败: %s\n", v.ParseError)
	}
	if len(v.Findings) == 0 {
		fmt.Println("\n没有命中任何规则，会直接执行。")
		return nil
	}
	fmt.Println(v.Explain())
	if v.Action == policy.ActionDeny {
		os.Exit(2)
	}
	if v.Action == policy.ActionConfirm {
		os.Exit(3)
	}
	return nil
}
