// Command demo 是治理演示的入口，对应 Python 版的 `python tool_governance_demo.py`。
//
//	go run ./cmd/demo                   离线演示，不需要 API Key
//	go run ./cmd/demo --agent           接 DeepSeek 跑真实 Agent Loop
//	go run ./cmd/demo --agent --input "请查询订单 ord_1001 的状态"
//
// Go 的 flag 包同时接受 -agent 和 --agent 两种写法，所以命令行和 Python 版保持一致。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/ZJFCEO/week_02_cc/go/governance"
)

func main() {
	// 对应 Python 版的 parse_args()
	agent := flag.Bool("agent", false, "使用 DeepSeek 运行真实 Agent Loop")
	input := flag.String("input", "请查询订单 ord_1001 的状态和可退金额", "发给模型的用户输入")
	flag.Parse()

	// Ctrl+C 时取消 ctx，正在进行的流式请求会随之中断。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 对应 Python 版的：
	//   asyncio.run(run_deepseek_agent(cli_args.input) if cli_args.agent else run_offline_demo())
	if *agent {
		if err := governance.RunDeepSeekAgent(ctx, *input, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		return
	}
	governance.RunOfflineDemo(ctx)
}
