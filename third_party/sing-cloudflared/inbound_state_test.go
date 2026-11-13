package cloudflared

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/quic-go"
	"github.com/sagernet/sing-cloudflared/internal/transport"
	"github.com/sagernet/sing/common/logger"
	N "github.com/sagernet/sing/common/network"

	"github.com/google/uuid"
)

var connectionHookAccess sync.Mutex

func restoreConnectionHooks(t *testing.T) {
	t.Helper()

	connectionHookAccess.Lock()
	originalNewQUICConnection := newQUICConnection
	originalNewHTTP2Connection := newHTTP2Connection
	originalServeQUICConnection := serveQUICConnection
	originalServeHTTP2Connection := serveHTTP2Connection
	originalProbeQUICEdge := probeQUICEdge
	originalQUICRecoveryProbeInterval := quicRecoveryProbeInterval
	originalEdupconnFirstRetryDelay := edupconnFirstRetryDelay
	originalEdupconnRetryDelay := edupconnRetryDelay
	t.Cleanup(func() {
		newQUICConnection = originalNewQUICConnection
		newHTTP2Connection = originalNewHTTP2Connection
		serveQUICConnection = originalServeQUICConnection
		serveHTTP2Connection = originalServeHTTP2Connection
		probeQUICEdge = originalProbeQUICEdge
		quicRecoveryProbeInterval = originalQUICRecoveryProbeInterval
		edupconnFirstRetryDelay = originalEdupconnFirstRetryDelay
		edupconnRetryDelay = originalEdupconnRetryDelay
		connectionHookAccess.Unlock()
	})
}

func TestRecordConnectionFailureAutoFallbackAfterRetryBudget(t *testing.T) {
	t.Parallel()

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = ""
	serviceInstance.initializeConnectionState(0)

	for retry := uint8(1); retry < defaultProtocolRetry; retry++ {
		count, switchedProtocol, switched := serviceInstance.recordConnectionFailure(0, fmt.Errorf("quic failed %d", retry))
		if switched {
			t.Fatalf("unexpected early fallback to %q at retry %d", switchedProtocol, retry)
		}
		if count != retry {
			t.Fatalf("unexpected retry count %d at step %d", count, retry)
		}
		if state := serviceInstance.connectionState(0); state.protocol != protocolQUIC || state.retries != retry {
			t.Fatalf("unexpected state before fallback %#v", state)
		}
	}

	count, switchedProtocol, switched := serviceInstance.recordConnectionFailure(0, errors.New("quic failed"))
	if !switched || switchedProtocol != protocolHTTP2 {
		t.Fatalf("expected fallback to HTTP/2, got switched=%v protocol=%q", switched, switchedProtocol)
	}
	if count != defaultProtocolRetry {
		t.Fatalf("unexpected retry count at fallback %d", count)
	}
	if state := serviceInstance.connectionState(0); state.protocol != protocolHTTP2 || state.retries != 0 {
		t.Fatalf("unexpected state after fallback %#v", state)
	}
}

func TestRecordConnectionFailureFallsBackOnBrokenQUIC(t *testing.T) {
	t.Parallel()

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = ""
	serviceInstance.initializeConnectionState(0)

	// IdleTimeoutError alone should keep trying QUIC edges first before falling back after DefaultProtocolRetry.
	count, switchedProtocol, switched := serviceInstance.recordConnectionFailure(0, &quic.IdleTimeoutError{})
	if switched {
		t.Fatalf("unexpected immediate fallback to %q on single idle timeout", switchedProtocol)
	}
	if count != 1 {
		t.Fatalf("unexpected retry count after idle timeout %d", count)
	}

	// Transport operation not permitted indicates local UDP/QUIC is blocked and falls back immediately.
	count, switchedProtocol, switched = serviceInstance.recordConnectionFailure(0, &quic.TransportError{
		ErrorCode:    quic.InternalError,
		ErrorMessage: "operation not permitted",
	})
	if !switched || switchedProtocol != protocolHTTP2 {
		t.Fatalf("expected immediate fallback on operation not permitted, got switched=%v protocol=%q", switched, switchedProtocol)
	}
	if count != 2 {
		t.Fatalf("unexpected retry count for broken QUIC fallback %d", count)
	}
}

func TestSecondConnectionInitialProtocolUsesSelectorCurrent(t *testing.T) {
	t.Parallel()
	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = ""
	serviceInstance.notifyConnected(0, "http2")
	serviceInstance.initializeConnectionState(1)

	if state := serviceInstance.connectionState(1); state.protocol != protocolQUIC {
		t.Fatalf("expected second connection to start from selector current protocol, got %#v", state)
	}
}

func TestNotifyConnectedResetsRetries(t *testing.T) {
	t.Parallel()
	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = ""
	serviceInstance.initializeConnectionState(0)
	serviceInstance.incrementConnectionRetries(0)
	serviceInstance.incrementConnectionRetries(0)

	serviceInstance.notifyConnected(0, "http2")

	state := serviceInstance.connectionState(0)
	if state.retries != 0 {
		t.Fatalf("expected retries reset after success, got %d", state.retries)
	}
	if state.protocol != protocolQUIC {
		t.Fatalf("expected protocol to return to selector current value, got %q", state.protocol)
	}
}

func TestRecordConnectionFailureExplicitQUICHasNoFallback(t *testing.T) {
	t.Parallel()

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = protocolQUIC
	serviceInstance.initializeConnectionState(0)

	for range defaultProtocolRetry + 2 {
		_, switchedProtocol, switched := serviceInstance.recordConnectionFailure(0, errors.New("quic failed"))
		if switched {
			t.Fatalf("unexpected fallback to %q for explicit QUIC transport", switchedProtocol)
		}
	}

	if state := serviceInstance.connectionState(0); state.protocol != protocolQUIC || state.retries != defaultProtocolRetry {
		t.Fatalf("unexpected explicit QUIC state %#v", state)
	}
}

func TestNotifyConnectedSignalsOnlyOncePerConnection(t *testing.T) {
	t.Parallel()

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.connectedNotify = make(chan uint8, 2)
	serviceInstance.connectedIndices = make(map[uint8]struct{})

	serviceInstance.notifyConnected(0, "http2")
	serviceInstance.notifyConnected(0, "http2")

	select {
	case connected := <-serviceInstance.connectedNotify:
		if connected != 0 {
			t.Fatalf("unexpected connected index %d", connected)
		}
	case <-time.After(time.Second):
		t.Fatal("expected first connected notification")
	}

	select {
	case duplicate := <-serviceInstance.connectedNotify:
		t.Fatalf("unexpected duplicate notification %d", duplicate)
	default:
	}
}

func TestSafeServeConnectionRecoversPanic(t *testing.T) {
	t.Parallel()
	restoreConnectionHooks(t)

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = "quic"
	serviceInstance.initializeConnectionState(0)

	newQUICConnection = func(context.Context, *EdgeAddr, uint8, Credentials, uuid.UUID, string, []string, uint8, time.Duration, N.Dialer, func(), logger.ContextLogger) (*QUICConnection, error) {
		return &QUICConnection{}, nil
	}
	serveQUICConnection = func(*QUICConnection, context.Context, StreamHandler) error {
		panic("boom")
	}

	err := serviceInstance.safeServeConnection(0, &EdgeAddr{})
	if err == nil || !strings.Contains(err.Error(), "panic in serve connection") {
		t.Fatalf("expected recovered panic error, got %v", err)
	}
}

func TestSuperviseConnectionStopsOnPermanentRegistrationError(t *testing.T) {
	t.Parallel()
	restoreConnectionHooks(t)

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = "quic"
	serviceInstance.initializeConnectionState(0)

	permanentErr := &permanentRegistrationError{Err: errors.New("permanent register error")}
	newQUICConnection = func(context.Context, *EdgeAddr, uint8, Credentials, uuid.UUID, string, []string, uint8, time.Duration, N.Dialer, func(), logger.ContextLogger) (*QUICConnection, error) {
		return &QUICConnection{}, nil
	}
	serveQUICConnection = func(*QUICConnection, context.Context, StreamHandler) error {
		return permanentErr
	}

	serviceInstance.done.Add(1)
	done := make(chan struct{})
	go func() {
		serviceInstance.superviseConnection(0, []*EdgeAddr{{}})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected connection supervision to stop")
	}

	if retries := serviceInstance.connectionState(0).retries; retries != 0 {
		t.Fatalf("expected no retries for permanent registration error, got %d", retries)
	}

	select {
	case <-serviceInstance.ctx.Done():
		t.Fatal("expected permanent registration error to stop only this connection")
	default:
	}
}

func TestSuperviseConnectionCancelsServiceOnNonRemoteManagedError(t *testing.T) {
	t.Parallel()
	restoreConnectionHooks(t)

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = "quic"
	serviceInstance.initializeConnectionState(0)

	newQUICConnection = func(context.Context, *EdgeAddr, uint8, Credentials, uuid.UUID, string, []string, uint8, time.Duration, N.Dialer, func(), logger.ContextLogger) (*QUICConnection, error) {
		return &QUICConnection{}, nil
	}
	serveQUICConnection = func(*QUICConnection, context.Context, StreamHandler) error {
		return ErrNonRemoteManagedTunnelUnsupported
	}

	serviceInstance.done.Add(1)
	done := make(chan struct{})
	go func() {
		serviceInstance.superviseConnection(0, []*EdgeAddr{{}})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected connection supervision to stop")
	}

	select {
	case <-serviceInstance.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected service cancellation on non-remote-managed tunnel error")
	}
}

func TestSuperviseConnectionUsesRetryableDelayAndRotatesEdges(t *testing.T) {
	t.Parallel()
	restoreConnectionHooks(t)

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = "quic"
	serviceInstance.initializeConnectionState(0)

	edge1 := &EdgeAddr{}
	edge2 := &EdgeAddr{}
	attempts := make(chan *EdgeAddr, 3)

	var serveCalls int
	serveQUICConnection = func(*QUICConnection, context.Context, StreamHandler) error {
		serveCalls++
		switch serveCalls {
		case 1, 2:
			return &RetryableError{Err: errors.New("retry"), Delay: 20 * time.Millisecond}
		default:
			serviceInstance.cancel()
			return context.Canceled
		}
	}
	newQUICConnection = func(ctx context.Context, edgeAddr *EdgeAddr, connIndex uint8, credentials Credentials, connectorID uuid.UUID, datagramVersion string, features []string, numPreviousAttempts uint8, gracePeriod time.Duration, tunnelDialer N.Dialer, onConnected func(), log logger.ContextLogger) (*QUICConnection, error) {
		attempts <- edgeAddr
		return &QUICConnection{}, nil
	}

	serviceInstance.done.Add(1)
	done := make(chan struct{})
	started := time.Now()
	go func() {
		serviceInstance.superviseConnection(0, []*EdgeAddr{edge1, edge2})
		close(done)
	}()

	var sequence []*EdgeAddr
	for expected := range 3 {
		select {
		case edgeAddr := <-attempts:
			sequence = append(sequence, edgeAddr)
			if expected == 1 && time.Since(started) > 500*time.Millisecond {
				t.Fatalf("expected retryable delay override to retry quickly, elapsed=%v", time.Since(started))
			}
		case <-time.After(time.Second):
			t.Fatalf("expected connection attempt %d", expected+1)
		}
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected supervision loop to stop after cancellation")
	}

	if len(sequence) != 3 || sequence[0] != edge1 || sequence[1] != edge2 || sequence[2] != edge1 {
		t.Fatalf("unexpected edge rotation sequence %#v", sequence)
	}
	if retries := serviceInstance.connectionState(0).retries; retries != 2 {
		t.Fatalf("expected two recorded retries, got %d", retries)
	}
}

func TestSuperviseConnectionSelfHealsOnEDUPCONN(t *testing.T) {
	t.Parallel()
	restoreConnectionHooks(t)

	edupconnFirstRetryDelay = 10 * time.Millisecond
	edupconnRetryDelay = 15 * time.Millisecond

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = ""
	serviceInstance.initializeConnectionState(0)

	edge1 := &EdgeAddr{IPVersion: 6, UDP: &net.UDPAddr{IP: net.ParseIP("2606:4700:a0::1"), Port: 7844}}
	edge2 := &EdgeAddr{IPVersion: 4, UDP: &net.UDPAddr{IP: net.IPv4(198, 41, 192, 7), Port: 7844}}

	var attempts atomic.Int32
	var secondAttemptReplace atomic.Bool
	recoveredCh := make(chan struct{}, 1)
	newQUICConnection = func(context.Context, *EdgeAddr, uint8, Credentials, uuid.UUID, string, []string, uint8, time.Duration, N.Dialer, func(), logger.ContextLogger) (*QUICConnection, error) {
		return &QUICConnection{}, nil
	}
	serveQUICConnection = func(conn *QUICConnection, ctx context.Context, handler StreamHandler) error {
		n := attempts.Add(1)
		if n == 2 {
			secondAttemptReplace.Store(serviceInstance.connectionState(0).replaceExisting)
		}
		if n <= 2 {
			return &permanentRegistrationError{Err: errors.New("register connection: EDUPCONN")}
		}
		serviceInstance.notifyConnected(0, protocolQUIC)
		select {
		case recoveredCh <- struct{}{}:
		default:
		}
		serviceInstance.cancel()
		return ctx.Err()
	}

	serviceInstance.done.Add(1)
	done := make(chan struct{})
	go func() {
		serviceInstance.superviseConnection(0, []*EdgeAddr{edge1, edge2})
		close(done)
	}()
	t.Cleanup(func() {
		serviceInstance.cancel()
		<-done
	})

	select {
	case <-recoveredCh:
	case <-time.After(8 * time.Second):
		t.Fatal("expected EDUPCONN connection to self-heal and reconnect via QUIC")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expected supervision loop to exit after cancellation")
	}

	if !secondAttemptReplace.Load() {
		t.Fatal("expected replaceExisting to be enabled on reconnect after EDUPCONN")
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 QUIC attempts before recovery, got %d", got)
	}
	if state := serviceInstance.connectionState(0); state.protocol != protocolQUIC {
		t.Fatalf("expected protocol to remain %q after EDUPCONN, got %q", protocolQUIC, state.protocol)
	}
}

func TestSuperviseConnectionSwitchesBackFromHTTP2ToQUICOnRecovery(t *testing.T) {
	t.Parallel()
	restoreConnectionHooks(t)

	quicRecoveryProbeInterval = 15 * time.Millisecond

	serviceInstance := newLimitedService(t, 0)
	serviceInstance.protocol = ""
	serviceInstance.initializeConnectionState(0)
	// Simulate connection already in fallback HTTP/2 state after prior QUIC failures.
	serviceInstance.stateAccess.Lock()
	serviceInstance.connectionStates[0].protocol = protocolHTTP2
	serviceInstance.stateAccess.Unlock()

	edgeV6 := &EdgeAddr{IPVersion: 6, UDP: &net.UDPAddr{IP: net.ParseIP("2606:4700:a0::1"), Port: 7844}, TCP: &net.TCPAddr{IP: net.ParseIP("2606:4700:a0::1"), Port: 7844}}
	edgeV4 := &EdgeAddr{IPVersion: 4, UDP: &net.UDPAddr{IP: net.IPv4(198, 41, 192, 7), Port: 7844}, TCP: &net.TCPAddr{IP: net.IPv4(198, 41, 192, 7), Port: 7844}}

	var h2Started atomic.Bool
	var quicReconnectedEdge atomic.Pointer[EdgeAddr]
	switchedToQUIC := make(chan struct{}, 1)

	newHTTP2Connection = func(context.Context, *EdgeAddr, uint8, Credentials, uuid.UUID, []string, uint8, time.Duration, N.Dialer, transport.HTTP2Handler, logger.ContextLogger) (*HTTP2Connection, error) {
		return &HTTP2Connection{}, nil
	}
	serveHTTP2Connection = func(conn *HTTP2Connection, ctx context.Context) error {
		h2Started.Store(true)
		serviceInstance.notifyConnected(0, protocolHTTP2)
		<-ctx.Done()
		return ctx.Err()
	}

	var probeCalls atomic.Int32
	probeQUICEdge = func(ctx context.Context, edgeAddr *EdgeAddr, features []string, tunnelDialer N.Dialer) error {
		if probeCalls.Add(1) <= 2 || edgeAddr.IPVersion != 4 {
			return errors.New("quic still blocked")
		}
		return nil
	}

	newQUICConnection = func(ctx context.Context, edgeAddr *EdgeAddr, connIndex uint8, credentials Credentials, connectorID uuid.UUID, datagramVersion string, features []string, numPreviousAttempts uint8, gracePeriod time.Duration, tunnelDialer N.Dialer, onConnected func(), log logger.ContextLogger) (*QUICConnection, error) {
		quicReconnectedEdge.Store(edgeAddr)
		return &QUICConnection{}, nil
	}
	serveQUICConnection = func(conn *QUICConnection, ctx context.Context, handler StreamHandler) error {
		serviceInstance.notifyConnected(0, protocolQUIC)
		select {
		case switchedToQUIC <- struct{}{}:
		default:
		}
		serviceInstance.cancel()
		return ctx.Err()
	}

	serviceInstance.done.Add(1)
	done := make(chan struct{})
	go func() {
		serviceInstance.superviseConnection(0, []*EdgeAddr{edgeV6, edgeV4})
		close(done)
	}()
	t.Cleanup(func() {
		serviceInstance.cancel()
		<-done
	})

	select {
	case <-switchedToQUIC:
	case <-time.After(3 * time.Second):
		t.Fatal("expected HTTP/2 fallback connection to probe and switch back to QUIC")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expected supervision loop to exit after switchback")
	}

	if !h2Started.Load() {
		t.Fatal("expected fallback HTTP/2 connection to run before recovery")
	}
	if reconnected := quicReconnectedEdge.Load(); reconnected != edgeV4 {
		t.Fatalf("expected QUIC reconnection to target recovered edgeV4, got %#v", reconnected)
	}
}
