package cloudflared

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-cloudflared/internal/discovery"
	"github.com/sagernet/sing-cloudflared/internal/transport"
	"github.com/sagernet/sing/common/logger"
	N "github.com/sagernet/sing/common/network"

	"github.com/google/uuid"
)

func TestPickEdgeAddrIndex_RespectsIPVersionPreference(t *testing.T) {
	addrs := []*discovery.EdgeAddr{
		{TCP: &net.TCPAddr{IP: net.IPv4(198, 41, 192, 1), Port: 7844}, IPVersion: 4},
		{TCP: &net.TCPAddr{IP: net.ParseIP("2606:4700::1"), Port: 7844}, IPVersion: 6},
		{TCP: &net.TCPAddr{IP: net.IPv4(198, 41, 192, 2), Port: 7844}, IPVersion: 4},
	}

	if got := pickEdgeAddrIndex(addrs, 0, 6); got != 1 {
		t.Fatalf("expected IPv6 index 1, got %d", got)
	}
	if got := pickEdgeAddrIndex(addrs, 1, 4); got != 2 {
		t.Fatalf("expected IPv4 index 2, got %d", got)
	}
	if got := pickEdgeAddrIndex(addrs, 1, 0); got != 1 {
		t.Fatalf("expected unchanged index 1 for auto, got %d", got)
	}
}

func TestServiceSlotsSnapshotAndUpdate(t *testing.T) {
	originalDiscoverEdge := discoverEdge
	originalNewQUICConnection := newQUICConnection
	originalServeQUICConnection := serveQUICConnection
	originalNewHTTP2Connection := newHTTP2Connection
	originalServeHTTP2Connection := serveHTTP2Connection
	defer func() {
		discoverEdge = originalDiscoverEdge
		newQUICConnection = originalNewQUICConnection
		serveQUICConnection = originalServeQUICConnection
		newHTTP2Connection = originalNewHTTP2Connection
		serveHTTP2Connection = originalServeHTTP2Connection
		SetDefaultTunnelSlots(nil, -1)
	}()

	SetDefaultTunnelSlots([]TunnelSlotConfig{
		{Protocol: transport.ProtocolQUIC, IPVersion: 4},
		{Protocol: transport.ProtocolHTTP2, IPVersion: 6},
	}, 0)

	discoverEdge = func(ctx context.Context, region string, controlDialer N.Dialer, controlResolver Resolver, tunnelResolver Resolver) ([][]*EdgeAddr, error) {
		return [][]*EdgeAddr{{
			{TCP: &net.TCPAddr{IP: net.IPv4(198, 41, 192, 10), Port: 7844}, UDP: &net.UDPAddr{IP: net.IPv4(198, 41, 192, 10), Port: 7844}, IPVersion: 4},
			{TCP: &net.TCPAddr{IP: net.ParseIP("2606:4700::10"), Port: 7844}, UDP: &net.UDPAddr{IP: net.ParseIP("2606:4700::10"), Port: 7844}, IPVersion: 6},
			{TCP: &net.TCPAddr{IP: net.IPv4(198, 41, 192, 11), Port: 7844}, UDP: &net.UDPAddr{IP: net.IPv4(198, 41, 192, 11), Port: 7844}, IPVersion: 4},
			{TCP: &net.TCPAddr{IP: net.ParseIP("2606:4700::11"), Port: 7844}, UDP: &net.UDPAddr{IP: net.ParseIP("2606:4700::11"), Port: 7844}, IPVersion: 6},
		}}, nil
	}

	newQUICConnection = func(
		ctx context.Context,
		edgeAddr *EdgeAddr,
		connIndex uint8,
		credentials Credentials,
		connectorID uuid.UUID,
		datagramVersion string,
		features []string,
		numPreviousAttempts uint8,
		gracePeriod time.Duration,
		tunnelDialer N.Dialer,
		onConnected func(),
		log logger.ContextLogger,
	) (*QUICConnection, error) {
		if onConnected != nil {
			onConnected()
		}
		return &QUICConnection{}, nil
	}
	serveQUICConnection = func(connection *QUICConnection, ctx context.Context, handler StreamHandler) error {
		<-ctx.Done()
		return ctx.Err()
	}
	newHTTP2Connection = func(
		ctx context.Context,
		edgeAddr *EdgeAddr,
		connIndex uint8,
		credentials Credentials,
		connectorID uuid.UUID,
		features []string,
		numPreviousAttempts uint8,
		gracePeriod time.Duration,
		tunnelDialer N.Dialer,
		handler transport.HTTP2Handler,
		log logger.ContextLogger,
	) (*HTTP2Connection, error) {
		if handler != nil {
			handler.NotifyConnected(connIndex, transport.ProtocolHTTP2)
		}
		return &HTTP2Connection{}, nil
	}
	serveHTTP2Connection = func(connection *HTTP2Connection, ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	svc := newTestService(t, testToken(t), protocolQUIC, 4)
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// Wait briefly for goroutines to register initial connection state
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snaps, ok := GetTunnelSlotsSnapshot()
		if ok && len(snaps) == 4 && snaps[0].Connected && snaps[1].Connected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	snaps, ok := GetTunnelSlotsSnapshot()
	if !ok || len(snaps) != 4 {
		t.Fatalf("expected 4 slot snapshots, got ok=%v len=%d", ok, len(snaps))
	}
	if !snaps[0].IsPrimary || snaps[0].Protocol != transport.ProtocolQUIC || snaps[0].IPVersion != 4 {
		t.Fatalf("unexpected slot 0 snapshot: %+v", snaps[0])
	}
	if snaps[1].Protocol != transport.ProtocolHTTP2 || snaps[1].IPVersion != 6 {
		t.Fatalf("unexpected slot 1 snapshot: %+v", snaps[1])
	}

	// Reconfigure slot 2 to HTTP2 + IPv6 + Primary
	UpdateTunnelSlotConfig(2, transport.ProtocolHTTP2, 6, true)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snaps, _ = GetTunnelSlotsSnapshot()
		if snaps[2].IsPrimary && snaps[2].Protocol == transport.ProtocolHTTP2 && snaps[2].IPVersion == 6 && snaps[2].Connected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	snaps, _ = GetTunnelSlotsSnapshot()
	if !snaps[2].IsPrimary || snaps[2].Protocol != transport.ProtocolHTTP2 || snaps[2].IPVersion != 6 {
		t.Fatalf("expected slot 2 reconfigured as HTTP2 IPv6 primary, got %+v", snaps[2])
	}
}
