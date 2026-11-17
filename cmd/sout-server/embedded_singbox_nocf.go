//go:build !with_cloudflared

package main

import (
	sbinbound "github.com/sagernet/sing-box/adapter/inbound"
)

func registerCloudflaredInbound(registry *sbinbound.Registry) {
}

func syncCloudflaredSlotDefaults(slots []TunnelSlotMeta, primarySlot int) {
}

func getCloudflaredSlotsSnapshot() ([]TunnelSlotInfo, bool) {
	return nil, false
}

func updateCloudflaredSlotConfig(index int, protocol string, ipVersion int, setPrimary bool) {
}
