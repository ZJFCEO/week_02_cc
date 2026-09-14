// Command eino-agent 用 Eino 框架跑真实模型 Agent Loop，与 `go run ./cmd/demo --agent` 的手写版对照。
//
//	go run ./cmd/eino-agent
//	go run ./cmd/eino-agent --input "查询订单 ord_9999 的状态"
//
// 环境变量规则与手写版一致：DEEPSEEK_API_KEY 必填，DEEPSEEK_BASE_URL、DEEPSEEK_MODEL 可选。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/ZJFCEO/week_02_cc/go/einoagent"
)

func main() {
	input := flag.String("input", "请查询订单 ord_1001 的状态和可退金额", "发给模型的用户输入")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := einoagent.RunFromEnv(ctx, *input, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}
