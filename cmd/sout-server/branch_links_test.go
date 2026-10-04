package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestInboundBranchLinksDirect(t *testing.T) {
	sb := &SingBox{}
	ibMap := map[string]any{
		"tag":         "vless-argo-o8px",
		"type":        "vless",
		"listen":      "127.0.0.1",
		"listen_port": 33465,
		"transport": map[string]any{
			"type": "ws",
			"path": "/vlws1a9bf90b",
			"headers": map[string]any{
				"Host": "dg1.111.dpdns.org",
			},
		},
		"users": []any{
			map[string]any{
				"name": "",
				"uuid": "2bf48194-685c-4305-a120-758dbacd2b55",
			},
			map[string]any{
				"name": "soutu1000vpn234562063",
				"uuid": "35d2f660-440b-43f1-8731-fa930ba43c3a",
			},
		},
	}

	cfg := map[string]any{
		"inbounds": []any{ibMap},
	}

	// 模拟 InboundBranchLinks 的内部循环
	baseID := 1000
	clientID := 1002
	clientTag := "vless-argo-o8px (Vpngate 日本)"

	inboundsRaw, _ := cfg["inbounds"].([]any)
	idx := (baseID / 1000) - 1
	targetIb := inboundsRaw[idx].(map[string]any)
	usersRaw, _ := targetIb["users"].([]any)
	isBaseBranch := (clientID == 0)

	var matchedUser map[string]any
	for cIdx, uRaw := range usersRaw {
		uMap := uRaw.(map[string]any)
		uName, _ := uMap["name"].(string)
		curClientID := baseID + (cIdx + 1)
		match := false

		if isBaseBranch {
			if uName == "default" || (!strings.HasPrefix(uName, "soutu") && cIdx == 0) {
				match = true
			}
		} else {
			if clientID > 0 && curClientID == clientID {
				match = true
			} else if clientTag != "" && uName != "" && (uName == clientTag || strings.Contains(clientTag, uName)) {
				match = true
			}
		}
		if match {
			matchedUser = uMap
			break
		}
	}

	if matchedUser == nil {
		t.Fatalf("No user matched!")
	}

	t.Logf("Matched user: name=%s, uuid=%s", matchedUser["name"], matchedUser["uuid"])
	if matchedUser["uuid"] != "35d2f660-440b-43f1-8731-fa930ba43c3a" {
		t.Fatalf("Expected branch user UUID 35d2f660..., got %s", matchedUser["uuid"])
	}

	links := sb.buildLinksForUser("vless", "vless-argo-o8px", 33465, targetIb, matchedUser, "dg1.111.dpdns.org", nil, clientTag)
	if len(links) == 0 {
		t.Fatalf("No links generated")
	}
	u, _ := url.Parse(links[0])
	t.Logf("Generated link: %s", links[0])
	t.Logf("User in link: %s", u.User.Username())
	if u.User.Username() != "35d2f660-440b-43f1-8731-fa930ba43c3a" {
		t.Fatalf("Link has wrong UUID: %s", u.User.Username())
	}
}
