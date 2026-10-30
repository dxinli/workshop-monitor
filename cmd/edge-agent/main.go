// edge-agent：车间终端上的采集进程。
// 本文件只做参数解析与依赖装配，业务逻辑在 internal/agent。
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	configPath := flag.String("config", "", "终端点表配置路径，如 config/terminals/ESN-ZX-0001.json")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "缺少必填参数 -config")
		os.Exit(2)
	}

	if _, err := os.Stat(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "edge-agent 尚未实现，配置: %s\n", *configPath)
	os.Exit(1)
}
