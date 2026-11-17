//go:build with_cloudflared

package main

import (
	sbinbound "github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/protocol/cloudflare"
	cloudflared "github.com/sagernet/sing-cloudflared"
)

func registerCloudflaredInbound(registry *sbinbound.Registry) {
	cloudflare.RegisterInbound(registry)
}

func syncCloudflaredSlotDefaults(slots []TunnelSlotMeta, primarySlot int) {
	cfSlots := make([]cloudflared.TunnelSlotConfig, len(slots))
	for i, s := range slots {
		cfSlots[i] = cloudflared.TunnelSlotConfig{
			Protocol:  s.Protocol,
			IPVersion: s.IPVersion,
		}
	}
	cloudflared.SetDefaultTunnelSlots(cfSlots, primarySlot)
}

func getCloudflaredSlotsSnapshot() ([]TunnelSlotInfo, bool) {
	snaps, ok := cloudflared.GetTunnelSlotsSnapshot()
	if !ok {
		return nil, false
	}
	out := make([]TunnelSlotInfo, len(snaps))
	for i, s := range snaps {
		out[i] = TunnelSlotInfo{
			Index:        s.Index,
			Connected:    s.Connected,
			Protocol:     s.Protocol,
			CfgProtocol:  s.CfgProtocol,
			IPVersion:    s.IPVersion,
			CfgIPVersion: s.CfgIPVer,
			EdgeAddr:     s.EdgeAddr,
			Retries:      s.Retries,
			Streams:      s.Streams,
			LastActive:   s.LastActive,
			IsPrimary:    s.IsPrimary,
		}
	}
	return out, true
}

func updateCloudflaredSlotConfig(index int, protocol string, ipVersion int, setPrimary bool) {
	cloudflared.UpdateTunnelSlotConfig(index, protocol, ipVersion, setPrimary)
}
