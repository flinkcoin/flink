package networking

import (
	"context"
	"crypto/ecdsa"
	"github.com/flinkcoin/mono/apps/broker/internal/config"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multiaddr"
	"github.com/pkg/errors"
	"github.com/prysmaticlabs/prysm/v5/async"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/p2p/encoder"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/p2p/peers"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/p2p/peers/scorers"
	"github.com/prysmaticlabs/prysm/v5/config/features"
	"github.com/prysmaticlabs/prysm/v5/config/params"
	leakybucket "github.com/prysmaticlabs/prysm/v5/container/leaky-bucket"
	"github.com/prysmaticlabs/prysm/v5/monitoring/tracing/trace"
	prysmnetwork "github.com/prysmaticlabs/prysm/v5/network"
	"github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1/metadata"
	"github.com/prysmaticlabs/prysm/v5/runtime"
	"github.com/prysmaticlabs/prysm/v5/time/slots"
	"github.com/sirupsen/logrus"
)

// Host for managing peer to peer (p2p) networking.
type Host struct {
	started     bool
	cancel      context.CancelFunc
	cfg         *Config
	ctx         context.Context
	host        host.Host
	genesisTime time.Time
}

// NewHost initializes a new p2p service compatible with shared.Service interface. No
// connections are made until the Start function is called during the service registry startup.
func NewHost(ctx context.Context, cfg *config.Config) (*Service, error) {
	ctx, cancel := context.WithCancel(ctx)
	_ = cancel // govet fix for lost cancel. Cancel is handled in service.Stop().

	cfg = validateConfig(cfg)
	privKey, err := privKey(cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate p2p private key")
	}

	metaData, err := metaDataFromConfig(cfg)
	if err != nil {
		log.WithError(err).Error("Failed to create peer metadata")
		return nil, err
	}

	addrFilter, err := configureFilter(cfg)
	if err != nil {
		log.WithError(err).Error("Failed to create address filter")
		return nil, err
	}

	s := &Host{
		ctx:          ctx,
		cancel:       cancel,
		cfg:          cfg,
		addrFilter:   addrFilter,
		ipLimiter:    ipLimiter,
		privKey:      privKey,
		metaData:     metaData,
		isPreGenesis: true,
		joinedTopics: make(map[string]*pubsub.Topic, len(gossipTopicMappings)),
		subnetsLock:  make(map[uint64]*sync.RWMutex),
	}

	ipAddr := prysmnetwork.IPAddr()

	opts, err := s.buildOptions(ipAddr, s.privKey)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to build p2p options")
	}

	// Sets mplex timeouts
	configureMplex()
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create p2p host")
	}

	s.host = h
	return s, nil
}

// Start the p2p service.
func (s *Host) Start() {
	if s.started {
		log.Error("Attempted to start p2p service when it was already started")
		return
	}

	// Waits until the state is initialized via an event feed.
	// Used for fork-related data when connecting peers.
	s.awaitStateInitialized()
	s.isPreGenesis = false

	var relayNodes []string
	if s.cfg.RelayNodeAddr != "" {
		relayNodes = append(relayNodes, s.cfg.RelayNodeAddr)
		if err := dialRelayNode(s.ctx, s.host, s.cfg.RelayNodeAddr); err != nil {
			log.WithError(err).Errorf("Could not dial relay node")
		}
	}

	if !s.cfg.NoDiscovery {
		ipAddr := prysmnetwork.IPAddr()
		listener, err := s.startDiscoveryV5(
			ipAddr,
			s.privKey,
		)
		if err != nil {
			log.WithError(err).Fatal("Failed to start discovery")
			s.startupErr = err
			return
		}

		if err := s.connectToBootnodes(); err != nil {
			log.WithError(err).Error("Could not connect to boot nodes")
			s.startupErr = err
			return
		}

		s.dv5Listener = listener
		go s.listenForNewNodes()
	}

	s.started = true

	if len(s.cfg.StaticPeers) > 0 {
		addrs, err := PeersFromStringAddrs(s.cfg.StaticPeers)
		if err != nil {
			log.WithError(err).Error("could not convert ENR to multiaddr")
		}
		// Set trusted peers for those that are provided as static addresses.
		pids := peerIdsFromMultiAddrs(addrs)
		s.peers.SetTrustedPeers(pids)
		s.connectWithAllTrustedPeers(addrs)
	}
	// Initialize metadata according to the
	// current epoch.
	s.RefreshPersistentSubnets()

	// Periodic functions.
	async.RunEvery(s.ctx, params.BeaconConfig().TtfbTimeoutDuration(), func() {
		ensurePeerConnections(s.ctx, s.host, s.peers, relayNodes...)
	})
	async.RunEvery(s.ctx, 30*time.Minute, s.Peers().Prune)
	async.RunEvery(s.ctx, time.Duration(params.BeaconConfig().RespTimeout)*time.Second, s.updateMetrics)
	async.RunEvery(s.ctx, refreshRate, s.RefreshPersistentSubnets)
	async.RunEvery(s.ctx, 1*time.Minute, func() {
		inboundQUICCount := len(s.peers.InboundConnectedWithProtocol(peers.QUIC))
		inboundTCPCount := len(s.peers.InboundConnectedWithProtocol(peers.TCP))
		outboundQUICCount := len(s.peers.OutboundConnectedWithProtocol(peers.QUIC))
		outboundTCPCount := len(s.peers.OutboundConnectedWithProtocol(peers.TCP))
		total := inboundQUICCount + inboundTCPCount + outboundQUICCount + outboundTCPCount

		fields := logrus.Fields{
			"inboundTCP":  inboundTCPCount,
			"outboundTCP": outboundTCPCount,
			"total":       total,
		}

		if features.Get().EnableQUIC {
			fields["inboundQUIC"] = inboundQUICCount
			fields["outboundQUIC"] = outboundQUICCount
		}

		log.WithFields(fields).Info("Connected peers")
	})

	multiAddrs := s.host.Network().ListenAddresses()
	logIPAddr(s.host.ID(), multiAddrs...)

	p2pHostAddress := s.cfg.HostAddress
	p2pTCPPort := s.cfg.TCPPort
	p2pQUICPort := s.cfg.QUICPort

	if p2pHostAddress != "" {
		logExternalIPAddr(s.host.ID(), p2pHostAddress, p2pTCPPort, p2pQUICPort)
		verifyConnectivity(p2pHostAddress, p2pTCPPort, "tcp")
	}

	p2pHostDNS := s.cfg.HostDNS
	if p2pHostDNS != "" {
		logExternalDNSAddr(s.host.ID(), p2pHostDNS, p2pTCPPort)
	}
	go s.forkWatcher()
}

// Stop the p2p service and terminate all peer connections.
func (s *Host) Stop() error {
	defer s.cancel()
	s.started = false
	if s.dv5Listener != nil {
		s.dv5Listener.Close()
	}
	return nil
}

// Status of the p2p service. Will return an error if the service is considered unhealthy to
// indicate that this node should not serve traffic until the issue has been resolved.
func (s *Host) Status() error {
	if s.isPreGenesis {
		return nil
	}
	if !s.started {
		return errors.New("not running")
	}
	if s.startupErr != nil {
		return s.startupErr
	}
	if s.genesisTime.IsZero() {
		return errors.New("no genesis time set")
	}
	return nil
}

// Started returns true if the p2p service has successfully started.
func (s *Host) Started() bool {
	return s.started
}

// Encoding returns the configured networking encoding.
func (*Host) Encoding() encoder.NetworkEncoding {
	return &encoder.SszNetworkEncoder{}
}

// PubSub returns the p2p pubsub framework.
func (s *Host) PubSub() *pubsub.PubSub {
	return s.pubsub
}

// Host returns the currently running libp2p
// host of the service.
func (s *Host) Host() host.Host {
	return s.host
}

// SetStreamHandler sets the protocol handler on the p2p host multiplexer.
// This method is a pass through to libp2pcore.Host.SetStreamHandler.
func (s *Host) SetStreamHandler(topic string, handler network.StreamHandler) {
	s.host.SetStreamHandler(protocol.ID(topic), handler)
}

// PeerID returns the Peer ID of the local peer.
func (s *Host) PeerID() peer.ID {
	return s.host.ID()
}

// Disconnect from a peer.
func (s *Service) Disconnect(pid peer.ID) error {
	return s.host.Network().ClosePeer(pid)
}

// Connect to a specific peer.
func (s *Service) Connect(pi peer.AddrInfo) error {
	return s.host.Connect(s.ctx, pi)
}

// Peers returns the peer status interface.
func (s *Service) Peers() *peers.Status {
	return s.peers
}

// ENR returns the local node's current ENR.
func (s *Service) ENR() *enr.Record {
	if s.dv5Listener == nil {
		return nil
	}
	return s.dv5Listener.Self().Record()
}

// DiscoveryAddresses represents our enr addresses as multiaddresses.
func (s *Service) DiscoveryAddresses() ([]multiaddr.Multiaddr, error) {
	if s.dv5Listener == nil {
		return nil, nil
	}
	return convertToUdpMultiAddr(s.dv5Listener.Self())
}

// Metadata returns a copy of the peer's metadata.
func (s *Service) Metadata() metadata.Metadata {
	return s.metaData.Copy()
}

// MetadataSeq returns the metadata sequence number.
func (s *Service) MetadataSeq() uint64 {
	return s.metaData.SequenceNumber()
}

// AddPingMethod adds the metadata ping rpc method to the p2p service, so that it can
// be used to refresh ENR.
func (s *Service) AddPingMethod(reqFunc func(ctx context.Context, id peer.ID) error) {
	s.pingMethodLock.Lock()
	s.pingMethod = reqFunc
	s.pingMethodLock.Unlock()
}

func (s *Service) pingPeersAndLogEnr() {
	s.pingMethodLock.RLock()
	defer s.pingMethodLock.RUnlock()

	localENR := s.dv5Listener.Self()
	log.WithField("ENR", localENR).Info("New node record")

	if s.pingMethod == nil {
		return
	}

	for _, pid := range s.peers.Connected() {
		go func(id peer.ID) {
			if err := s.pingMethod(s.ctx, id); err != nil {
				log.WithField("peer", id).WithError(err).Debug("Failed to ping peer")
			}
		}(pid)
	}
}

// Waits for the beacon state to be initialized, important
// for initializing the p2p service as p2p needs to be aware
// of genesis information for peering.
func (s *Service) awaitStateInitialized() {
	s.initializationLock.Lock()
	defer s.initializationLock.Unlock()
	if s.isInitialized() {
		return
	}
	clock, err := s.cfg.ClockWaiter.WaitForClock(s.ctx)
	if err != nil {
		log.WithError(err).Fatal("failed to receive initial genesis data")
	}
	s.genesisTime = clock.GenesisTime()
	gvr := clock.GenesisValidatorsRoot()
	s.genesisValidatorsRoot = gvr[:]
	_, err = s.currentForkDigest() // initialize fork digest cache
	if err != nil {
		log.WithError(err).Error("Could not initialize fork digest")
	}
}

func (s *Service) connectWithAllTrustedPeers(multiAddrs []multiaddr.Multiaddr) {
	addrInfos, err := peer.AddrInfosFromP2pAddrs(multiAddrs...)
	if err != nil {
		log.WithError(err).Error("Could not convert to peer address info's from multiaddresses")
		return
	}
	for _, info := range addrInfos {
		// add peer into peer status
		s.peers.Add(nil, info.ID, info.Addrs[0], network.DirUnknown)
		// make each dial non-blocking
		go func(info peer.AddrInfo) {
			if err := s.connectWithPeer(s.ctx, info); err != nil {
				log.WithError(err).Tracef("Could not connect with peer %s", info.String())
			}
		}(info)
	}
}

func (s *Service) connectWithAllPeers(multiAddrs []multiaddr.Multiaddr) {
	addrInfos, err := peer.AddrInfosFromP2pAddrs(multiAddrs...)
	if err != nil {
		log.WithError(err).Error("Could not convert to peer address info's from multiaddresses")
		return
	}
	for _, info := range addrInfos {
		// make each dial non-blocking
		go func(info peer.AddrInfo) {
			if err := s.connectWithPeer(s.ctx, info); err != nil {
				log.WithError(err).Tracef("Could not connect with peer %s", info.String())
			}
		}(info)
	}
}

func (s *Service) connectWithPeer(ctx context.Context, info peer.AddrInfo) error {
	ctx, span := trace.StartSpan(ctx, "p2p.connectWithPeer")
	defer span.End()

	if info.ID == s.host.ID() {
		return nil
	}
	if err := s.Peers().IsBad(info.ID); err != nil {
		return errors.Wrap(err, "refused to connect to bad peer")
	}
	ctx, cancel := context.WithTimeout(ctx, maxDialTimeout)
	defer cancel()
	if err := s.host.Connect(ctx, info); err != nil {
		s.Peers().Scorers().BadResponsesScorer().Increment(info.ID)
		return err
	}
	return nil
}

func (s *Service) connectToBootnodes() error {
	nodes := make([]*enode.Node, 0, len(s.cfg.Discv5BootStrapAddrs))
	for _, addr := range s.cfg.Discv5BootStrapAddrs {
		bootNode, err := enode.Parse(enode.ValidSchemes, addr)
		if err != nil {
			return err
		}
		// do not dial bootnodes with their tcp ports not set
		if err := bootNode.Record().Load(enr.WithEntry("tcp", new(enr.TCP))); err != nil {
			if !enr.IsNotFound(err) {
				log.WithError(err).Error("Could not retrieve tcp port")
			}
			continue
		}
		nodes = append(nodes, bootNode)
	}
	multiAddresses := convertToMultiAddr(nodes)
	s.connectWithAllPeers(multiAddresses)
	return nil
}

// Returns true if the service is aware of the genesis time and genesis validators root. This is
// required for discovery and pubsub validation.
func (s *Service) isInitialized() bool {
	return !s.genesisTime.IsZero() && len(s.genesisValidatorsRoot) == 32
}
