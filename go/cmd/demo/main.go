// Command demo 运行离线治理演示，对应 Python 版的 `python tool_governance_demo.py`。
package main

import (
	"context"

	"github.com/ZJFCEO/week_02_cc/go/governance"
)

func main() {
	governance.RunOfflineDemo(context.Background())
}
