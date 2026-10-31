package agent

import (
	"errors"
	"fmt"
)

// Modbus RTU 帧编解码（接口规范 §3.4 §3.6）。
//
// 本期只发一种请求：03 读保持寄存器。点表的所有块都是 03（V-01 校验强制），
// 不实现 04 / 06 / 16——验收里出现 FC04 只是为了证明 0x84 异常码字节，那是模拟器侧的事。
const (
	fcReadHolding = 3
	// 异常应答把功能码最高位置 1：03 → 0x83。
	exceptionFlag = 0x80
	// 最小应答：从站 + 功能码 + 异常码 + CRC，共 5 字节。
	minFrameLen = 5
	crcLen      = 2
)

// ErrTimeout 表示整帧无响应。超时换一次重试，仍超时记本点 TIMEOUT。
var ErrTimeout = errors.New("响应超时")

// ErrCRC 表示收到完整帧但 CRC 不匹配。重试 1 次，仍错记 CRC_ERROR。
var ErrCRC = errors.New("CRC 校验失败")

// ErrFrame 表示帧长度或字段不符（接口规范 §3.6「帧长度或格式不符」）。
var ErrFrame = errors.New("帧格式不符")

// ExceptionError 是从站返回的异常应答，Code 为 01~04。
type ExceptionError struct{ Code int }

func (e *ExceptionError) Error() string {
	return fmt.Sprintf("从站异常应答 0x%02X", e.Code)
}

// crc16 是 Modbus 的 CRC16：初值 0xFFFF、多项式 0xA001、低位在前。
func crc16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// withCRC 追加 CRC，线上低字节在前。
func withCRC(frame []byte) []byte {
	c := crc16(frame)
	return append(frame, byte(c), byte(c>>8))
}

// buildReadRequest 拼一帧 03 请求。
func buildReadRequest(slave, startAddr, count int) ([]byte, error) {
	if slave < 1 || slave > 247 {
		return nil, fmt.Errorf("从站地址 %d 越界", slave)
	}
	if count < 1 || count > 125 {
		return nil, fmt.Errorf("读取寄存器数 %d 越界", count)
	}
	frame := []byte{
		byte(slave), fcReadHolding,
		byte(startAddr >> 8), byte(startAddr),
		byte(count >> 8), byte(count),
	}
	return withCRC(frame), nil
}

// responseLength 是一帧正常应答的长度：从站 + 功能码 + 字节数 + 数据 + CRC。
func responseLength(count int) int {
	return 3 + count*2 + crcLen
}

// exceptionFrameLen 是异常应答长度：从站 + 功能码 + 异常码 + CRC。
const exceptionFrameLen = minFrameLen

// isException 判断已收到的前几个字节是否异常应答。
func isException(buf []byte) bool {
	return len(buf) >= 2 && buf[1]&exceptionFlag != 0
}

// decodeReadResponse 校验一帧正常应答并取出寄存器字。
// 帧长度与 CRC 在 transport 层已经判过，这里只判功能码、字节数与从站地址。
func decodeReadResponse(slave, count int, frame []byte) ([]uint16, error) {
	if len(frame) != responseLength(count) {
		return nil, fmt.Errorf("%w：期望 %d 字节，实收 %d", ErrFrame, responseLength(count), len(frame))
	}
	if int(frame[0]) != slave {
		return nil, fmt.Errorf("%w：应答从站 %d，请求 %d", ErrFrame, frame[0], slave)
	}
	if int(frame[1]) != fcReadHolding {
		return nil, fmt.Errorf("%w：应答功能码 %d，请求 %d", ErrFrame, frame[1], fcReadHolding)
	}
	if int(frame[2]) != count*2 {
		return nil, fmt.Errorf("%w：字节数 %d，期望 %d", ErrFrame, frame[2], count*2)
	}
	words := make([]uint16, count)
	for i := 0; i < count; i++ {
		words[i] = uint16(frame[3+i*2])<<8 | uint16(frame[4+i*2])
	}
	return words, nil
}

// parseException 取出异常码，并同时确认 CRC 正确——CRC 错的「异常帧」不算设备异常。
func parseException(frame []byte) (int, error) {
	if len(frame) != exceptionFrameLen {
		return 0, fmt.Errorf("%w：异常帧 %d 字节", ErrFrame, len(frame))
	}
	if crc16(frame[:len(frame)-crcLen]) != uint16(frame[len(frame)-crcLen])|uint16(frame[len(frame)-crcLen+1])<<8 {
		return 0, ErrCRC
	}
	return int(frame[2]), nil
}
