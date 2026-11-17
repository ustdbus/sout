package cloudflared

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-cloudflared/internal/config"
	"github.com/sagernet/sing-cloudflared/internal/control"
	"github.com/sagernet/sing-cloudflared/internal/datagram"
	"github.com/sagernet/sing-cloudflared/internal/discovery"
	"github.com/sagernet/sing-cloudflared/internal/protocol"
	"github.com/sagernet/sing-cloudflared/internal/transport"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/logger"
	N "github.com/sagernet/sing/common/network"

	"github.com/google/uuid"
)

var (
	discoverEdge        = discovery.DiscoverEdge
	newQUICConnection   = transport.NewQUICConnection
	newHTTP2Connection  = transport.NewHTTP2Connection
	serveQUICConnection = func(connection *transport.QUICConnection, ctx context.Context, handler transport.StreamHandler) error {
		return connection.Serve(ctx, handler)
	}
	serveHTTP2Connection = func(connection *transport.HTTP2Connection, ctx context.Context) error {
		return connection.Serve(ctx)
	}
	probeQUICEdge             = transport.ProbeQUIC
	quicRecoveryProbeInterval = 3 * time.Minute
	edupconnFirstRetryDelay   = 5 * time.Second
	edupconnRetryDelay        = 15 * time.Second
	errSwitchBackToQUIC       = errors.New("switch back to QUIC")
	errSlotReconfigured       = errors.New("slot reconfigured")
)

type TunnelSlotConfig struct {
	Protocol  string `json:"protocol"`   // "" | "quic" | "http2"
	IPVersion int    `json:"ip_version"` // 0 | 4 | 6
}

type TunnelSlotSnapshot struct {
	Index       int    `json:"index"`
	Connected   bool   `json:"connected"`
	Protocol    string `json:"protocol"`
	CfgProtocol string `json:"cfg_protocol"`
	IPVersion   int    `json:"ip_version"`
	CfgIPVer    int    `json:"cfg_ip_version"`
	EdgeAddr    string `json:"edge_addr"`
	Retries     int    `json:"retries"`
	Streams     uint64 `json:"streams"`
	LastActive  int64  `json:"last_active"`
	IsPrimary   bool   `json:"is_primary"`
}

var (
	slotRegistryMu       sync.Mutex
	activeService        *Service
	slotOverrides        [4]TunnelSlotConfig
	preferredPrimarySlot = -1
)

func registerActiveService(s *Service) {
	slotRegistryMu.Lock()
	activeService = s
	slotRegistryMu.Unlock()
}

func unregisterActiveService(s *Service) {
	slotRegistryMu.Lock()
	if activeService == s {
		activeService = nil
	}
	slotRegistryMu.Unlock()
}

func getSlotOverride(connIndex uint8) TunnelSlotConfig {
	slotRegistryMu.Lock()
	defer slotRegistryMu.Unlock()
	if int(connIndex) < len(slotOverrides) {
		return slotOverrides[connIndex]
	}
	return TunnelSlotConfig{}
}

func SetDefaultTunnelSlots(configs []TunnelSlotConfig, primaryIndex int) {
	slotRegistryMu.Lock()
	for i := 0; i < len(slotOverrides); i++ {
		if i < len(configs) {
			slotOverrides[i] = configs[i]
		} else {
			slotOverrides[i] = TunnelSlotConfig{}
		}
	}
	if primaryIndex >= 0 && primaryIndex < len(slotOverrides) {
		preferredPrimarySlot = primaryIndex
	} else {
		preferredPrimarySlot = -1
	}
	slotRegistryMu.Unlock()
}

func UpdateTunnelSlotConfig(index int, protocol string, ipVersion int, setPrimary bool) {
	if index < 0 || index >= len(slotOverrides) {
		return
	}
	if protocol != "" && protocol != transport.ProtocolQUIC && protocol != transport.ProtocolHTTP2 {
		protocol = ""
	}
	if ipVersion != 0 && ipVersion != 4 && ipVersion != 6 {
		ipVersion = 0
	}
	slotRegistryMu.Lock()
	slotOverrides[index] = TunnelSlotConfig{
		Protocol:  protocol,
		IPVersion: ipVersion,
	}
	if setPrimary {
		preferredPrimarySlot = index
	}
	svc := activeService
	slotRegistryMu.Unlock()

	if svc != nil {
		svc.reconfigureSlot(uint8(index), protocol)
	}
}

func GetTunnelSlotsSnapshot() ([]TunnelSlotSnapshot, bool) {
	slotRegistryMu.Lock()
	svc := activeService
	overrides := slotOverrides
	prefPrimary := preferredPrimarySlot
	slotRegistryMu.Unlock()

	if svc == nil {
		return nil, false
	}
	return svc.snapshotSlots(overrides, prefPrimary), true
}

type Service struct {
	ctx              context.Context
	cancel           context.CancelFunc
	logger           logger.ContextLogger
	connectionDialer N.Dialer
	icmpHandler      ICMPHandler
	connContext      func(context.Context) context.Context
	clientVersion    string
	credentials      protocol.Credentials
	connectorID      uuid.UUID
	haConnections    int
	protocol         string
	postQuantum      bool
	protocolSelector transport.ProtocolSelector
	region           string
	edgeIPVersion    int
	datagramVersion  string
	featureSelector  *transport.FeatureSelector
	gracePeriod      time.Duration
	configManager    *config.ConfigManager
	flowLimiter      *datagram.FlowLimiter
	accessCache      *accessValidatorCache
	controlDialer    N.Dialer
	tunnelDialer     N.Dialer
	controlResolver  Resolver
	tunnelResolver   Resolver

	connectionAccess sync.Mutex
	connections      []io.Closer
	done             sync.WaitGroup

	datagramMuxerAccess sync.Mutex
	datagramV2Muxers    map[protocol.DatagramSender]*datagram.DatagramV2Muxer
	datagramV3Muxers    map[protocol.DatagramSender]*datagram.DatagramV3Muxer
	datagramV3Manager   *datagram.DatagramV3SessionManager

	connectedAccess  sync.Mutex
	connectedIndices map[uint8]struct{}
	connectedNotify  chan uint8

	stateAccess      sync.Mutex
	connectionStates []connectionState

	directTransportAccess sync.Mutex
	directTransports      map[string]*http.Transport
}

type connectionState struct {
	protocol           string
	retries            uint8
	replaceExisting    bool
	hasConnected       bool
	currentlyConnected bool
	edgeAddr           string
	ipVersion          int
	streams            uint64
	lastActive         int64
	cancelConn         context.CancelCauseFunc
}

func connectionRetryDecision(err error) (retry bool, cancelAll bool) {
	switch {
	case err == nil:
		return false, false
	case E.IsMulti(err, control.ErrNonRemoteManagedTunnelUnsupported):
		return false, true
	case control.IsDuplicateConnectionError(err):
		return true, false
	case control.IsPermanentRegistrationError(err):
		return false, false
	default:
		return true, false
	}
}

func NewService(options ServiceOptions) (*Service, error) {
	if options.Token == "" {
		return nil, E.New("missing token")
	}
	credentials, err := parseToken(options.Token)
	if err != nil {
		return nil, E.Cause(err, "parse token")
	}

	haConnections := options.HAConnections
	if haConnections <= 0 {
		haConnections = 4
	}

	serviceLogger := options.Logger
	if serviceLogger == nil {
		serviceLogger = logger.NOP()
	}

	normalizedProtocol, err := transport.NormalizeProtocol(options.Protocol)
	if err != nil {
		return nil, err
	}
	selector, err := transport.NewProtocolSelector(normalizedProtocol, options.PostQuantum)
	if err != nil {
		return nil, err
	}
	if options.Protocol == transport.ProtocolH2MUX {
		serviceLogger.Warn("h2mux is no longer supported, using HTTP/2 instead")
	}

	edgeIPVersion := options.EdgeIPVersion
	if edgeIPVersion != 0 && edgeIPVersion != 4 && edgeIPVersion != 6 {
		return nil, E.New("unsupported edge_ip_version: ", edgeIPVersion, ", expected 0, 4 or 6")
	}

	datagramVersion := options.DatagramVersion
	if datagramVersion != "" && datagramVersion != protocol.DefaultDatagramVersion && datagramVersion != protocol.DatagramVersionV3 {
		return nil, E.New("unsupported datagram_version: ", datagramVersion, ", expected v2 or v3")
	}

	gracePeriod := options.GracePeriod
	if gracePeriod <= 0 {
		gracePeriod = 30 * time.Second
	}

	configManager, err := config.NewConfigManager()
	if err != nil {
		return nil, E.Cause(err, "build cloudflared runtime config")
	}

	controlDialer := options.ControlDialer
	if controlDialer == nil {
		controlDialer = N.SystemDialer
	}
	tunnelDialer := options.TunnelDialer
	if tunnelDialer == nil {
		tunnelDialer = N.SystemDialer
	}

	region := options.Region
	if region != "" && credentials.Endpoint != "" {
		return nil, E.New("region cannot be specified when credentials already include an endpoint")
	}
	if region == "" {
		region = credentials.Endpoint
	}

	clientVersion := options.ClientVersion
	if clientVersion == "" {
		clientVersion = "sing-cloudflared"
	}

	serviceCtx, cancel := context.WithCancel(context.Background())

	return &Service{
		ctx:               serviceCtx,
		cancel:            cancel,
		logger:            serviceLogger,
		connectionDialer:  options.ConnectionDialer,
		icmpHandler:       options.ICMPHandler,
		connContext:       options.ConnContext,
		clientVersion:     clientVersion,
		credentials:       credentials,
		connectorID:       uuid.New(),
		haConnections:     haConnections,
		protocol:          normalizedProtocol,
		postQuantum:       options.PostQuantum,
		protocolSelector:  selector,
		region:            region,
		edgeIPVersion:     edgeIPVersion,
		datagramVersion:   datagramVersion,
		featureSelector:   transport.NewFeatureSelector(credentials.AccountTag, datagramVersion),
		gracePeriod:       gracePeriod,
		configManager:     configManager,
		flowLimiter:       &datagram.FlowLimiter{},
		accessCache:       &accessValidatorCache{values: make(map[string]accessValidator), dialer: controlDialer},
		controlDialer:     controlDialer,
		tunnelDialer:      tunnelDialer,
		controlResolver:   options.ControlResolver,
		tunnelResolver:    options.TunnelResolver,
		datagramV2Muxers:  make(map[protocol.DatagramSender]*datagram.DatagramV2Muxer),
		datagramV3Muxers:  make(map[protocol.DatagramSender]*datagram.DatagramV3Muxer),
		datagramV3Manager: datagram.NewDatagramV3SessionManager(),
		connectedIndices:  make(map[uint8]struct{}),
		connectedNotify:   make(chan uint8, haConnections),
		connectionStates:  make([]connectionState, haConnections),
		directTransports:  make(map[string]*http.Transport),
	}, nil
}

func (s *Service) Start() error {
	registerActiveService(s)
	s.logger.Info("starting Cloudflare Tunnel with ", s.haConnections, " HA connections")
	s.featureSelector.Start(s.ctx)

	regions, err := discoverEdge(s.ctx, s.region, s.controlDialer, s.controlResolver, s.tunnelResolver)
	if err != nil {
		return E.Cause(err, "discover edge")
	}
	regions = discovery.FilterByIPVersion(regions, s.edgeIPVersion)
	edgeAddrs := flattenRegions(regions)
	if len(edgeAddrs) == 0 {
		return E.New("no edge addresses available")
	}
	if cappedHAConnections := effectiveHAConnections(s.haConnections, len(edgeAddrs)); cappedHAConnections != s.haConnections {
		s.logger.Info("requested ", s.haConnections, " HA connections but only ", cappedHAConnections, " edge addresses are available")
		s.haConnections = cappedHAConnections
	}

	for connIndex := 0; connIndex < s.haConnections; connIndex++ {
		s.initializeConnectionState(uint8(connIndex))
		s.done.Add(1)
		go s.superviseConnection(uint8(connIndex), edgeAddrs)
		select {
		case readyConnIndex := <-s.connectedNotify:
			if readyConnIndex != uint8(connIndex) {
				s.logger.Debug("received unexpected ready notification for connection ", readyConnIndex)
			}
		case <-time.After(firstConnectionReadyTimeout):
		case <-s.ctx.Done():
			if connIndex == 0 {
				return s.ctx.Err()
			}
			return nil
		}
	}
	return nil
}

func (s *Service) notifyConnected(connIndex uint8, connProtocol string) {
	s.stateAccess.Lock()
	s.ensureConnectionStateLocked(connIndex)
	state := s.connectionStates[connIndex]
	state.retries = 0
	if connProtocol != "" {
		state.protocol = connProtocol
	} else if state.protocol == "" {
		state.protocol = s.currentProtocol()
	}
	state.hasConnected = true
	state.currentlyConnected = true
	state.replaceExisting = false
	s.connectionStates[connIndex] = state
	s.stateAccess.Unlock()

	if s.connectedNotify == nil {
		return
	}
	s.connectedAccess.Lock()
	if _, loaded := s.connectedIndices[connIndex]; loaded {
		s.connectedAccess.Unlock()
		return
	}
	s.connectedIndices[connIndex] = struct{}{}
	s.connectedAccess.Unlock()
	s.connectedNotify <- connIndex
}

func (s *Service) ApplyConfig(version int32, configData []byte) config.UpdateResult {
	result := s.configManager.Apply(version, configData)
	if result.Err != nil {
		s.logger.Error("update ingress configuration: ", result.Err)
		return result
	}
	s.resetDirectOriginTransports()
	s.logger.Info("updated ingress configuration (version ", result.LastAppliedVersion, ")")
	return result
}

func (s *Service) maxActiveFlows() uint64 {
	return s.configManager.Snapshot().WarpRouting.MaxActiveFlows
}

func (s *Service) Close() error {
	unregisterActiveService(s)
	s.cancel()
	s.done.Wait()
	s.connectionAccess.Lock()
	for _, connection := range s.connections {
		connection.Close()
	}
	s.connections = nil
	s.connectionAccess.Unlock()
	s.resetDirectOriginTransports()
	return nil
}

const (
	backoffBaseTime             = time.Second
	backoffMaxTime              = 2 * time.Minute
	firstConnectionReadyTimeout = 15 * time.Second
)

func pickEdgeAddrIndex(edgeAddrs []*discovery.EdgeAddr, current int, preferredIPVer int) int {
	if len(edgeAddrs) == 0 {
		return 0
	}
	if current < 0 || current >= len(edgeAddrs) {
		current = 0
	}
	if preferredIPVer == 4 || preferredIPVer == 6 {
		for offset := 0; offset < len(edgeAddrs); offset++ {
			idx := (current + offset) % len(edgeAddrs)
			if edgeAddrs[idx].IPVersion == preferredIPVer {
				return idx
			}
		}
	}
	return current
}

func formatEdgeAddr(edgeAddr *discovery.EdgeAddr, connProtocol string) string {
	if edgeAddr == nil {
		return ""
	}
	if connProtocol == transport.ProtocolQUIC && edgeAddr.UDP != nil {
		return edgeAddr.UDP.String()
	}
	if edgeAddr.TCP != nil {
		return edgeAddr.TCP.String()
	}
	if edgeAddr.UDP != nil {
		return edgeAddr.UDP.String()
	}
	return ""
}

func (s *Service) setConnectionRunning(connIndex uint8, edgeAddrStr string, ipVer int, cancelConn context.CancelCauseFunc) {
	s.stateAccess.Lock()
	s.ensureConnectionStateLocked(connIndex)
	st := s.connectionStates[connIndex]
	st.edgeAddr = edgeAddrStr
	st.ipVersion = ipVer
	st.cancelConn = cancelConn
	s.connectionStates[connIndex] = st
	s.stateAccess.Unlock()
}

func (s *Service) clearConnectionRunning(connIndex uint8) {
	s.stateAccess.Lock()
	s.ensureConnectionStateLocked(connIndex)
	st := s.connectionStates[connIndex]
	st.currentlyConnected = false
	st.cancelConn = nil
	s.connectionStates[connIndex] = st
	s.stateAccess.Unlock()
}

func (s *Service) recordConnectionActivity(connIndex uint8) {
	now := time.Now().UnixMilli()
	s.stateAccess.Lock()
	s.ensureConnectionStateLocked(connIndex)
	st := s.connectionStates[connIndex]
	st.streams++
	st.lastActive = now
	s.connectionStates[connIndex] = st
	s.stateAccess.Unlock()
}

func (s *Service) reconfigureSlot(connIndex uint8, protocol string) {
	effectiveProto := protocol
	if effectiveProto == "" {
		effectiveProto = s.currentProtocol()
	}
	s.stateAccess.Lock()
	s.ensureConnectionStateLocked(connIndex)
	st := s.connectionStates[connIndex]
	st.protocol = effectiveProto
	st.retries = 0
	cancel := st.cancelConn
	s.connectionStates[connIndex] = st
	s.stateAccess.Unlock()

	if cancel != nil {
		cancel(errSlotReconfigured)
	}
}

func (s *Service) snapshotSlots(overrides [4]TunnelSlotConfig, prefPrimary int) []TunnelSlotSnapshot {
	s.stateAccess.Lock()
	count := max(s.haConnections, len(s.connectionStates))
	if count <= 0 {
		count = 4
	}
	states := make([]connectionState, count)
	for i := 0; i < count; i++ {
		if i < len(s.connectionStates) {
			states[i] = s.connectionStates[i]
		}
	}
	s.stateAccess.Unlock()

	globalProto := s.currentProtocol()
	res := make([]TunnelSlotSnapshot, count)
	anyConnected := false
	for i := 0; i < count; i++ {
		st := states[i]
		proto := st.protocol
		if proto == "" {
			proto = globalProto
		}
		var cfg TunnelSlotConfig
		if i < len(overrides) {
			cfg = overrides[i]
		}
		cfgProto := cfg.Protocol
		if cfgProto == "" {
			cfgProto = globalProto
		}
		if st.currentlyConnected {
			anyConnected = true
		}
		res[i] = TunnelSlotSnapshot{
			Index:       i,
			Connected:   st.currentlyConnected,
			Protocol:    proto,
			CfgProtocol: cfgProto,
			IPVersion:   st.ipVersion,
			CfgIPVer:    cfg.IPVersion,
			EdgeAddr:    st.edgeAddr,
			Retries:     int(st.retries),
			Streams:     st.streams,
			LastActive:  st.lastActive,
		}
	}

	primaryIdx := -1
	if prefPrimary >= 0 && prefPrimary < len(res) && (res[prefPrimary].Connected || !anyConnected) {
		primaryIdx = prefPrimary
	} else if anyConnected {
		bestActive := int64(-1)
		for i, sl := range res {
			if !sl.Connected {
				continue
			}
			if primaryIdx == -1 || sl.LastActive > bestActive {
				primaryIdx = i
				bestActive = sl.LastActive
			}
		}
	} else if len(res) > 0 {
		if prefPrimary >= 0 && prefPrimary < len(res) {
			primaryIdx = prefPrimary
		} else {
			primaryIdx = 0
		}
	}
	if primaryIdx >= 0 && primaryIdx < len(res) {
		res[primaryIdx].IsPrimary = true
	}
	return res
}

func (s *Service) superviseConnection(connIndex uint8, edgeAddrs []*discovery.EdgeAddr) {
	defer s.done.Done()

	edgeIndex := initialEdgeAddrIndex(connIndex, len(edgeAddrs))
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		slotCfg := getSlotOverride(connIndex)
		edgeIndex = pickEdgeAddrIndex(edgeAddrs, edgeIndex, slotCfg.IPVersion)
		edgeAddr := edgeAddrs[edgeIndex]
		state := s.connectionState(connIndex)
		servingProtocol := state.protocol
		if slotCfg.Protocol == transport.ProtocolQUIC || slotCfg.Protocol == transport.ProtocolHTTP2 {
			servingProtocol = slotCfg.Protocol
			s.stateAccess.Lock()
			s.ensureConnectionStateLocked(connIndex)
			st := s.connectionStates[connIndex]
			st.protocol = servingProtocol
			s.connectionStates[connIndex] = st
			s.stateAccess.Unlock()
		}
		wasReplacing := state.replaceExisting || state.hasConnected

		connCtx, cancelConn := context.WithCancelCause(s.ctx)
		s.setConnectionRunning(connIndex, formatEdgeAddr(edgeAddr, servingProtocol), edgeAddr.IPVersion, cancelConn)

		var recoveredEdgeIndex atomic.Int32
		recoveredEdgeIndex.Store(-1)
		var waitProbe func()
		if servingProtocol == transport.ProtocolHTTP2 && s.currentProtocol() == transport.ProtocolQUIC && slotCfg.Protocol != transport.ProtocolHTTP2 {
			waitProbe = s.startQUICRecoveryProbe(connCtx, cancelConn, connIndex, edgeAddrs, edgeIndex, &recoveredEdgeIndex)
		}

		err := s.safeServeConnectionContext(connCtx, connIndex, edgeAddr)
		s.clearConnectionRunning(connIndex)
		cancelConn(nil)
		if waitProbe != nil {
			waitProbe()
		}

		if s.ctx.Err() != nil {
			return
		}
		if errors.Is(context.Cause(connCtx), errSlotReconfigured) {
			continue
		}
		if errors.Is(context.Cause(connCtx), errSwitchBackToQUIC) {
			s.resetConnectionToQUIC(connIndex)
			if idx := int(recoveredEdgeIndex.Load()); idx >= 0 && idx < len(edgeAddrs) {
				edgeIndex = idx
			}
			continue
		}
		if err == nil {
			return
		}
		retry, cancelAll := connectionRetryDecision(err)
		if cancelAll {
			s.logger.Error("connection ", connIndex, " failed permanently: ", err)
			s.cancel()
			return
		}
		if !retry {
			s.logger.Error("connection ", connIndex, " failed permanently: ", err)
			return
		}

		isDuplicateErr := control.IsDuplicateConnectionError(err)
		retries, switchedProtocol, switched := s.recordConnectionFailure(connIndex, err)
		edgeIndex = rotateEdgeAddrIndex(edgeIndex, len(edgeAddrs))
		backoff := backoffDuration(int(retries))
		if isDuplicateErr {
			if !wasReplacing {
				backoff = edupconnFirstRetryDelay
			} else if backoff < edupconnRetryDelay {
				backoff = edupconnRetryDelay
			}
		}
		retryableErr, isRetryable := E.Cast[*protocol.RetryableError](err)
		if isRetryable && retryableErr.Delay > 0 {
			backoff = retryableErr.Delay
		}
		if switched {
			s.logger.Warn("connection ", connIndex, " switching to fallback protocol ", switchedProtocol, ": ", err)
		}
		s.logger.Error("connection ", connIndex, " failed: ", err, ", retrying in ", backoff)

		select {
		case <-time.After(backoff):
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Service) startQUICRecoveryProbe(
	ctx context.Context,
	cancelConn context.CancelCauseFunc,
	connIndex uint8,
	edgeAddrs []*discovery.EdgeAddr,
	currentEdgeIndex int,
	recoveredEdgeIndex *atomic.Int32,
) func() {
	if len(edgeAddrs) == 0 {
		return func() {}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		interval := quicRecoveryProbeInterval
		if interval <= 0 {
			interval = 3 * time.Minute
		}
		timer := time.NewTimer(interval)
		defer timer.Stop()

		probeStart := currentEdgeIndex
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}

			_, features := s.currentConnectionFeatures()
			for offset := 0; offset < len(edgeAddrs); offset++ {
				if ctx.Err() != nil {
					return
				}
				idx := (probeStart + offset) % len(edgeAddrs)
				if err := probeQUICEdge(ctx, edgeAddrs[idx], features, s.tunnelDialer); err == nil {
					recoveredEdgeIndex.Store(int32(idx))
					s.logger.Info("connection ", connIndex, " QUIC reachability recovered on ", edgeAddrs[idx].UDP, ", switching back to QUIC")
					cancelConn(errSwitchBackToQUIC)
					return
				}
			}
			probeStart = rotateEdgeAddrIndex(probeStart, len(edgeAddrs))
			timer.Reset(interval)
		}
	}()
	return wg.Wait
}

func (s *Service) resetConnectionToQUIC(connIndex uint8) {
	s.stateAccess.Lock()
	defer s.stateAccess.Unlock()
	s.ensureConnectionStateLocked(connIndex)
	st := s.connectionStates[connIndex]
	st.protocol = s.currentProtocol()
	st.retries = 0
	st.replaceExisting = true
	s.connectionStates[connIndex] = st
}

func (s *Service) serveConnection(connIndex uint8, edgeAddr *discovery.EdgeAddr) error {
	return s.serveConnectionContext(s.ctx, connIndex, edgeAddr)
}

func (s *Service) serveConnectionContext(ctx context.Context, connIndex uint8, edgeAddr *discovery.EdgeAddr) error {
	state := s.connectionState(connIndex)
	connProtocol := state.protocol
	numPreviousAttempts := state.retries
	replaceExisting := state.replaceExisting || state.hasConnected
	datagramVersion, features := s.currentConnectionFeatures()

	switch connProtocol {
	case transport.ProtocolQUIC:
		return s.serveQUICContext(ctx, connIndex, edgeAddr, datagramVersion, features, numPreviousAttempts, replaceExisting)
	case transport.ProtocolHTTP2:
		return s.serveHTTP2Context(ctx, connIndex, edgeAddr, features, numPreviousAttempts, replaceExisting)
	default:
		return E.New("unsupported protocol: ", connProtocol)
	}
}

func (s *Service) safeServeConnection(connIndex uint8, edgeAddr *discovery.EdgeAddr) error {
	return s.safeServeConnectionContext(s.ctx, connIndex, edgeAddr)
}

func (s *Service) safeServeConnectionContext(ctx context.Context, connIndex uint8, edgeAddr *discovery.EdgeAddr) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = E.New("panic in serve connection: ", recovered, "\n", string(debug.Stack()))
		}
	}()
	return s.serveConnectionContext(ctx, connIndex, edgeAddr)
}

func (s *Service) serveQUICContext(ctx context.Context, connIndex uint8, edgeAddr *discovery.EdgeAddr, datagramVersion string, features []string, numPreviousAttempts uint8, replaceExisting bool) error {
	s.logger.Info("connecting to edge via QUIC (connection ", connIndex, ")")

	connection, err := newQUICConnection(
		ctx, edgeAddr, connIndex,
		s.credentials, s.connectorID, datagramVersion,
		features, numPreviousAttempts, s.gracePeriod, s.tunnelDialer, func() {
			s.notifyConnected(connIndex, transport.ProtocolQUIC)
		}, s.logger,
	)
	if err != nil {
		return E.Cause(err, "create QUIC connection")
	}
	connection.SetReplaceExisting(replaceExisting)

	s.trackConnection(connection)
	defer func() {
		s.untrackConnection(connection)
		s.removeDatagramMuxer(connection)
	}()

	return serveQUICConnection(connection, ctx, &streamHandlerAdapter{s})
}

func (s *Service) currentConnectionFeatures() (string, []string) {
	version, features := s.featureSelector.Snapshot()
	if s.postQuantum && !transport.HasFeature(features, transport.FeaturePostQuantum) {
		features = append(features, transport.FeaturePostQuantum)
	}
	return version, features
}

func (s *Service) serveHTTP2Context(ctx context.Context, connIndex uint8, edgeAddr *discovery.EdgeAddr, features []string, numPreviousAttempts uint8, replaceExisting bool) error {
	s.logger.Info("connecting to edge via HTTP/2 (connection ", connIndex, ")")

	connection, err := newHTTP2Connection(
		ctx, edgeAddr, connIndex,
		s.credentials, s.connectorID,
		features, numPreviousAttempts, s.gracePeriod, s.tunnelDialer,
		&http2HandlerAdapter{service: s, connIndex: connIndex}, s.logger,
	)
	if err != nil {
		return E.Cause(err, "create HTTP/2 connection")
	}
	connection.SetReplaceExisting(replaceExisting)

	s.trackConnection(connection)
	defer s.untrackConnection(connection)

	return serveHTTP2Connection(connection, ctx)
}

func (s *Service) initializeConnectionState(connIndex uint8) {
	slotCfg := getSlotOverride(connIndex)
	s.stateAccess.Lock()
	defer s.stateAccess.Unlock()
	s.ensureConnectionStateLocked(connIndex)
	if slotCfg.Protocol == transport.ProtocolQUIC || slotCfg.Protocol == transport.ProtocolHTTP2 {
		s.connectionStates[connIndex].protocol = slotCfg.Protocol
	} else if s.connectionStates[connIndex].protocol == "" {
		s.connectionStates[connIndex].protocol = s.currentProtocol()
	}
}

func (s *Service) connectionState(connIndex uint8) connectionState {
	s.stateAccess.Lock()
	defer s.stateAccess.Unlock()
	s.ensureConnectionStateLocked(connIndex)
	state := s.connectionStates[connIndex]
	if state.protocol == "" {
		state.protocol = s.currentProtocol()
		s.connectionStates[connIndex] = state
	}
	return state
}

func (s *Service) recordConnectionFailure(connIndex uint8, err error) (uint8, string, bool) {
	s.stateAccess.Lock()
	defer s.stateAccess.Unlock()
	s.ensureConnectionStateLocked(connIndex)
	state := s.connectionStates[connIndex]
	if state.protocol == "" {
		state.protocol = s.currentProtocol()
	}
	state.replaceExisting = true
	if state.retries < transport.DefaultProtocolRetry {
		state.retries++
	}
	backoffRetries := state.retries
	if !control.IsDuplicateConnectionError(err) && state.protocol == transport.ProtocolQUIC && (backoffRetries >= transport.DefaultProtocolRetry || transport.IsQUICBroken(err)) {
		if fallback, hasFallback := s.fallbackProtocol(); hasFallback && state.protocol != fallback {
			state.protocol = fallback
			state.retries = 0
			s.connectionStates[connIndex] = state
			return backoffRetries, fallback, true
		}
	}
	s.connectionStates[connIndex] = state
	return backoffRetries, "", false
}

func (s *Service) incrementConnectionRetries(connIndex uint8) uint8 {
	s.stateAccess.Lock()
	defer s.stateAccess.Unlock()
	s.ensureConnectionStateLocked(connIndex)
	state := s.connectionStates[connIndex]
	if state.retries < transport.DefaultProtocolRetry {
		state.retries++
	}
	s.connectionStates[connIndex] = state
	return state.retries
}

func (s *Service) ensureConnectionStateLocked(connIndex uint8) {
	requiredLen := int(connIndex) + 1
	if len(s.connectionStates) >= requiredLen {
		return
	}
	grown := make([]connectionState, requiredLen)
	copy(grown, s.connectionStates)
	s.connectionStates = grown
}

func (s *Service) selector() transport.ProtocolSelector {
	if s.protocolSelector != nil {
		return s.protocolSelector
	}
	selector, err := transport.NewProtocolSelector(s.protocol, s.postQuantum)
	if err != nil {
		return staticProtocolSelector{current: transport.ProtocolQUIC}
	}
	return selector
}

func (s *Service) currentProtocol() string {
	return s.selector().Current()
}

func (s *Service) fallbackProtocol() (string, bool) {
	return s.selector().Fallback()
}

func (s *Service) resetDirectOriginTransports() {
	s.directTransportAccess.Lock()
	transports := s.directTransports
	s.directTransports = make(map[string]*http.Transport)
	s.directTransportAccess.Unlock()

	for _, t := range transports {
		t.CloseIdleConnections()
	}
}

func (s *Service) trackConnection(connection io.Closer) {
	s.connectionAccess.Lock()
	defer s.connectionAccess.Unlock()
	s.connections = append(s.connections, connection)
}

func (s *Service) untrackConnection(connection io.Closer) {
	s.connectionAccess.Lock()
	defer s.connectionAccess.Unlock()
	for index, tracked := range s.connections {
		if tracked == connection {
			s.connections = append(s.connections[:index], s.connections[index+1:]...)
			break
		}
	}
}

func backoffDuration(retries int) time.Duration {
	backoff := backoffBaseTime * (1 << min(retries, 7))
	if backoff > backoffMaxTime {
		backoff = backoffMaxTime
	}
	jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
	return backoff/2 + jitter
}

func initialEdgeAddrIndex(connIndex uint8, size int) int {
	if size <= 1 {
		return 0
	}
	return int(connIndex) % size
}

func rotateEdgeAddrIndex(current int, size int) int {
	if size <= 1 {
		return 0
	}
	return (current + 1) % size
}

func flattenRegions(regions [][]*discovery.EdgeAddr) []*discovery.EdgeAddr {
	total := 0
	maxLen := 0
	for _, region := range regions {
		total += len(region)
		if len(region) > maxLen {
			maxLen = len(region)
		}
	}
	result := make([]*discovery.EdgeAddr, 0, total)
	for idx := 0; idx < maxLen; idx++ {
		for _, region := range regions {
			if idx < len(region) {
				result = append(result, region[idx])
			}
		}
	}
	return result
}

func effectiveHAConnections(requested, available int) int {
	if available <= 0 {
		return 0
	}
	return min(requested, available)
}

func parseToken(token string) (protocol.Credentials, error) {
	data, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return protocol.Credentials{}, E.Cause(err, "decode token")
	}
	var tunnelToken protocol.TunnelToken
	err = json.Unmarshal(data, &tunnelToken)
	if err != nil {
		return protocol.Credentials{}, E.Cause(err, "unmarshal token")
	}
	return tunnelToken.ToCredentials(), nil
}

type staticProtocolSelector struct {
	current string
}

func (s staticProtocolSelector) Current() string {
	return s.current
}

func (s staticProtocolSelector) Fallback() (string, bool) {
	return "", false
}

type streamHandlerAdapter struct {
	service *Service
}

func (a *streamHandlerAdapter) HandleDataStream(ctx context.Context, stream io.ReadWriteCloser, request *protocol.ConnectRequest, connIndex uint8) {
	a.service.handleDataStream(ctx, stream, request, connIndex)
}

func (a *streamHandlerAdapter) HandleRPCStream(ctx context.Context, stream io.ReadWriteCloser, connIndex uint8) {
	a.service.handleRPCStream(ctx, stream, connIndex)
}

func (a *streamHandlerAdapter) HandleRPCStreamWithSender(ctx context.Context, stream io.ReadWriteCloser, connIndex uint8, sender protocol.DatagramSender) {
	a.service.handleRPCStreamWithSender(ctx, stream, connIndex, sender)
}

func (a *streamHandlerAdapter) HandleDatagram(ctx context.Context, data []byte, sender protocol.DatagramSender) {
	a.service.handleDatagram(ctx, data, sender)
}

type http2HandlerAdapter struct {
	service   *Service
	connIndex uint8
}

func (a *http2HandlerAdapter) DispatchRequest(ctx context.Context, stream io.ReadWriteCloser, writer protocol.ConnectResponseWriter, request *protocol.ConnectRequest) {
	a.service.recordConnectionActivity(a.connIndex)
	a.service.dispatchRequest(ctx, stream, writer, request)
}

func (a *http2HandlerAdapter) ApplyConfig(version int32, configData []byte) config.UpdateResult {
	return a.service.ApplyConfig(version, configData)
}

func (a *http2HandlerAdapter) NotifyConnected(connIndex uint8, connProtocol string) {
	a.service.notifyConnected(connIndex, connProtocol)
}
