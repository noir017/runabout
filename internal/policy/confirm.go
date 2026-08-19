package policy

import (
	"fmt"
	"sync"
	"time"

	"github.com/noir017/runabout/internal/idgen"
)

// ConfirmStore 保管危险操作的一次性确认令牌。
//
// ChatGPT 侧不支持 MCP elicitation，没法弹确认框，所以做法是：命中 confirm
// 级规则时返回一个绑定到"这条具体命令"的令牌，agent 带着令牌重发同一条命令
// 才会真正执行。令牌一次性、限时、且与命令指纹强绑定，换命令不复用。
type ConfirmStore struct {
	ttl time.Duration

	mu      sync.Mutex
	pending map[string]*pendingConfirm
}

type pendingConfirm struct {
	fingerprint string
	subject     string
	ruleID      string
	expiresAt   time.Time
}

func NewConfirmStore(ttl time.Duration) *ConfirmStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &ConfirmStore{ttl: ttl, pending: map[string]*pendingConfirm{}}
}

// Issue 为一条命令签发确认令牌。
func (s *ConfirmStore) Issue(fingerprint, subject, ruleID string) (token string, expiresIn time.Duration) {
	token = idgen.Secret(18)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.pending[token] = &pendingConfirm{
		fingerprint: fingerprint,
		subject:     subject,
		ruleID:      ruleID,
		expiresAt:   time.Now().Add(s.ttl),
	}
	return token, s.ttl
}

// Redeem 校验并消耗令牌。指纹不符、过期或已用过都会失败。
func (s *ConfirmStore) Redeem(token, fingerprint, subject string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	p, ok := s.pending[token]
	if !ok {
		return fmt.Errorf("confirm_token 无效或已被使用过；请重新发起一次调用以获取新令牌")
	}
	delete(s.pending, token)
	if time.Now().After(p.expiresAt) {
		return fmt.Errorf("confirm_token 已过期（有效期 %s）；请重新发起调用获取新令牌", s.ttl)
	}
	if p.fingerprint != fingerprint {
		return fmt.Errorf("confirm_token 与当前命令不匹配：令牌只对签发它的那条命令有效，" +
			"命令内容或工作目录有任何改动都需要重新确认")
	}
	if p.subject != "" && subject != "" && p.subject != subject {
		return fmt.Errorf("confirm_token 属于其他用户")
	}
	return nil
}

// Pending 返回当前未消耗的令牌数，供 /healthz 之类观测。
func (s *ConfirmStore) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	return len(s.pending)
}

func (s *ConfirmStore) gcLocked() {
	now := time.Now()
	for t, p := range s.pending {
		if now.After(p.expiresAt) {
			delete(s.pending, t)
		}
	}
}
