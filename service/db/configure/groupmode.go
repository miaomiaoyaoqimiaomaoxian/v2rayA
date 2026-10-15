package configure

import (
	"fmt"

	"github.com/v2rayA/v2rayA/db"
)

// GroupMode 描述一个出站分组挑选节点的方式。
//
//   1. GroupModeAny（如 PROXY）：只要有一个节点能用就行。不关心是哪个。
//      v2rayA 把分组配成"单个节点"，该节点连续失败达到阈值后换下一个。
//
//   2. GroupModePinned（如 OPENAI / GMINI）：只允许一组特定节点，且通常
//      要求出口地区正确。这一组全灭时不是"换一个"，而是明确告诉用户
//      "所有节点均无法访问"——那是源头上的错误，换分组只会掩盖它。
type GroupMode string

const (
	// GroupModeAny 是默认值，等价于旧行为：分组内任意节点可用即可。
	GroupModeAny GroupMode = "any"
	// GroupModePinned 是固定分组：只用候选集合内的节点，全灭即报错。
	GroupModePinned GroupMode = "pinned"
)

// DefaultGroupMode 是新建分组的默认模式。
const DefaultGroupMode = GroupModeAny

// Normalize 把未知或空值收敛为合法模式。
// 数据库里可能出现历史脏值，这里统一兜底。
func (m GroupMode) Normalize() GroupMode {
	switch m {
	case GroupModeAny, GroupModePinned:
		return m
	default:
		return DefaultGroupMode
	}
}

// String 实现 fmt.Stringer，便于日志与错误信息。
func (m GroupMode) String() string {
	return string(m.Normalize())
}

// IsPinned 报告该模式是否要求固定候选集合。
func (m GroupMode) IsPinned() bool {
	return m.Normalize() == GroupModePinned
}

// GroupPolicy 是一个分组的自动化策略，随 OutboundSetting 持久化。
//
// 它被设计成 OutboundSetting 的可选扩展：旧字段（ProbeURL / ProbeInterval /
// Type / Selected）保持原义，新增字段全部有默认值，因此老数据库读出来
// 就是一个合法的 GroupPolicy。
type GroupPolicy struct {
	// Mode 决定挑选语义，见 GroupMode。
	Mode GroupMode `json:"mode,omitempty"`

	// FailThreshold 是"连续失败多少次后切换节点"的阈值，默认 5。
	// 只对 GroupModeAny 有意义；pinned 分组不切换节点。
	FailThreshold int `json:"failThreshold,omitempty"`

	// MaxSwitches 是连续切换次数上限，默认 5。
	// 达到上限后不再逐个换，而是触发一次全量重测并重选。
	// 只对 GroupModeAny 有意义。
	MaxSwitches int `json:"maxSwitches,omitempty"`

	// AutoTest 开启时，分组会在后台周期重测候选节点，保持缓存新鲜。
	// 关闭时只在切换路径上按需探测。
	AutoTest bool `json:"autoTest,omitempty"`
}

// DefaultGroupPolicy 返回内置默认策略。
func DefaultGroupPolicy() GroupPolicy {
	return GroupPolicy{
		Mode:         DefaultGroupMode,
		FailThreshold: DefaultFailThreshold,
		MaxSwitches:   DefaultMaxSwitches,
		AutoTest:      false,
	}
}

const (
	// DefaultFailThreshold 是默认连续失败阈值。
	DefaultFailThreshold = 5
	// DefaultMaxSwitches 是默认连续切换上限。
	DefaultMaxSwitches = 5
)

// FillEmpty 用默认值补齐零值字段，使从数据库读出的旧配置依然可用。
func (p *GroupPolicy) FillEmpty() {
	if p.Mode == "" {
		p.Mode = DefaultGroupMode
	}
	if p.FailThreshold <= 0 {
		p.FailThreshold = DefaultFailThreshold
	}
	if p.MaxSwitches <= 0 {
		p.MaxSwitches = DefaultMaxSwitches
	}
}

// GetGroupPolicy 读取指定分组的策略，缺省返回默认策略。
func GetGroupPolicy(outbound string) GroupPolicy {
	p := DefaultGroupPolicy()
	if err := db.Get(fmt.Sprintf("outbound.%v", outbound), "groupPolicy", &p); err != nil {
		return DefaultGroupPolicy()
	}
	p.FillEmpty()
	return p
}

// SetGroupPolicy 写入指定分组的策略。
func SetGroupPolicy(outbound string, policy GroupPolicy) error {
	policy.FillEmpty()
	return db.Set(fmt.Sprintf("outbound.%v", outbound), "groupPolicy", policy)
}
