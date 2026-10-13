//go:build !with_cloudflared

package main

import (
	sbinbound "github.com/sagernet/sing-box/adapter/inbound"
)

func registerCloudflaredInbound(registry *sbinbound.Registry) {
}
