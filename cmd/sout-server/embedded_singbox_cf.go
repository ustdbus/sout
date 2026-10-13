//go:build with_cloudflared

package main

import (
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/protocol/cloudflare"
)

func registerCloudflaredInbound(registry adapter.InboundRegistry) {
	cloudflare.RegisterInbound(registry)
}
