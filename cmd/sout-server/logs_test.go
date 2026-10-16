package main

import (
	"strings"
	"testing"
)

func TestIgnoredLogLine(t *testing.T) {
	eofLine := "2026/10/16 06:05:53 ERROR [1037599052 14s] inbound/mixed[mixed-in]: mux connection closed: read frame header: EOF"
	if !isIgnoredLogLine(eofLine) {
		t.Fatalf("expected eofLine to be ignored, got false")
	}

	normalLine := "2026/10/16 06:05:54 INFO inbound/mixed[mixed-in]: inbound connection from 127.0.0.1:54321"
	if isIgnoredLogLine(normalLine) {
		t.Fatalf("expected normalLine to NOT be ignored, got true")
	}
}

func TestFilterLogNoise(t *testing.T) {
	raw := "line1\n2026/10/16 ERROR mux connection closed: read frame header: EOF\nline2\n"
	filtered := filterLogNoise(raw)
	if strings.Contains(filtered, "read frame header: EOF") {
		t.Fatalf("filterLogNoise failed to strip EOF log line: %s", filtered)
	}
	if !strings.Contains(filtered, "line1") || !strings.Contains(filtered, "line2") {
		t.Fatalf("filterLogNoise incorrectly stripped valid lines: %s", filtered)
	}
}

func TestRingLogBuffer(t *testing.T) {
	buf := &RingLogBuffer{
		maxLines: 3,
		lines:    make([]string, 0, 3),
	}

	_, _ = buf.Write([]byte("1\n2\n3\n"))
	if len(buf.lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(buf.lines))
	}

	// 写入被忽略的噪音行，应该被自动丢弃
	_, _ = buf.Write([]byte("mux connection closed: read frame header: EOF\n"))
	if len(buf.lines) != 3 {
		t.Fatalf("expected still 3 lines after noise line, got %d", len(buf.lines))
	}

	// 写入第 4 行，应该将第 1 行挤出
	_, _ = buf.Write([]byte("4\n"))
	if len(buf.lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(buf.lines))
	}
	res := buf.Get(3)
	if res != "2\n3\n4" {
		t.Fatalf("expected '2\\n3\\n4', got %q", res)
	}
}
