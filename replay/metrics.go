package replay

import "sync/atomic"

// Metrics 匹配命中/未命中统计。
type Metrics struct {
	hits   atomic.Int64
	misses atomic.Int64
}

// Hits 返回命中次数。
func (m *Metrics) Hits() int64 { return m.hits.Load() }

// Misses 返回未命中次数。
func (m *Metrics) Misses() int64 { return m.misses.Load() }

func (m *Metrics) incHit()  { m.hits.Add(1) }
func (m *Metrics) incMiss() { m.misses.Add(1) }
