package governance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// 四、工具参数模型（第一道闸门）
// ---------------------------------------------------------------------------

// Args 是所有工具参数的公共接口。
//
// 模型提交上来的是一段不可信的 JSON，必须先过这一层才能变成 handler 敢用的对象。
// Python 靠 pydantic 声明式完成；Go 没有等价物，拆成两步：
//
//	第一步 DecodeArgs  —— encoding/json 负责结构和类型（含未知字段拦截）
//	第二步 Validate    —— 每个类型自己写取值范围和正则
type Args interface {
	Validate() error
}

var (
	orderIDPattern = regexp.MustCompile(`^ord_[0-9]{4}$`)
	accountPattern = regexp.MustCompile(`^ACC-[A-Z]-[0-9]{6}$`)

	// unknownFieldPattern 用来从 encoding/json 的报错里抠出字段名。
	// 报错原文形如：json: unknown field "approved"
	unknownFieldPattern = regexp.MustCompile(`unknown field "([^"]+)"`)
)

// GetOrderArgs 查询订单的参数。
type GetOrderArgs struct {
	OrderID string `json:"order_id"`
}

func (a *GetOrderArgs) Validate() error {
	errs := &ValidationError{}
	if !orderIDPattern.MatchString(a.OrderID) {
		errs.Add("order_id", "必须形如 ord_1001")
	}
	return errs.OrNil()
}

// CreateRefundArgs 创建退款的参数。
type CreateRefundArgs struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
	Reason  string  `json:"reason"`
}

func (a *CreateRefundArgs) Validate() error {
	errs := &ValidationError{}
	if !orderIDPattern.MatchString(a.OrderID) {
		errs.Add("order_id", "必须形如 ord_1001")
	}
	if a.Amount <= 0 || a.Amount > 10_000 {
		errs.Add("amount", "必须大于 0 且不超过 10000")
	}
	if n := len([]rune(a.Reason)); n < 4 || n > 200 {
		errs.Add("reason", "长度必须在 4 到 200 之间")
	}
	return errs.OrNil()
}

// RunShellArgs 模拟 Shell 的参数。
type RunShellArgs struct {
	Command string `json:"command"`
}

func (a *RunShellArgs) Validate() error {
	errs := &ValidationError{}
	if n := len([]rune(a.Command)); n < 1 || n > 200 {
		errs.Add("command", "长度必须在 1 到 200 之间")
	}
	return errs.OrNil()
}

// TransferArgs 是【作业·任务 2】的转账参数。三条约束都在这一层完成，handler 里不必再校验：
//
//	账号形如 ACC-A-123456（银行前缀 + 单个大写字母 + 6 位数字）
//	金额大于 0 且不超过 100000（这是 Schema 上限，业务限额另见 transferPrecheck）
type TransferArgs struct {
	FromAccount string  `json:"from_account"`
	ToAccount   string  `json:"to_account"`
	Amount      float64 `json:"amount"`
}

func (a *TransferArgs) Validate() error {
	errs := &ValidationError{}
	if !accountPattern.MatchString(a.FromAccount) {
		errs.Add("from_account", "必须形如 ACC-A-123456")
	}
	if !accountPattern.MatchString(a.ToAccount) {
		errs.Add("to_account", "必须形如 ACC-A-123456")
	}
	if a.Amount <= 0 || a.Amount > 100_000 {
		errs.Add("amount", "必须大于 0 且不超过 100000")
	}
	return errs.OrNil()
}

// DecodeArgs 把模型提交的原始 JSON 解成参数对象，再跑业务校验。
//
// DisallowUnknownFields 就是 Python 里 extra="forbid" 的等价物：
// 多传一个 user_id / approved 这样的越权字段会直接失败，这是防模型注入的最后一道屏障。
//
// 【Go 差异】pydantic 会把所有字段的问题一次报全，encoding/json 遇到第一个未知字段就停。
// 所以结构性错误这里最多报一条，业务校验（Validate）才是一次报全的。
func DecodeArgs(raw json.RawMessage, target Args) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		errs := &ValidationError{}
		var typeErr *json.UnmarshalTypeError
		switch {
		case matchUnknownField(err) != "":
			// 多传了 Schema 里没有的字段
			errs.Add(matchUnknownField(err), "不允许提交额外字段")
		case errors.As(err, &typeErr):
			// 类型对不上，例如把金额写成字符串。
			// Python 靠 strict=True 关掉隐式转换，Go 的 json 解码本来就不做这种转换。
			field := typeErr.Field
			if field == "" {
				field = "(root)"
			}
			errs.Add(field, fmt.Sprintf("类型错误，期望 %s", typeErr.Type))
		default:
			errs.Add("(root)", "JSON 解析失败")
		}
		return errs
	}

	return target.Validate()
}

func matchUnknownField(err error) string {
	m := unknownFieldPattern.FindStringSubmatch(err.Error())
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// ArgumentKeys 取出原始 JSON 的顶层字段名并排序，供审计使用（只记名字不记值）。
func ArgumentKeys(raw json.RawMessage) []string {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	keys := make([]string, 0, len(probe))
	for key := range probe {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// MustArgs 把一个 map 编码成 JSON，方便测试和演示构造调用参数。
func MustArgs(fields map[string]any) json.RawMessage {
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return raw
}

// RawArgs 直接用 JSON 字面量构造参数，测试里读起来更直观。
func RawArgs(literal string) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(literal))
}
