package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"
)

func TestParseCgroupV2(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		content  string
		create   bool
		expected int64
	}{
		{"valid_64mb", "67108864\n", true, 67108864},
		{"valid_80mb", "83886080", true, 83886080},
		{"max_ignored", "max\n", true, 0},
		{"max_caps_ignored", "MAX", true, 0},
		{"empty_file", "", true, 0},
		{"whitespace_only", "   \n\t", true, 0},
		{"invalid_string", "not_a_number", true, 0},
		{"negative_number", "-100", true, 0},
		{"zero", "0", true, 0},
		{"file_not_exist", "", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(tempDir, tt.name)
			if tt.create {
				if err := os.WriteFile(filePath, []byte(tt.content), 0644); err != nil {
					t.Fatalf("failed to write test file: %v", err)
				}
			}
			got := parseCgroupV2(filePath)
			if got != tt.expected {
				t.Errorf("parseCgroupV2() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestParseCgroupV1(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		content  string
		create   bool
		expected int64
	}{
		{"valid_128mb", "134217728\n", true, 134217728},
		{"unlimited_v1_large", "9223372036854771712\n", true, 0},
		{"unlimited_v1_uint64_max", "18446744073709551615\n", true, 0},
		{"empty_file", "", true, 0},
		{"invalid_string", "abc\n", true, 0},
		{"zero", "0", true, 0},
		{"file_not_exist", "", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(tempDir, tt.name)
			if tt.create {
				if err := os.WriteFile(filePath, []byte(tt.content), 0644); err != nil {
					t.Fatalf("failed to write test file: %v", err)
				}
			}
			got := parseCgroupV1(filePath)
			if got != tt.expected {
				t.Errorf("parseCgroupV1() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestParseProcMeminfo(t *testing.T) {
	tempDir := t.TempDir()

	normalMeminfo := `MemTotal:        1016148 kB
MemFree:          617260 kB
MemAvailable:     762888 kB
Buffers:           35212 kB
Cached:           198284 kB
`
	meminfo64MB := `MemTotal:        65536 kB
MemFree:         30000 kB
`
	meminfoNoColonSpace := `MemTotal:131072 kB
MemFree: 50000 kB
`

	tests := []struct {
		name     string
		content  string
		create   bool
		expected int64
	}{
		{"normal", normalMeminfo, true, 1016148 * 1024},
		{"64mb", meminfo64MB, true, 65536 * 1024},
		{"no_space_colon", meminfoNoColonSpace, true, 131072 * 1024},
		{"missing_memtotal", "MemFree: 50000 kB\n", true, 0},
		{"invalid_value", "MemTotal: invalid kB\n", true, 0},
		{"zero_value", "MemTotal: 0 kB\n", true, 0},
		{"file_not_exist", "", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filePath := filepath.Join(tempDir, tt.name)
			if tt.create {
				if err := os.WriteFile(filePath, []byte(tt.content), 0644); err != nil {
					t.Fatalf("failed to write test file: %v", err)
				}
			}
			got := parseProcMeminfo(filePath)
			if got != tt.expected {
				t.Errorf("parseProcMeminfo() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestDetectMemoryLimitPrecedence(t *testing.T) {
	tempDir := t.TempDir()

	v2File := filepath.Join(tempDir, "memory.max")
	v1File := filepath.Join(tempDir, "memory.limit_in_bytes")
	meminfoFile := filepath.Join(tempDir, "meminfo")

	// 1. v2 与 v1 均有效时：v2 优先
	_ = os.WriteFile(v2File, []byte("67108864\n"), 0644)                 // 64MB
	_ = os.WriteFile(v1File, []byte("134217728\n"), 0644)                // 128MB
	_ = os.WriteFile(meminfoFile, []byte("MemTotal: 262144 kB\n"), 0644) // 256MB

	limit := detectMemoryLimitFromPaths(v2File, v1File, meminfoFile)
	if limit != 67108864 {
		t.Fatalf("expected v2 precedence (67108864), got %d", limit)
	}

	// 2. v2 为 max，v1 有效时：v1 优先于 meminfo
	_ = os.WriteFile(v2File, []byte("max\n"), 0644)
	limit = detectMemoryLimitFromPaths(v2File, v1File, meminfoFile)
	if limit != 134217728 {
		t.Fatalf("expected v1 precedence (134217728), got %d", limit)
	}

	// 3. v2 为 max，v1 为无限制 (> 1<<50)：回退到 meminfo
	_ = os.WriteFile(v1File, []byte("9223372036854771712\n"), 0644)
	limit = detectMemoryLimitFromPaths(v2File, v1File, meminfoFile)
	if limit != 262144*1024 {
		t.Fatalf("expected meminfo fallback (%d), got %d", 262144*1024, limit)
	}

	// 4. 全部无效时返回 0
	_ = os.WriteFile(meminfoFile, []byte(""), 0644)
	limit = detectMemoryLimitFromPaths(v2File, v1File, meminfoFile)
	if limit != 0 {
		t.Fatalf("expected 0 when all invalid, got %d", limit)
	}
}

func TestDetectMemoryLimitWithGlobalPaths(t *testing.T) {
	origV2, origV1, origMem := cgroupV2Path, cgroupV1Path, procMeminfoPath
	defer func() {
		cgroupV2Path, cgroupV1Path, procMeminfoPath = origV2, origV1, origMem
	}()

	tempDir := t.TempDir()
	mockV2 := filepath.Join(tempDir, "memory.max")
	_ = os.WriteFile(mockV2, []byte("52428800\n"), 0644) // 50MB

	cgroupV2Path = mockV2
	cgroupV1Path = filepath.Join(tempDir, "non_existent_v1")
	procMeminfoPath = filepath.Join(tempDir, "non_existent_meminfo")

	limit := detectMemoryLimit()
	if limit != 52428800 {
		t.Fatalf("expected 52428800, got %d", limit)
	}
}

func TestApplyLowMemoryProtectionTiers(t *testing.T) {
	oldGOGC := debug.SetGCPercent(100)
	oldMemLimit := debug.SetMemoryLimit(-1)
	defer func() {
		debug.SetGCPercent(oldGOGC)
		debug.SetMemoryLimit(oldMemLimit)
	}()

	tests := []struct {
		name         string
		limitBytes   int64
		wantApplied  bool
		wantMemLimit int64
		wantGC       int
		wantInterval time.Duration
	}{
		{
			name:         "<= 80MB (典型 64MB 容器)",
			limitBytes:   64 * 1024 * 1024,
			wantApplied:  true,
			wantMemLimit: 38 * 1024 * 1024,
			wantGC:       40,
			wantInterval: 60 * time.Second,
		},
		{
			name:         "边界 80MB",
			limitBytes:   80 * 1024 * 1024,
			wantApplied:  true,
			wantMemLimit: 38 * 1024 * 1024,
			wantGC:       40,
			wantInterval: 60 * time.Second,
		},
		{
			name:         "<= 160MB (典型 128MB 容器)",
			limitBytes:   128 * 1024 * 1024,
			wantApplied:  true,
			wantMemLimit: 75 * 1024 * 1024,
			wantGC:       60,
			wantInterval: 120 * time.Second,
		},
		{
			name:         "边界 160MB",
			limitBytes:   160 * 1024 * 1024,
			wantApplied:  true,
			wantMemLimit: 75 * 1024 * 1024,
			wantGC:       60,
			wantInterval: 120 * time.Second,
		},
		{
			name:         "> 160MB (典型 256MB+ 容器/主机，保持默认)",
			limitBytes:   256 * 1024 * 1024,
			wantApplied:  false,
			wantMemLimit: 0,
			wantGC:       0,
			wantInterval: 0,
		},
		{
			name:         "0 或未探测到",
			limitBytes:   0,
			wantApplied:  false,
			wantMemLimit: 0,
			wantGC:       0,
			wantInterval: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stopCh := make(chan struct{})
			defer close(stopCh)

			applied, memLimit, gc, interval := applyLowMemoryProtectionWithStop(tt.limitBytes, stopCh)
			if applied != tt.wantApplied {
				t.Errorf("applied = %v, want %v", applied, tt.wantApplied)
			}
			if memLimit != tt.wantMemLimit {
				t.Errorf("memLimit = %d, want %d", memLimit, tt.wantMemLimit)
			}
			if gc != tt.wantGC {
				t.Errorf("gc = %d, want %d", gc, tt.wantGC)
			}
			if interval != tt.wantInterval {
				t.Errorf("interval = %v, want %v", interval, tt.wantInterval)
			}

			if tt.wantApplied {
				curLimit := debug.SetMemoryLimit(-1)
				if curLimit != tt.wantMemLimit {
					t.Errorf("runtime/debug SetMemoryLimit = %d, want %d", curLimit, tt.wantMemLimit)
				}
				curGC := debug.SetGCPercent(100)
				if curGC != tt.wantGC {
					t.Errorf("runtime/debug SetGCPercent = %d, want %d", curGC, tt.wantGC)
				}
			}
		})
	}
}

func TestInitLowMemoryProtectionIntegration(t *testing.T) {
	origV2, origV1, origMem := cgroupV2Path, cgroupV1Path, procMeminfoPath
	oldGOGC := debug.SetGCPercent(100)
	oldMemLimit := debug.SetMemoryLimit(-1)
	defer func() {
		cgroupV2Path, cgroupV1Path, procMeminfoPath = origV2, origV1, origMem
		debug.SetGCPercent(oldGOGC)
		debug.SetMemoryLimit(oldMemLimit)
	}()

	tempDir := t.TempDir()
	v2File := filepath.Join(tempDir, "memory.max")
	_ = os.WriteFile(v2File, []byte("67108864\n"), 0644) // 64MB

	cgroupV2Path = v2File
	cgroupV1Path = filepath.Join(tempDir, "non_existent")
	procMeminfoPath = filepath.Join(tempDir, "non_existent")

	initLowMemoryProtection()

	curLimit := debug.SetMemoryLimit(-1)
	if curLimit != 38*1024*1024 {
		t.Errorf("initLowMemoryProtection() SetMemoryLimit = %d, want %d", curLimit, 38*1024*1024)
	}
	curGC := debug.SetGCPercent(100)
	if curGC != 40 {
		t.Errorf("initLowMemoryProtection() SetGCPercent = %d, want 40", curGC)
	}
}
