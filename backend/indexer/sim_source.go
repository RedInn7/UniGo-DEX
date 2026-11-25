package indexer

import (
	"context"
	"fmt"
	"time"
)

// sim_source.go — 模拟链数据源
//
// 未配置 RPC 节点（或节点连不上）时使用，保证后端可以独立启动。
// 每次轮询推进 2 个空区块，哈希由区块号生成，父哈希始终衔接，不会触发 Reorg。

type simSource struct {
	next uint64 // 下一次轮询的起始区块号
}

func simHash(number uint64) string {
	return fmt.Sprintf("0x%064x", number)
}

func (s *simSource) LatestBlock(ctx context.Context) (uint64, error) {
	s.next += 2
	return s.next - 1, nil
}

func (s *simSource) BlockByNumber(ctx context.Context, number uint64) (*BlockInfo, error) {
	block := &BlockInfo{Number: number, Hash: simHash(number), Timestamp: time.Now()}
	if number > 0 {
		block.ParentHash = simHash(number - 1)
	}
	return block, nil
}

func (s *simSource) BlockEvents(ctx context.Context, blocks []*BlockInfo) ([][]interface{}, error) {
	return make([][]interface{}, len(blocks)), nil
}
