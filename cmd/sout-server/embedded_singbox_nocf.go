//go:build !with_cloudflared

package main

import (
	"github.com/sagernet/sing-box/adapter"
)

func registerCloudflaredInbound(registry adapter.InboundRegistry) {
}
