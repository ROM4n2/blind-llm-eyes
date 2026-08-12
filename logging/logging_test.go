package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Request ID ───

func TestNewRequestID_LengthAndFormat(t *testing.T) {
	id := NewRequestID()
	if len(id) != 16 {
		t.Errorf("want 16 chars, got %d (%q)", len(id), id)
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex char %q in id %q", c, id)
		}
	}
}

func TestNewRequestID_Uniqueness(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if seen[id] {
			t.Fatalf("duplicate id at iteration %d: %s", i, id)
		}
		seen[id] = true
	}
}

func TestWithRequestID_GetRequestID(t *testing.T) {
	ctx := context.Background()
	if got := GetRequestID(ctx); got != "" {
		t.Errorf("empty context should return empty id, got %q", got)
	}

	id := "abc123def456"
	ctx = WithRequestID(ctx, id)
	if got := GetRequestID(ctx); got != id {
		t.Errorf("want %q, got %q", id, got)
	}
}

func TestGetRequestID_NestedContext(t *testing.T) {
	ctx := context.Background()
	ctx = WithRequestID(ctx, "test-id-123")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if got := GetRequestID(ctx); got != "test-id-123" {
		t.Errorf("want %q, got %q", "test-id-123", got)
	}
}

// ─── AsyncWriter ───

func TestAsyncWriter_BasicWrite(t *testing.T) {
	var buf bytes.Buffer
	w := NewAsyncWriter(&buf, 16)
	w.Write([]byte("hello world"))
	w.Close()

	if buf.String() != "hello world" {
		t.Errorf("want %q, got %q", "hello world", buf.String())
	}
}

func TestAsyncWriter_MultipleWrites(t *testing.T) {
	var buf bytes.Buffer
	w := NewAsyncWriter(&buf, 64)
	w.Write([]byte("aaa"))
	w.Write([]byte("bbb"))
	w.Write([]byte("ccc"))
	w.Close()

	if buf.String() != "aaabbbccc" {
		t.Errorf("want %q, got %q", "aaabbbccc", buf.String())
	}
}

func TestAsyncWriter_CloseFlushesBuffer(t *testing.T) {
	var buf bytes.Buffer
	w := NewAsyncWriter(&buf, 32)

	// 写入数据但等待一小段时间确保异步写入完成
	w.Write([]byte("buffered-data"))
	time.Sleep(50 * time.Millisecond)

	// Close 应该等待所有缓冲数据写入完成
	w.Close()

	if buf.String() != "buffered-data" {
		t.Errorf("want %q, got %q", "buffered-data", buf.String())
	}
}

func TestAsyncWriter_CloseIdempotent(t *testing.T) {
	var buf bytes.Buffer
	w := NewAsyncWriter(&buf, 16)
	w.Write([]byte("data"))
	w.Close()
	w.Close() // 不应 panic
	w.Close() // 不应 panic

	if buf.String() != "data" {
		t.Errorf("want %q, got %q", "data", buf.String())
	}
}

func TestAsyncWriter_ConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	w := NewAsyncWriter(&buf, 1024)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Write([]byte("x"))
		}()
	}
	wg.Wait()
	w.Close()

	if len(buf.String()) != 100 {
		t.Errorf("want 100 bytes, got %d", len(buf.String()))
	}
}

func TestAsyncWriter_ChannelFullSynchronousFallback(t *testing.T) {
	var buf bytes.Buffer
	// 容量 1，很快填满
	w := NewAsyncWriter(&buf, 1)

	// 写入大量数据，channel 满后应同步写入
	total := 5000
	for i := 0; i < total; i++ {
		w.Write([]byte("z"))
	}
	w.Close()

	if len(buf.String()) != total {
		t.Errorf("want %d bytes, got %d", total, len(buf.String()))
	}
}

func TestAsyncWriter_ConcurrentLargeWrites(t *testing.T) {
	var buf bytes.Buffer
	w := NewAsyncWriter(&buf, 64)

	chunk := strings.Repeat("A", 100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Write([]byte(chunk))
		}()
	}
	wg.Wait()
	w.Close()

	if len(buf.String()) != 50*100 {
		t.Errorf("want %d bytes, got %d", 50*100, len(buf.String()))
	}
}

func TestAsyncWriter_WriteAfterClose(t *testing.T) {
	var buf bytes.Buffer
	w := NewAsyncWriter(&buf, 16)
	w.Write([]byte("before"))
	w.Close()

	// Close 后写入应直接走底层 writer
	w.Write([]byte("after"))

	if buf.String() != "beforeafter" {
		t.Errorf("want %q, got %q", "beforeafter", buf.String())
	}
}

// ─── Truncate ───

func TestTruncate_ShortString(t *testing.T) {
	s := "hello"
	got := Truncate(s, 10)
	if got != "hello" {
		t.Errorf("want %q, got %q", "hello", got)
	}
}

func TestTruncate_ExactLength(t *testing.T) {
	s := "hello"
	got := Truncate(s, 5)
	if got != "hello" {
		t.Errorf("want %q, got %q", "hello", got)
	}
}

func TestTruncate_LongString(t *testing.T) {
	s := "hello world"
	got := Truncate(s, 5)
	if got != "hello..." {
		t.Errorf("want %q, got %q", "hello...", got)
	}
}

func TestTruncate_EmptyString(t *testing.T) {
	got := Truncate("", 10)
	if got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

// ─── StackTrace ───

func TestStackTrace_NonEmpty(t *testing.T) {
	stack := StackTrace(500)
	if stack == "" {
		t.Error("stack trace should not be empty")
	}
}

func TestStackTrace_Truncation(t *testing.T) {
	maxLen := 50
	stack := StackTrace(maxLen)
	suffix := "...(truncated)"
	if !strings.HasSuffix(stack, suffix) {
		t.Error("truncated stack should end with ...(truncated)")
	}
	// 内容截断到 maxLen 字节 + suffix
	if len(stack) > maxLen+len(suffix) {
		t.Errorf("stack trace too long: %d bytes (max should be %d)", len(stack), maxLen+len(suffix))
	}
}

func TestStackTrace_FullLength(t *testing.T) {
	stack := StackTrace(10000)
	if strings.HasSuffix(stack, "...(truncated)") {
		t.Error("large maxLen should not truncate")
	}
}

// ─── NewLogger / NewLoggerWithWriter ───

func TestNewLoggerWithWriter_JSONOutput(t *testing.T) {
	var buf bytes.Buffer
	logger, writer := NewLoggerWithWriter(&buf, 16, "info")

	logger.Info("test message", "key", "value")
	writer.Close()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, buf.String())
	}
	if entry["message"] != "test message" {
		t.Errorf("message mismatch: %v", entry["message"])
	}
	if entry["key"] != "value" {
		t.Errorf("key mismatch: %v", entry["key"])
	}
	if entry["level"] != "INFO" {
		t.Errorf("level mismatch: %v", entry["level"])
	}
	if _, ok := entry["timestamp"]; !ok {
		t.Error("timestamp field missing")
	}
}

func TestNewLoggerWithWriter_LevelDebug(t *testing.T) {
	var buf bytes.Buffer
	logger, writer := NewLoggerWithWriter(&buf, 16, "debug")

	logger.Debug("debug msg")
	writer.Close()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if entry["level"] != "DEBUG" {
		t.Errorf("want DEBUG, got %v", entry["level"])
	}
}

func TestNewLoggerWithWriter_LevelInfo(t *testing.T) {
	var buf bytes.Buffer
	logger, writer := NewLoggerWithWriter(&buf, 16, "info")

	logger.Debug("should be filtered")
	logger.Info("should appear")
	writer.Close()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if entry["message"] != "should appear" {
		t.Errorf("want %q, got %v", "should appear", entry["message"])
	}
}

func TestNewLoggerWithWriter_LevelWarn(t *testing.T) {
	var buf bytes.Buffer
	logger, writer := NewLoggerWithWriter(&buf, 16, "warn")

	logger.Info("should be filtered")
	logger.Warn("should appear")
	writer.Close()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if entry["level"] != "WARN" {
		t.Errorf("want WARN, got %v", entry["level"])
	}
}

func TestNewLoggerWithWriter_LevelError(t *testing.T) {
	var buf bytes.Buffer
	logger, writer := NewLoggerWithWriter(&buf, 16, "error")

	logger.Warn("should be filtered")
	logger.Error("should appear")
	writer.Close()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if entry["level"] != "ERROR" {
		t.Errorf("want ERROR, got %v", entry["level"])
	}
}

func TestNewLoggerWithWriter_LevelDefault(t *testing.T) {
	var buf bytes.Buffer
	logger, writer := NewLoggerWithWriter(&buf, 16, "unknown")

	logger.Info("should appear with default info level")
	writer.Close()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if entry["level"] != "INFO" {
		t.Errorf("want INFO (default), got %v", entry["level"])
	}
}

func TestNewLoggerWithWriter_TimestampField(t *testing.T) {
	var buf bytes.Buffer
	logger, writer := NewLoggerWithWriter(&buf, 16, "info")

	logger.Info("ts test")
	writer.Close()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	ts, ok := entry["timestamp"].(string)
	if !ok {
		t.Fatalf("timestamp not a string: %T", entry["timestamp"])
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("invalid timestamp format: %v", err)
	}
}
