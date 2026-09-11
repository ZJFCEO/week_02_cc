package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 五、审批、审计与脱敏
// ---------------------------------------------------------------------------

// canonicalJSON 把任意值压成"顺序稳定"的 JSON 字符串。
//
// 为什么要多绕一圈 unmarshal 再 marshal：
// Go 序列化 struct 时按字段声明顺序输出，序列化 map 时按 key 排序输出。
// 签发审批时传的可能是 map（来自演示/测试），核销时传的是 struct（解码后的参数），
// 两者直接序列化会得到不同的字符串，摘要自然对不上。
// 先统一转成 map[string]any 再序列化，就都按 key 排序了——这就是 Python 里 _stable_value 做的事。
func canonicalJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return string(raw)
	}
	stable, err := json.Marshal(normalized)
	if err != nil {
		return string(raw)
	}
	return string(stable)
}

// ApprovalDigest 是审批摘要 = sha256(工具名 + 规范化后的完整参数)。
// 这是整个审批机制的地基：改任何一个参数，摘要就变，之前那张审批立刻失效。
func ApprovalDigest(toolName string, arguments any) string {
	sum := sha256.Sum256([]byte(toolName + ":" + canonicalJSON(arguments)))
	return hex.EncodeToString(sum[:])
}

// approvalRecord 是一条审批凭证。Used 会在核销时被改写，所以这里存指针。
type approvalRecord struct {
	ApprovalID string
	UserID     string
	TenantID   string
	ToolName   string
	Digest     string
	ExpiresAt  time.Time
	Used       bool
}

// ApprovalStore 存放审批凭证。教学示例用内存 map，生产上应换成带 TTL 的 Redis 或数据库。
//
// 【Go 差异】Python 版没有锁，因为 asyncio 是单线程事件循环；
// Go 的 handler 可能被多个 goroutine 并发调用，这把锁是必需的。
type ApprovalStore struct {
	mu      sync.Mutex
	records map[string]*approvalRecord
}

// NewApprovalStore 创建一个空的审批库。
func NewApprovalStore() *ApprovalStore {
	return &ApprovalStore{records: make(map[string]*approvalRecord)}
}

// DefaultApprovalTTL 是审批凭证的默认有效期。
const DefaultApprovalTTL = 5 * time.Minute

// Approve 签发一张审批，把"谁、哪个租户、哪个工具、哪份参数、有效到什么时候"一次性钉死。
// 现实中这一步应该由人在 UI 上点"确认"触发，示例里由测试或演示直接调用。
func (s *ApprovalStore) Approve(approvalID string, ec ExecutionContext, toolName string, arguments any, ttl time.Duration) {
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[approvalID] = &approvalRecord{
		ApprovalID: approvalID,
		UserID:     ec.UserID,
		TenantID:   ec.TenantID,
		ToolName:   toolName,
		Digest:     ApprovalDigest(toolName, arguments),
		ExpiresAt:  time.Now().Add(ttl),
	}
}

// Consume 核销一张审批。六个条件全部成立才算数，缺一不可：
//
//  1. 凭证存在   2. 没被用过（一次性）  3. 没过期
//  4. 同一个人   5. 同一个租户         6. 同一个工具 + 同一份参数（摘要相等）
//
// 校验通过后立刻标记 Used，所以同一张审批重放第二次必然失败。
func (s *ApprovalStore) Consume(approvalID string, ec ExecutionContext, toolName string, arguments Args) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.records[approvalID]
	valid := ok &&
		!record.Used &&
		record.ExpiresAt.After(time.Now()) &&
		record.UserID == ec.UserID &&
		record.TenantID == ec.TenantID &&
		record.ToolName == toolName &&
		record.Digest == ApprovalDigest(toolName, arguments)

	if valid {
		// 核销即作废。这一行是"一次性"三个字的全部实现。
		record.Used = true
	}
	return valid
}
