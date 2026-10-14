package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdoptLegacySlotCredentials(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "sout-cred-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	InitCredentials(tmpDir)

	// 1. 模拟生成新 Slot 规范凭据
	uuid1, pass1 := GetOrCreateSlotCredentials(1, "soutu1000_s1")
	if uuid1 == "" || pass1 == "" {
		t.Fatalf("GetOrCreateSlotCredentials returned empty: uuid=%s, pass=%s", uuid1, pass1)
	}

	// 2. 模拟老客户端传入老 UUID 与密码进行固化继承
	legacyUUID := "0f3f0749-0000-4000-8000-000000000001"
	legacyPass := "oldPassword12345"
	changed := AdoptLegacySlotCredentials(1, legacyUUID, legacyPass, "soutu1000vpn250920860")
	if !changed {
		t.Fatalf("AdoptLegacySlotCredentials expected changed=true")
	}

	// 3. 再次查询 slot 1 凭据，必须永久返回继承的老 UUID 与老密码
	uuidAfter, passAfter := GetOrCreateSlotCredentials(1, "soutu1000_s1")
	if uuidAfter != legacyUUID {
		t.Errorf("UUID mismatch after adoption: got %s, want %s", uuidAfter, legacyUUID)
	}
	if passAfter != legacyPass {
		t.Errorf("Password mismatch after adoption: got %s, want %s", passAfter, legacyPass)
	}

	// 4. 重复调用相同凭据幂等返回 false
	if AdoptLegacySlotCredentials(1, legacyUUID, legacyPass, "soutu1000_s1") {
		t.Errorf("expected changed=false on re-adopting identical credentials")
	}
}

func TestCleanupAndMigrateLegacyClients(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "sout-sb-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	InitCredentials(tmpDir)
	sb := &SingBox{
		workDir:    tmpDir,
		configPath: filepath.Join(tmpDir, "singbox_config.json"),
		lastTunnels: []*Tunnel{
			{
				Slot: 1,
				Node: Node{HostName: "vpn250920860"},
			},
		},
	}

	legacyUUID := "0f3f0749-legacy-uuid"
	newUUID := "d31308ff-new-uuid"

	cfg := map[string]any{
		"inbounds": []any{
			map[string]any{
				"tag":  "vless-in",
				"type": "vless",
				"users": []any{
					map[string]any{
						"name": "default",
						"uuid": "direct-uuid",
					},
					map[string]any{
						"name": "soutu1000vpn250920860",
						"uuid": legacyUUID,
					},
					map[string]any{
						"name": "soutu1000_s1",
						"uuid": newUUID,
					},
				},
			},
		},
		"route": map[string]any{
			"rules": []any{
				map[string]any{
					"auth_user": []any{"soutu1000vpn250920860"},
					"outbound":  "soutvpn250920860",
				},
				map[string]any{
					"auth_user": []any{"soutu1000_s1"},
					"outbound":  "soutvpn250920860",
				},
			},
		},
	}

	changed := sb.cleanupAndMigrateLegacyClients(cfg)
	if !changed {
		t.Fatalf("expected cleanupAndMigrateLegacyClients to return true")
	}

	// 验证旧客户端已从 users 移除，仅保留规范名且已继承旧 UUID
	inbounds := cfg["inbounds"].([]any)
	ib0 := inbounds[0].(map[string]any)
	users := ib0["users"].([]any)
	if len(users) != 2 {
		t.Fatalf("expected 2 users (default and soutu1000_s1), got %d: %+v", len(users), users)
	}

	u1 := users[1].(map[string]any)
	if u1["name"] != "soutu1000_s1" {
		t.Errorf("expected user name soutu1000_s1, got %v", u1["name"])
	}
	if u1["uuid"] != legacyUUID {
		t.Errorf("expected inherited uuid %s, got %v", legacyUUID, u1["uuid"])
	}

	// 验证 route rules 去重与旧用户名替换
	route := cfg["route"].(map[string]any)
	rules := route["rules"].([]any)
	foundS1 := 0
	for _, r := range rules {
		rMap := r.(map[string]any)
		if authUsers, ok := rMap["auth_user"].([]any); ok {
			for _, au := range authUsers {
				if au == "soutu1000_s1" {
					foundS1++
				}
				if au == "soutu1000vpn250920860" {
					t.Errorf("legacy client name still in route rules: %v", au)
				}
			}
		}
	}
	if foundS1 != 1 {
		t.Errorf("expected soutu1000_s1 to appear exactly once in route rules, got %d", foundS1)
	}

	// 验证 credentials 存储中永久保持该 legacyUUID
	credUUID, _ := GetOrCreateSlotCredentials(1, "soutu1000_s1")
	if credUUID != legacyUUID {
		t.Errorf("credentials.json did not retain legacy UUID: got %s, want %s", credUUID, legacyUUID)
	}
}
