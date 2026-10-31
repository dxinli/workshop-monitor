package main

import (
	"fmt"
	"io"
	"time"

	"github.com/goburrow/serial"

	"workshop-monitor/internal/agent"
	"workshop-monitor/internal/config"
)

// loadPorts 读端点表；不给路径时返回 nil，表示按点表里声明的 port 打开。
func loadPorts(path string) (agent.PortMap, error) {
	if path == "" {
		return nil, nil
	}
	return agent.LoadPorts(path)
}

// openBuses 打开终端全部总线的串口，返回按点表顺序排列的句柄与统一关闭函数。
// 任一条打开失败就关掉已开的并返回错误——只开一半的总线上线，比整体失败更难解释。
func openBuses(term *config.Terminal, ports agent.PortMap) ([]io.ReadWriter, func(), error) {
	handles := make([]io.ReadWriter, 0, len(term.Buses))
	closers := make([]io.Closer, 0, len(term.Buses))
	closeAll := func() {
		for _, c := range closers {
			_ = c.Close() // 退出路径上的关闭失败无处上报，且不改变退出码
		}
	}

	for i := range term.Buses {
		bus := &term.Buses[i]
		port, err := ports.Resolve(term.TerminalID, bus.BusID, bus.Port)
		if err != nil {
			closeAll()
			return nil, func() {}, err
		}
		handle, err := openBus(port, bus)
		if err != nil {
			closeAll()
			return nil, func() {}, fmt.Errorf("总线 %s: %w", bus.BusID, err)
		}
		handles = append(handles, handle)
		closers = append(closers, handle)
	}
	return handles, closeAll, nil
}

// openBus 打开一条总线的串口。串口参数全部来自点表，不在这里写默认值。
func openBus(port string, bus *config.Bus) (serial.Port, error) {
	p, err := serial.Open(&serial.Config{
		Address:  port,
		BaudRate: bus.BaudRate,
		DataBits: bus.DataBits,
		StopBits: bus.StopBits,
		Parity:   bus.Parity,
		Timeout:  timeoutOf(bus),
	})
	if err != nil {
		return nil, fmt.Errorf("打开串口 %s: %w", port, err)
	}
	return p, nil
}

// timeoutOf 把点表的毫秒超时转成 Duration；为 0 时给一个非 0 值--
// 串口库把 0 解释成「无超时」，会让读操作永远阻塞，整条总线就此卡死。
func timeoutOf(bus *config.Bus) time.Duration {
	if bus.TimeoutMs <= 0 {
		return time.Second
	}
	return time.Duration(bus.TimeoutMs) * time.Millisecond
}
