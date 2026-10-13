//go:build with_cloudflared

package main

import (
	sbinbound "github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/protocol/cloudflare"
)

func registerCloudflaredInbound(registry *sbinbound.Registry) {
	cloudflare.RegisterInbound(registry)
}
