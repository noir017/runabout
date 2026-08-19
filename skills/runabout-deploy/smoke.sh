#!/usr/bin/env bash
# runabout 部署后自检：把一次真实客户端接入会走的每一步都验一遍。
#
# 用法：
#   ./smoke.sh https://mcp.example.com              # 只验免鉴权能验的部分
#   ./smoke.sh https://mcp.example.com <静态令牌>    # 连 /mcp 握手一起验
#
# 只依赖 curl / grep / sed，不需要 jq —— 部署现场往往没装。
# 退出码：0 全通过，1 有失败项。

set -uo pipefail

BASE=${1:-}
TOKEN=${2:-}

if [ -z "$BASE" ]; then
  echo "用法: $0 <base_url> [静态令牌]" >&2
  echo "  base_url 必须与配置里的 server.base_url 逐字一致（含 scheme，无尾斜杠）" >&2
  exit 2
fi
BASE=${BASE%/}

PASS=0
FAIL=0
CURL="curl -sS --max-time 25"

ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; FAIL=$((FAIL+1)); }
note() { printf '        %s\n' "$1"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# ---------- 1. 存活 ----------
head_ "1. 服务存活"
body=$($CURL -w '\n%{http_code}' "$BASE/healthz" 2>/dev/null)
code=$(printf '%s' "$body" | tail -n1)
json=$(printf '%s' "$body" | sed '$d')
if [ "$code" = "200" ]; then
  ok "GET /healthz -> 200"
  case "$json" in
    *'"status":"ok"'*) ok 'status=ok' ;;
    *) bad "status 不是 ok: $json" ;;
  esac
  case "$json" in
    *'"auth":true'*)  ok '鉴权已启用' ;;
    *'"auth":false'*) bad '鉴权是关闭的！只有本机调试才允许，公网暴露等于把 shell 拱手让人' ;;
  esac
else
  bad "GET /healthz -> $code（服务没起来，或 base_url/入口不对）"
  note "先看 docker logs runabout（或 journalctl -u runabout）"
fi

# ---------- 2. 受保护资源元数据 (RFC 9728) ----------
head_ "2. 受保护资源元数据"
resp=$($CURL -D - -o /tmp/.rb_pr.$$ "$BASE/.well-known/oauth-protected-resource" 2>/dev/null)
code=$(printf '%s' "$resp" | sed -n '1s@.*[[:space:]]\([0-9][0-9][0-9]\).*@\1@p' | head -1)
meta=$(cat /tmp/.rb_pr.$$ 2>/dev/null); rm -f /tmp/.rb_pr.$$
if [ "$code" = "200" ]; then
  ok "GET /.well-known/oauth-protected-resource -> 200"
else
  bad "GET /.well-known/oauth-protected-resource -> ${code:-无响应}"
fi
# resource 必须等于 base_url + /mcp。不一致说明 base_url 配错了，
# 客户端会拿着对不上的 resource 去换令牌。
case "$meta" in
  *"\"resource\": \"$BASE/mcp\""*|*"\"resource\":\"$BASE/mcp\""*)
    ok "resource = $BASE/mcp" ;;
  *)
    bad "resource 与你传入的 base_url 不一致"
    note "元数据里是: $(printf '%s' "$meta" | tr ',' '\n' | grep -i '"resource"' | head -1)"
    note "改 RB_BASE_URL / server.base_url，两边必须逐字相同（注意尾斜杠）" ;;
esac
if printf '%s' "$resp" | grep -qi '^access-control-allow-origin'; then
  ok "带 CORS 头（浏览器侧客户端能做发现）"
else
  bad "缺 Access-Control-Allow-Origin：浏览器里的 MCP 客户端会发现失败"
fi

# ---------- 3. 授权服务器元数据 (RFC 8414) ----------
head_ "3. 授权服务器元数据"
as=$($CURL "$BASE/.well-known/oauth-authorization-server" 2>/dev/null)
for field in issuer authorization_endpoint token_endpoint registration_endpoint code_challenge_methods_supported; do
  case "$as" in
    *"\"$field\""*) ok "含 $field" ;;
    *) bad "缺 $field" ;;
  esac
done
case "$as" in
  *'"S256"'*) ok 'PKCE S256 已通告' ;;
  *) bad '未通告 S256，MCP 客户端会拒绝' ;;
esac
case "$as" in
  *"\"issuer\": \"$BASE\""*|*"\"issuer\":\"$BASE\""*) ok "issuer = $BASE" ;;
  *) bad "issuer 与 base_url 不一致" ;;
esac

# ---------- 4. 未授权挑战 ----------
head_ "4. 未授权时的 401 挑战"
resp=$($CURL -D - -o /dev/null -X POST "$BASE/mcp" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' 2>/dev/null)
code=$(printf '%s' "$resp" | sed -n '1s@.*[[:space:]]\([0-9][0-9][0-9]\).*@\1@p' | head -1)
if [ "$code" = "401" ]; then
  ok "POST /mcp 无令牌 -> 401"
else
  bad "POST /mcp 无令牌 -> ${code:-无响应}（应为 401）"
fi
# 这个头是 MCP 客户端发现授权服务器的唯一入口，少了它客户端只会显示
# 一个没有下文的连接失败。注意 Go 会把头名规范化成 Www-Authenticate，
# 用 grep -i 匹配。
if printf '%s' "$resp" | grep -i '^www-authenticate' | grep -q 'resource_metadata='; then
  ok 'WWW-Authenticate 带 resource_metadata'
else
  bad 'WWW-Authenticate 缺 resource_metadata：客户端无法自动发现授权服务器'
fi

# ---------- 5. CORS 预检 ----------
head_ "5. CORS 预检（必须绕过鉴权）"
# 浏览器规范不允许预检携带 Authorization。若这里返回 401，
# 浏览器侧的客户端只会报一句没有下文的"意外错误"。
resp=$($CURL -D - -o /dev/null -X OPTIONS "$BASE/mcp" \
  -H 'Origin: https://chatgpt.com' \
  -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: content-type,authorization' 2>/dev/null)
code=$(printf '%s' "$resp" | sed -n '1s@.*[[:space:]]\([0-9][0-9][0-9]\).*@\1@p' | head -1)
case "$code" in
  204|200) ok "OPTIONS /mcp -> $code" ;;
  401) bad 'OPTIONS /mcp -> 401：预检被鉴权拦了（预检不可能带 Authorization）' ;;
  403) bad 'OPTIONS /mcp -> 403：Origin 不在 server.allowed_origins 白名单里' ;;
  *)   bad "OPTIONS /mcp -> ${code:-无响应}" ;;
esac
if printf '%s' "$resp" | grep -qi '^access-control-allow-headers.*[Aa]uthorization'; then
  ok '预检允许 Authorization 头'
else
  bad '预检未允许 Authorization 头'
fi

# ---------- 6. 带令牌的真实握手 ----------
head_ "6. MCP 握手"
if [ -z "$TOKEN" ]; then
  note "跳过：没给静态令牌"
  note "生成一个：runabout gen-token，填到配置的 auth.static_tokens"
else
  resp=$($CURL -D /tmp/.rb_h.$$ -o /tmp/.rb_b.$$ -X POST "$BASE/mcp" \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -H 'MCP-Protocol-Version: 2025-06-18' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}' 2>/dev/null; echo)
  hdr=$(cat /tmp/.rb_h.$$ 2>/dev/null); bdy=$(cat /tmp/.rb_b.$$ 2>/dev/null)
  code=$(printf '%s' "$hdr" | sed -n '1s@.*[[:space:]]\([0-9][0-9][0-9]\).*@\1@p' | head -1)
  sid=$(printf '%s' "$hdr" | grep -i '^mcp-session-id' | sed -E 's/.*:[[:space:]]*//' | tr -d '\r')
  rm -f /tmp/.rb_h.$$ /tmp/.rb_b.$$
  if [ "$code" = "200" ]; then
    ok "initialize -> 200"
  else
    bad "initialize -> ${code:-无响应}"
    note "401 = 令牌不对；403 = Origin 白名单；其它看服务端日志"
  fi
  case "$bdy" in
    *'"serverInfo"'*) ok "服务端应答含 serverInfo" ;;
    *) bad "应答不含 serverInfo: $(printf '%s' "$bdy" | head -c 200)" ;;
  esac
  [ -n "$sid" ] && ok "拿到会话 id" || bad "响应缺 Mcp-Session-Id 头"

  # 列工具：真正证明工具注册成功
  if [ -n "$sid" ]; then
    tl=$($CURL -X POST "$BASE/mcp" \
      -H "Authorization: Bearer $TOKEN" \
      -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' \
      -H "Mcp-Session-Id: $sid" \
      -H 'MCP-Protocol-Version: 2025-06-18' \
      -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' 2>/dev/null)
    n=$(printf '%s' "$tl" | grep -o '"name":' | wc -l | tr -d ' ')
    if [ "${n:-0}" -gt 0 ]; then
      ok "tools/list 返回 $n 个工具"
    else
      bad "tools/list 没列出工具: $(printf '%s' "$tl" | head -c 200)"
    fi
    # 收尾：显式终止会话，别在服务端留悬挂会话
    $CURL -o /dev/null -X DELETE "$BASE/mcp" \
      -H "Authorization: Bearer $TOKEN" -H "Mcp-Session-Id: $sid" 2>/dev/null || true
  fi
fi

# ---------- 汇总 ----------
printf '\n\033[1m结果\033[0m  通过 %d，失败 %d\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf '排障入手点：把日志级别调到 debug（RB_LOG_LEVEL=debug）再复现一次，\n'
  printf '否则 4xx 只写在 Debug 级，info 下什么都看不到。\n'
  exit 1
fi
exit 0
