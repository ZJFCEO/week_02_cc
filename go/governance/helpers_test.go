// 测试公共工具。
//
// Go 的测试文件和源码放在同一个目录、同一个包里，文件名以 _test.go 结尾，
// 不像 Python 那样单独建 tests/ 目录。这样测试能直接访问包内未导出的函数和变量。
//
// 本目录的测试文件与 Python 版 tests/test_tool_governance.py 一一对应：
//
//	governance_test.go         训练营原版的 8 个基线测试（移植）
//	transfer_test.go           作业新增的 5 个转账测试
//	timeout_semantics_test.go  Go 独有：证明超时不等于取消
//
// go test 速查（对照 pytest）：
//
//	Go                              pytest 里对应什么
//	-----------------------------   ----------------------------------------
//	func TestXxx(t *testing.T)      def test_xxx()，同样靠名字前缀发现
//	t.Fatalf / t.Errorf             assert，前者立即结束用例，后者继续往下跑
//	t.Cleanup(fn)                   fixture 里 yield 之后的 teardown
//	临时改包级变量 + t.Cleanup 还原   monkeypatch.setattr
//	go test -run Transfer           pytest -k transfer
//	go test -v                      pytest -v
//
// 这些用例共享包级账本，所以都不能调用 t.Parallel()。
package governance

import (
	"context"
	"maps"
	"testing"
)

// setup 对应 Python 用例开头的 reset_side_effects()：复位账本与副作用计数器。
// 额外在用例结束后再复位一次，避免失败的用例把脏状态留给下一个。
func setup(t *testing.T) context.Context {
	t.Helper()
	ResetState()
	t.Cleanup(ResetState)
	return context.Background()
}

// refundArguments 对应 Python 版的 REFUND_ARGUMENTS 常量。
//
// 【Go 差异】map 不能声明成 const。如果做成包级变量，某个用例改了它，
// 后面的用例就会拿到被改过的数据。所以用函数每次返回一份新的。
func refundArguments() map[string]any {
	return map[string]any{
		"order_id": "ord_1001",
		"amount":   399.0,
		"reason":   "商品存在质量问题",
	}
}

// transferArguments 对应 Python 版的 TRANSFER_ARGUMENTS，overrides 相当于 {**TRANSFER_ARGUMENTS, ...}。
func transferArguments(overrides map[string]any) map[string]any {
	args := map[string]any{
		"from_account": "ACC-A-123456",
		"to_account":   "ACC-A-654321",
		"amount":       1000.0,
	}
	maps.Copy(args, overrides)
	return args
}

// merge 对应 Python 的 {**base, **extra}。
func merge(base, extra map[string]any) map[string]any {
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

// toolNames 从 ModelTools 的返回值里取出工具名集合。
func toolNames(tools []map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, tool := range tools {
		if function, ok := tool["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok {
				names[name] = true
			}
		}
	}
	return names
}
