package agent

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/goburrow/serial"
)

// 一帧收发的实现。串口句柄由调用方打开并注入（cmd/edge-agent），本包不创建外部资源。
//
// 收帧按 RTU 的两种应答形状判定长度：
//   正常：从站 + 03 + 字节数 + 数据 + CRC
//   异常：从站 + 83 + 异常码 + CRC
// 先读 2 字节即可分辨，因此不需要预先知道长度。

// readFrame 读到一帧完整应答（含 CRC）。
//
// 超时与「收到半帧」分开：一个字节都没收到的才是通信超时（可重试 TIMEOUT），
// 收到半帧属于帧格式不符（PARSE_ERROR）。把两者混成一个错误会让现场无法区分
// 「设备不响应」和「设备响应了但线不好」。
func readFrame(port io.Reader, deadline time.Time) ([]byte, error) {
	buf := make([]byte, 0, 300)
	tmp := make([]byte, 256)

	for {
		if want := expectedFrameLen(buf); want > 0 && len(buf) >= want {
			return buf[:want], nil
		}
		if len(buf) >= 3 && !isException(buf) && buf[2] > 250 {
			return nil, fmt.Errorf("%w：字节数 %d", ErrFrame, buf[2])
		}
		if time.Now().After(deadline) {
			return nil, partialOrTimeout(buf)
		}

		n, err := port.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			// io.EOF 只说明这一轮没读到字节，不代表端口关闭：串口上的「没数据」
			// 就是一直等到 deadline，由上面那一次判定分超时与半帧。
			if errors.Is(err, io.EOF) {
				continue
			}
			if errors.Is(err, serial.ErrTimeout) {
				if want := expectedFrameLen(buf); want > 0 && len(buf) >= want {
					return buf[:want], nil
				}
				return nil, partialOrTimeout(buf)
			}
			return nil, fmt.Errorf("读取串口: %w", err)
		}
	}
}

// expectedFrameLen 返回应收帧长；尚不足以判断时返回 0。
func expectedFrameLen(buf []byte) int {
	if len(buf) < 2 {
		return 0
	}
	if isException(buf) {
		return exceptionFrameLen
	}
	if len(buf) < 3 {
		return 0
	}
	return 3 + int(buf[2]) + crcLen
}

func partialOrTimeout(buf []byte) error {
	if len(buf) == 0 {
		return ErrTimeout
	}
	return fmt.Errorf("%w：半帧 %d 字节", ErrFrame, len(buf))
}

// exchange 发一帧请求并收一帧应答，返回寄存器字或错误。
// 串口写失败直接返回，不重试：写不进去说明句柄已坏，重试只会拖住整条总线。
func exchange(port io.ReadWriter, slave int, startAddr, count int, timeout, interFrame time.Duration) ([]uint16, error) {
	req, err := buildReadRequest(slave, startAddr, count)
	if err != nil {
		return nil, err
	}
	if _, err := port.Write(req); err != nil {
		return nil, fmt.Errorf("写入串口: %w", err)
	}
	frame, err := readFrame(port, time.Now().Add(timeout))
	if err != nil {
		return nil, err
	}
	// 帧间静默：下一次请求必须等总线安静下来，否则从站会把新请求当成上一帧的尾巴。
	time.Sleep(interFrame)

	if isException(frame) {
		code, err := parseException(frame)
		if err != nil {
			return nil, err
		}
		return nil, &ExceptionError{Code: code}
	}
	if crc16(frame[:len(frame)-crcLen]) != uint16(frame[len(frame)-crcLen])|uint16(frame[len(frame)-crcLen+1])<<8 {
		return nil, ErrCRC
	}
	return decodeReadResponse(slave, count, frame)
}
