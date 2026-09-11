package governance

import (
	"regexp"
	"sync"
)

// AuditSink 是审计落库口。教学示例只塞进内存切片，生产上写日志、写 Kafka 或写审计库。
// 关键不在于存到哪里，而在于 ToolRuntime 会在决策和执行两个阶段都主动写一条。
type AuditSink struct {
	mu      sync.Mutex
	records []AuditRecord
}

// NewAuditSink 创建一个空的审计口。
func NewAuditSink() *AuditSink {
	return &AuditSink{}
}

// Append 追加一条审计记录。
func (s *AuditSink) Append(record AuditRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
}

// Records 返回审计记录的快照。
//
// 【Go 差异】Python 版直接暴露 list 属性；Go 返回拷贝，避免调用方拿着切片头去改内部状态，
// 也避免读的同时别的 goroutine 在 append 导致数据竞争。
func (s *AuditSink) Records() []AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditRecord(nil), s.records...)
}

var (
	// dangerousShellPatterns 是危险 Shell 命令的正则黑名单。
	// 正则只是教学兜底：真实环境必须靠窄接口、AST 解析和沙箱，
	// 因为 r''m -rf 之类的变形随手就能绕过黑名单。
	dangerousShellPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\brm\s+-rf\b`),
		regexp.MustCompile(`(?i)\bgit\s+push\s+--force\b`),
		regexp.MustCompile(`(?i)\bgit\s+reset\s+--hard\b`),
		regexp.MustCompile(`(?i)\bsudo\b`),
		regexp.MustCompile(`(?i)\bmkfs\b`),
		regexp.MustCompile(`(?i)>\s*/dev/`),
	}

	secretKeyPattern = regexp.MustCompile(`(?i)token|secret|password|authorization`)
	emailPattern     = regexp.MustCompile(`[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}`)

	// accountMaskPattern 是【作业·任务 6】的账号脱敏正则。
	// 把 6 位数字拆成"前 2 位（丢弃）+ 后 4 位（保留）"：ACC-A-123456 -> ACC-A-****3456
	//
	// 【Go 差异】Go 的 regexp 是 RE2，不支持命名反向引用那一套花活，
	// 但捕获组 + ${1} 这种替换是够用的，而且保证线性时间，不会被恶意输入卡死。
	accountMaskPattern = regexp.MustCompile(`\b(ACC-[A-Z]-)[0-9]{2}([0-9]{4})\b`)
)

func isDangerousShell(command string) bool {
	for _, pattern := range dangerousShellPatterns {
		if pattern.MatchString(command) {
			return true
		}
	}
	return false
}

// Redact 做结果脱敏，递归处理 map、切片和字符串三种情况：
//
//  1. map 的 key 命中 token/secret/password/authorization 时，整个值替换成 ***
//  2. 字符串里的邮箱替换成 ***@***
//  3. 字符串里的账号替换成 ACC-A-****3456
//
// 位置很重要：它被 ToolRuntime 在 finalize 阶段统一调用，不在任何 handler 里。
// 这样新增工具的作者不可能忘记脱敏——这是"横切关注点上提"的典型例子。
func Redact(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if secretKeyPattern.MatchString(key) {
				out[key] = "***"
				continue
			}
			out[key] = Redact(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, Redact(item))
		}
		return out
	case string:
		masked := emailPattern.ReplaceAllString(typed, "***@***")
		return accountMaskPattern.ReplaceAllString(masked, "${1}****${2}")
	default:
		return value
	}
}

// RedactString 是只针对字符串的便捷入口，演示里打印余额时用得到。
func RedactString(value string) string {
	masked, _ := Redact(value).(string)
	return masked
}
