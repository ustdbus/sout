package main

import (
	"testing"
)

func TestFormatExitRemarkAndTunnelNaming(t *testing.T) {
	// 1. 测试 VPN Gate 家宽出口
	t1 := &Tunnel{
		Kind: "vpngate",
		TargetRegion: "SRC:vpngate:ALL",
		TargetPoolType: "all",
		IPType: "residential",
		Node: Node{
			HostName: "vpn315836095",
			CountryCode: "JP",
			Country: "Japan",
		},
	}
	res1 := formatTunnelExitRemark(t1)
	if res1 != "家宽-日本" {
		t.Fatalf("Expected 家宽-日本, got %s", res1)
	}

	t2 := &Tunnel{
		Kind: "vpngate",
		TargetRegion: "SRC:vpngate:US",
		TargetPoolType: "residential",
		Node: Node{
			HostName: "vpn123456",
			CountryCode: "US",
			Country: "United States",
		},
	}
	res2 := formatTunnelExitRemark(t2)
	if res2 != "家宽-美国" {
		t.Fatalf("Expected 家宽-美国, got %s", res2)
	}

	// 2. 测试预设出口 (WARP / Windscribe / Proton / Opera)
	tWarp := &Tunnel{
		Kind: "custom",
		TargetSourceID: "preset-warp",
		TargetRegion: "SRC:preset-warp:HK",
		Node: Node{
			HostName: "cs-wg-warp-hk",
			CountryCode: "HK",
			Country: "Hong Kong",
		},
	}
	resWarp := formatTunnelExitRemark(tWarp)
	if resWarp != "WARP-香港" {
		t.Fatalf("Expected WARP-香港, got %s", resWarp)
	}

	tWind := &Tunnel{
		Kind: "custom",
		TargetSourceID: "preset-windscribe",
		TargetRegion: "Windscribe 洛杉矶",
		Node: Node{
			HostName: "us-west-084.totallyacdn.com",
			CountryCode: "US",
		},
	}
	resWind := formatTunnelExitRemark(tWind)
	if resWind != "Windscribe-洛杉矶" {
		t.Fatalf("Expected Windscribe-洛杉矶, got %s", resWind)
	}

	tProton := &Tunnel{
		Kind: "custom",
		TargetSourceID: "preset-proton",
		TargetRegion: "SRC:preset-proton:JP",
		Node: Node{
			HostName: "jp-free-01.protonvpn.net",
			CountryCode: "JP",
		},
	}
	resProton := formatTunnelExitRemark(tProton)
	if resProton != "Proton-日本" {
		t.Fatalf("Expected Proton-日本, got %s", resProton)
	}

	// 3. 测试自定义订阅源
	if globalCustomStore == nil {
		globalCustomStore = &CustomStore{
			Sources: map[string]*CustomSource{
				"sub1": {ID: "sub1", Name: "魔戒"},
			},
			Nodes: map[string]*CustomNode{},
		}
	} else {
		globalCustomStore.mu.Lock()
		globalCustomStore.Sources["sub1"] = &CustomSource{ID: "sub1", Name: "魔戒"}
		globalCustomStore.mu.Unlock()
	}

	tCustom := &Tunnel{
		Kind: "custom",
		TargetSourceID: "sub1",
		TargetRegion: "SRC:sub1:SG",
		Node: Node{
			HostName: "mojie-sg-01",
			CountryCode: "SG",
			Country: "Singapore",
		},
	}
	resCustom := formatTunnelExitRemark(tCustom)
	if resCustom != "魔戒-新加坡" {
		t.Fatalf("Expected 魔戒-新加坡, got %s", resCustom)
	}

	// 4. 测试 cleanExitName 清洗旧版本残留
	if clean := cleanExitName("Vpngate 日本"); clean != "家宽-日本" {
		t.Fatalf("Expected 家宽-日本, got %s", clean)
	}
	if clean := cleanExitName("Vpngate"); clean != "家宽" {
		t.Fatalf("Expected 家宽, got %s", clean)
	}
	if clean := cleanExitName("魔戒 香港"); clean != "魔戒-香港" {
		t.Fatalf("Expected 魔戒-香港, got %s", clean)
	}
}
