// Package pkglog 输出固定字段的结构化日志（详细设计 §7.1）。
//
// 采用 zerolog（详细设计 §1.3 的依赖清单已锁定 v1.33.0）：默认输出 JSON 单行。
// 详细设计 §7.1 规定的五个字段 ts / level / svc / inst / msg 每条日志都在，
// 其余字段由调用方按需补充。JSON 而不是自定义 logfmt，是为了现场能用 jq 直接切。
package pkglog

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// Level 是日志级别，数值越大越严重。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel 把命令行字符串转成级别。
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("未知日志级别 %q，可选 debug / info / warn / error", s)
}

func (l Level) toZerolog() zerolog.Level {
	switch l {
	case LevelDebug:
		return zerolog.DebugLevel
	case LevelWarn:
		return zerolog.WarnLevel
	case LevelError:
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

// Field 是一条附加字段。值为字符串时按字符串输出，避免被当成数字或 null。
type Field struct {
	Key   string
	Value any
}

// F 是 Field 的简写，让调用点保持单行。
func F(key string, value any) Field { return Field{Key: key, Value: value} }

// Logger 是一组固定 svc / inst 的日志输出器。并发安全：两条总线协程会同时写。
type Logger struct {
	out   io.Writer
	level Level
	base  zerolog.Logger
}

func init() {
	// 字段名与详细设计 §7.1 逐个对齐：ts / level / svc / inst / msg。
	zerolog.TimestampFieldName = "ts"
	zerolog.LevelFieldName = "level"
	zerolog.MessageFieldName = "msg"
	// 毫秒时间戳，与 collectTime / sendTime 同一刻度，横比日志与报文时不用换算。
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
}

// New 创建日志器。out 为 nil 时写 stderr。
func New(svc, inst string, level Level, out io.Writer) *Logger {
	if out == nil {
		out = os.Stderr
	}
	l := &Logger{out: out, level: level}
	l.base = zerolog.New(out).
		Level(level.toZerolog()).
		With().
		Timestamp().
		Str("svc", svc).
		Str("inst", inst).
		Logger()
	return l
}

// Debug / Info / Warn / Error 按级别过滤后输出一行。
func (l *Logger) Debug(msg string, fields ...Field) { l.log(zerolog.DebugLevel, msg, fields) }
func (l *Logger) Info(msg string, fields ...Field)  { l.log(zerolog.InfoLevel, msg, fields) }
func (l *Logger) Warn(msg string, fields ...Field)  { l.log(zerolog.WarnLevel, msg, fields) }
func (l *Logger) Error(msg string, fields ...Field) { l.log(zerolog.ErrorLevel, msg, fields) }

func (l *Logger) log(level zerolog.Level, msg string, fields []Field) {
	if l.base.GetLevel() > level {
		return
	}
	ev := l.base.WithLevel(level)
	for _, f := range fields {
		// zerolog 的 Interface 走 JSON 编码：*float64 的空值会写成 null，
		// 与「值为 0」区分得开，这正是质量码 2/3/4 时要看到的东西。
		ev = ev.Interface(f.Key, f.Value)
	}
	ev.Msg(msg)
}
