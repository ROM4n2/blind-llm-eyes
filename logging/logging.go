package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
)

// --- Request ID 传播 ---

type ctxKey struct{}

// NewRequestID 生成 16 字符的十六进制请求 ID。
func NewRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// WithRequestID 将 request_id 存入 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// GetRequestID 从 context 中提取 request_id，不存在则返回空串。
func GetRequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// --- 异步写入器 ---

// AsyncWriter 包装 io.Writer，通过带缓冲的 channel 实现异步写入。
// channel 满时回退为同步写入，避免丢弃日志。
type AsyncWriter struct {
	ch     chan []byte
	out    io.Writer
	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// NewAsyncWriter 创建容量为 bufSize 的异步写入器。
func NewAsyncWriter(out io.Writer, bufSize int) *AsyncWriter {
	w := &AsyncWriter{
		ch:   make(chan []byte, bufSize),
		out:  out,
		done: make(chan struct{}),
	}
	go w.writeLoop()
	return w
}

func (w *AsyncWriter) writeLoop() {
	for entry := range w.ch {
		w.out.Write(entry)
	}
	close(w.done)
}

// Write 将数据拷贝后异步写入。channel 满时同步写入。
func (w *AsyncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return w.out.Write(p)
	}
	w.mu.Unlock()

	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case w.ch <- cp:
		return len(p), nil
	default:
		return w.out.Write(p)
	}
}

// Close 关闭 channel 并等待所有缓冲日志写入完成。
func (w *AsyncWriter) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()
	close(w.ch)
	<-w.done
}

// --- JSON 日志工厂 ---

// replaceAttr 将 slog 默认字段名映射为业务要求的 JSON schema。
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		a.Key = "timestamp"
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

// NewLogger 创建 JSON 结构化日志器，带异步写入。
// 返回 logger 和 AsyncWriter（调用方在 shutdown 时需调用 Close）。
func NewLogger(level string) (*slog.Logger, *AsyncWriter) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	writer := NewAsyncWriter(os.Stderr, 4096)
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: replaceAttr,
	})
	return slog.New(handler), writer
}

// --- 辅助函数 ---

// StackTrace 返回当前 goroutine 的堆栈信息（截断到 maxLen）。
func StackTrace(maxLen int) string {
	stack := debug.Stack()
	if len(stack) > maxLen {
		return string(stack[:maxLen]) + "...(truncated)"
	}
	return string(stack)
}

// Truncate 截断字符串到指定长度，超出部分用 "..." 标记。
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
