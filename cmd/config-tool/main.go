// config-tool：配置校验与种子生成。不连数据库，不改配置文件。
//
//	config-tool verify  -terminals config/terminals -thresholds config/thresholds.json
//	config-tool seed    -terminals config/terminals -thresholds config/thresholds.json -out deploy/timescaledb/init/02_seed.sql
//
// 退出码：0 合法；1 配置有致命问题（或生成失败）；2 参数错误。
package main

import (
	"flag"
	"fmt"
	"os"

	"workshop-monitor/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "seed":
		os.Exit(runSeed(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法：config-tool <verify|seed> [参数]")
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	terminalsDir := fs.String("terminals", "config/terminals", "终端点表目录")
	thresholdsPath := fs.String("thresholds", "config/thresholds.json", "告警阈值文件")
	topicsPath := fs.String("topics", "config/topics.json", "主题模板文件")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rep, err := load(*terminalsDir, *thresholdsPath, *topicsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置载入失败：%v\n", err)
		return 1
	}

	printReport(rep)
	if rep.Failed() {
		return 1
	}
	return 0
}

func runSeed(args []string) int {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	terminalsDir := fs.String("terminals", "config/terminals", "终端点表目录")
	thresholdsPath := fs.String("thresholds", "config/thresholds.json", "告警阈值文件")
	topicsPath := fs.String("topics", "config/topics.json", "主题模板文件")
	out := fs.String("out", "deploy/timescaledb/init/02_seed.sql", "生成路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 先校验再生成：有致命问题时绝不产出半份种子。
	rep, err := load(*terminalsDir, *thresholdsPath, *topicsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置载入失败：%v\n", err)
		return 1
	}
	printReport(rep)
	if rep.Failed() {
		fmt.Fprintln(os.Stderr, "配置校验未通过，未生成种子脚本")
		return 1
	}

	terminals, th, _, err := loadRaw(*terminalsDir, *thresholdsPath, *topicsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置载入失败：%v\n", err)
		return 1
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建 %s 失败：%v\n", *out, err)
		return 1
	}
	werr := config.WriteSeedSQL(f, terminals, th)
	cerr := f.Close()
	if werr != nil {
		fmt.Fprintf(os.Stderr, "生成种子脚本失败：%v\n", werr)
		return 1
	}
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "关闭 %s 失败：%v\n", *out, cerr)
		return 1
	}

	fmt.Printf("已生成 %s：终端 %d / 总线 %d / 从站 %d / 测点 %d / 规则 %d 条（展开 %d 行）\n",
		*out, rep.Terminals, rep.Buses, rep.Slaves, rep.Points, rep.Rules, thresholdRows(terminals, th))
	return 0
}

func load(terminalsDir, thresholdsPath, topicsPath string) (config.Report, error) {
	terminals, th, topics, err := loadRaw(terminalsDir, thresholdsPath, topicsPath)
	if err != nil {
		return config.Report{}, err
	}
	return config.Validate(terminals, th, topics), nil
}

func loadRaw(terminalsDir, thresholdsPath, topicsPath string) ([]*config.Terminal, *config.ThresholdFile, *config.TopicFile, error) {
	terminals, err := config.LoadTerminalDir(terminalsDir)
	if err != nil {
		return nil, nil, nil, err
	}
	th, err := config.LoadThresholds(thresholdsPath)
	if err != nil {
		return nil, nil, nil, err
	}
	topics, err := config.LoadTopics(topicsPath)
	if err != nil {
		return nil, nil, nil, err
	}
	return terminals, th, topics, nil
}

func printReport(rep config.Report) {
	for _, iss := range rep.Issues {
		fmt.Fprintln(os.Stderr, iss.String())
	}
	fmt.Printf("规模：终端 %d / 总线 %d / 从站 %d / 测点 %d；阈值规则 %d 条；问题 %d 条\n",
		rep.Terminals, rep.Buses, rep.Slaves, rep.Points, rep.Rules, len(rep.Issues))
}

func thresholdRows(terminals []*config.Terminal, th *config.ThresholdFile) int {
	n := 0
	for _, t := range terminals {
		for _, r := range th.Rules {
			if r.TerminalID != "" && r.TerminalID != t.TerminalID {
				continue
			}
			n += len(r.PointCodes)
		}
	}
	return n
}
