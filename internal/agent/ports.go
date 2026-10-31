package agent

import (
	"encoding/json"
	"fmt"
	"os"
)

// 串口端点外部化：点表里的 port 是现场设备名（如 /dev/ttyV0），
// 仿真环境下真实端点由 scripts/setup-pty.sh 生成在别处，键为 `{terminalId}.{busId}`。
//
// 端点不写进点表：点表是冻结线 A 的契约，端点是部署事实，两者生命周期不同。
// 实机上不给 -ports 时按点表里的 port 打开，行为不变。

// PortMap 是 `{terminalId}.{busId}` → 串口路径的端点表。
type PortMap map[string]string

// LoadPorts 读取端点表。文件形状与模拟器共用的 .pty/ports.json 一致。
func LoadPorts(path string) (PortMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取端点表 %s: %w", path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析端点表 %s: %w", path, err)
	}
	return PortMap(m), nil
}

// Resolve 返回该终端该总线实际要打开的串口路径。
//
// 给了端点表就必须有对应条目：静默回落到点表的 /dev/ttyV* 会让程序去开一个
// 不存在或不属于本次运行的口，现场表现是「总线全超时」，比启动失败难查得多。
func (m PortMap) Resolve(terminalID, busID, declared string) (string, error) {
	if m == nil {
		return declared, nil
	}
	key := terminalID + "." + busID
	port, ok := m[key]
	if !ok || port == "" {
		return "", fmt.Errorf("端点表缺少总线 %s（键 %s）", busID, key)
	}
	return port, nil
}
