package configure

import (
	"github.com/v2rayA/v2rayA/db"
)

// NodeHealth 是单个节点的健康状态缓存。
//
// 它回答两个问题，且只回答这两个：
//   1. 这个节点现在能不能用？(Alive)
//   2. 它上一次被测出来是什么时候、失败了几次？(CheckedAt / Fails)
//
// 设计约束：
//   - 持久化：写入 system_config 的 "nodehealth" bucket，重启后仍在。
//   - 内存态：由 NodeHealthCache 持有，避免每次决策都读库。
//   - 只缓存"事实"，不缓存"决策"。决策由调用方在每次需要时重新算，
//     这样修改阈值不需要清理缓存。
type NodeHealth struct {
	// Alive 是最近一次探测的结论。只有一次显式探测过后才有意义，
	// 从未探测过的节点是 Unknown，不是 Dead。
	Alive bool `json:"alive"`
	// Fails 是连续失败次数。成功一次即归零，用于实现"连续 N 次失败"语义。
	Fails int `json:"fails"`
	// LatencyMs 是最近一次成功探测的往返毫秒数；失败时保留上一次成功
	// 的值，用于在多个可用节点之间挑较快的那个。0 表示没有记录。
	LatencyMs int64 `json:"latency_ms,omitempty"`
	// CheckedAt 是最近一次探测的 Unix 秒。0 表示从未探测。
	CheckedAt int64 `json:"checked_at,omitempty"`
	// LastError 是最近一次失败的原因摘要，仅用于向用户解释。
	LastError string `json:"last_error,omitempty"`
}

// NodeHealthStore 是一组节点的健康状态，按节点引用字符串索引。
//
// key 由 NodeRef.Key() 生成，对 server / subscriptionServer 都唯一。
type NodeHealthStore map[string]NodeHealth

const nodeHealthBucket = "nodehealth"

// NodeHealthTTL 是健康记录的默认有效期。超过这个时间没有新探测的记录
// 视为过期，调用方应重新探测而不是直接信任。默认 10 分钟。
const NodeHealthTTL = 10 * 60

// GetNodeHealthStore 读取全部持久化的节点健康状态。
// 数据库不可用时返回空 store 而不是错误，让调用方退化为"全部未知"。
func GetNodeHealthStore() (store NodeHealthStore) {
	store = make(NodeHealthStore)
	if err := db.Get(nodeHealthBucket, "state", &store); err != nil {
		// key 不存在或解析失败都当作空 store：未知不等于不可用。
		return make(NodeHealthStore)
	}
	return store
}

// SaveNodeHealthStore 覆盖写入全部节点健康状态。
func SaveNodeHealthStore(store NodeHealthStore) error {
	if store == nil {
		store = make(NodeHealthStore)
	}
	return db.Set(nodeHealthBucket, "state", store)
}

// GetNodeHealth 返回单个节点的健康状态，未探测过则返回零值。
func GetNodeHealth(key string) NodeHealth {
	return GetNodeHealthStore()[key]
}

// Fresh 报告这条记录是否仍然新鲜（未过期）。
// 从未探测过的记录不新鲜。
func (h NodeHealth) Fresh(now int64) bool {
	if h.CheckedAt == 0 {
		return false
	}
	return now-h.CheckedAt <= NodeHealthTTL
}
