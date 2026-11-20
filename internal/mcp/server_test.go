package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// TestServerInstructionsEvaluatedPerInitialize 验证 instructions 不是构造时定死的，
// 每次 initialize 都会重新取值（上游可能刚连上）。
func TestServerInstructionsEvaluatedPerInitialize(t *testing.T) {
	calls := 0
	s := NewServer(Implementation{Name: "test", Version: "1"}, func() string {
		calls++
		return fmt.Sprintf("说明 %d", calls)
	}, nil)

	initialize := func() string {
		resp := s.Dispatch(context.Background(), "", "", &Message{
			JSONRPC: "2.0",
			ID:      json.RawMessage("1"),
			Method:  "initialize",
			Params:  json.RawMessage(`{"protocolVersion":"2025-06-18"}`),
		})
		if resp == nil || resp.Error != nil {
			t.Fatalf("initialize 失败: %+v", resp)
		}
		res, ok := resp.Result.(InitializeResult)
		if !ok {
			t.Fatalf("返回类型不对: %T", resp.Result)
		}
		return res.Instructions
	}

	if got := initialize(); got != "说明 1" {
		t.Errorf("第一次 initialize 应取新值，实际 %q", got)
	}
	if got := initialize(); got != "说明 2" {
		t.Errorf("第二次 initialize 应重新取值，实际 %q", got)
	}
}

func TestServerNilInstructions(t *testing.T) {
	s := NewServer(Implementation{Name: "test", Version: "1"}, nil, nil)
	resp := s.Dispatch(context.Background(), "", "", &Message{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-06-18"}`),
	})
	if resp == nil || resp.Error != nil {
		t.Fatalf("initialize 失败: %+v", resp)
	}
	if got := resp.Result.(InitializeResult).Instructions; got != "" {
		t.Errorf("没有 provider 时说明应为空，实际 %q", got)
	}
}
