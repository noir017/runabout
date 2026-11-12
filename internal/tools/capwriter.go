package tools

import "github.com/noir017/runabout/internal/fleet"

// capWriter 保留旧名字，内部实现已挪到 fleet，供 shell 后台任务与搜索共用。
type capWriter = fleet.CapWriter

func newCapWriter(limit int) *capWriter { return fleet.NewCapWriter(limit) }
