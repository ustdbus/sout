package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// SlotCredential 出口槽位的固化凭据
type SlotCredential struct {
	Slot     int    `json:"slot"`
	UUID     string `json:"uuid"`
	Password string `json:"password"`
	Remark   string `json:"remark,omitempty"`
}

// GlobalCredentials 全局持久化凭据集合
type GlobalCredentials struct {
	DirectUUID      string                    `json:"direct_uuid"`
	WarpUUID        string                    `json:"warp_uuid"`
	SlotCredentials map[string]SlotCredential `json:"slot_credentials"` // key 为 slot 字符串如 "1", "2"
}

var (
	credMu      sync.Mutex
	credWorkDir string
	globalCreds *GlobalCredentials
)

// InitCredentials 初始化凭据存储模块
func InitCredentials(workDir string) {
	credMu.Lock()
	defer credMu.Unlock()
	if workDir != "" {
		credWorkDir = workDir
	}
	loadCredentialsLocked()
}

func credentialsFilePath() string {
	dir := credWorkDir
	if dir == "" {
		dir = panelState.workDir
	}
	if dir == "" {
		dir = "/var/lib/sout"
	}
	return filepath.Join(dir, "credentials.json")
}

func loadCredentialsLocked() {
	p := credentialsFilePath()
	data, err := os.ReadFile(p)
	if err != nil {
		if globalCreds == nil {
			globalCreds = &GlobalCredentials{
				SlotCredentials: make(map[string]SlotCredential),
			}
		}
		return
	}
	var creds GlobalCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		if globalCreds == nil {
			globalCreds = &GlobalCredentials{
				SlotCredentials: make(map[string]SlotCredential),
			}
		}
		return
	}
	if creds.SlotCredentials == nil {
		creds.SlotCredentials = make(map[string]SlotCredential)
	}
	globalCreds = &creds
}

func saveCredentialsLocked() error {
	if globalCreds == nil {
		return nil
	}
	p := credentialsFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(globalCreds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
}

// GetOrCreateDirectUUID 获取或创建母节点直连分支的固化 UUID
func GetOrCreateDirectUUID(defaultUUID string) string {
	credMu.Lock()
	defer credMu.Unlock()
	if globalCreds == nil {
		loadCredentialsLocked()
	}
	if globalCreds.DirectUUID != "" {
		return globalCreds.DirectUUID
	}
	if defaultUUID != "" {
		globalCreds.DirectUUID = defaultUUID
	} else {
		globalCreds.DirectUUID = generateUUID()
	}
	_ = saveCredentialsLocked()
	return globalCreds.DirectUUID
}

// GetOrCreateSlotCredentials 根据 slot 读取固化凭据；若该 slot 尚无凭据，则生成全新 UUID 与 16 位随机密码并持久化保存。多次调用同一个 slot 必须返回完全相同的 UUID 和密码！
func GetOrCreateSlotCredentials(slot int, remark string) (string, string) {
	if slot <= 0 {
		return generateUUID(), generateRandomPassword(16)
	}
	credMu.Lock()
	defer credMu.Unlock()
	if globalCreds == nil {
		loadCredentialsLocked()
	}
	if globalCreds.SlotCredentials == nil {
		globalCreds.SlotCredentials = make(map[string]SlotCredential)
	}
	key := strconv.Itoa(slot)
	if sc, ok := globalCreds.SlotCredentials[key]; ok && sc.UUID != "" && sc.Password != "" {
		if remark != "" && sc.Remark != remark {
			sc.Remark = remark
			globalCreds.SlotCredentials[key] = sc
			_ = saveCredentialsLocked()
		}
		return sc.UUID, sc.Password
	}

	newUUID := generateUUID()
	newPass := generateRandomPassword(16)
	sc := SlotCredential{
		Slot:     slot,
		UUID:     newUUID,
		Password: newPass,
		Remark:   remark,
	}
	globalCreds.SlotCredentials[key] = sc
	_ = saveCredentialsLocked()
	return newUUID, newPass
}

// ReleaseSlotCredentials 支持标记或释放指定 slot 的凭据
func ReleaseSlotCredentials(slot int) {
	if slot <= 0 {
		return
	}
	credMu.Lock()
	defer credMu.Unlock()
	if globalCreds == nil {
		loadCredentialsLocked()
	}
	if globalCreds.SlotCredentials == nil {
		return
	}
	key := strconv.Itoa(slot)
	if _, ok := globalCreds.SlotCredentials[key]; ok {
		delete(globalCreds.SlotCredentials, key)
		_ = saveCredentialsLocked()
	}
}

// GetSlotCredential 查询指定 slot 的持久化凭据
func GetSlotCredential(slot int) (SlotCredential, bool) {
	if slot <= 0 {
		return SlotCredential{}, false
	}
	credMu.Lock()
	defer credMu.Unlock()
	if globalCreds == nil {
		loadCredentialsLocked()
	}
	if globalCreds.SlotCredentials == nil {
		return SlotCredential{}, false
	}
	sc, ok := globalCreds.SlotCredentials[strconv.Itoa(slot)]
	return sc, ok
}

// parseSlotFromClientName 从 soutu%d_s%d 命名规则中解析 Slot 编号
func parseSlotFromClientName(name string) int {
	if idx := strings.LastIndex(name, "_s"); idx != -1 {
		if s, err := strconv.Atoi(name[idx+2:]); err == nil && s > 0 {
			return s
		}
	}
	return 0
}
