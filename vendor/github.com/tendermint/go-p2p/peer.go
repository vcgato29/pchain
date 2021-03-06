package p2p

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/pkg/errors"
	cmn "github.com/tendermint/go-common"
	crypto "github.com/tendermint/go-crypto"
	wire "github.com/tendermint/go-wire"
)

// Peer could be marked as persistent, in which case you can use
// Redial function to reconnect. Note that inbound peers can't be
// made persistent. They should be made persistent on the other end.
//
// Before using a peer, you will need to perform a handshake on connection.
type Peer struct {
	cmn.BaseService

	outbound   bool
	persistent bool
	config     *PeerConfig
	conn       net.Conn // source connection

	mconn *MConnection // multiplex connection

	*NodeInfo
	Key  string
	Data *cmn.CMap // User data.
}

// PeerConfig is a Peer configuration.
type PeerConfig struct {
	AuthEnc bool // authenticated encryption

	HandshakeTimeout time.Duration
	DialTimeout      time.Duration

	MConfig *MConnConfig

	Fuzz       bool // fuzz connection (for testing)
	FuzzConfig *FuzzConnConfig
}

// DefaultPeerConfig returns the default config.
func DefaultPeerConfig() *PeerConfig {
	return &PeerConfig{
		AuthEnc:          true,
		HandshakeTimeout: 2 * time.Second,
		DialTimeout:      3 * time.Second,
		MConfig:          DefaultMConnConfig(),
		Fuzz:             false,
		FuzzConfig:       DefaultFuzzConnConfig(),
	}
}

func newOutboundPeer(addr *NetAddress, switchChainRouter map[string]*ChainRouter, onPeerError func(*Peer, interface{}), ourNodePrivKey crypto.PrivKeyEd25519) (*Peer, error) {
	return newOutboundPeerWithConfig(addr, switchChainRouter, onPeerError, ourNodePrivKey, DefaultPeerConfig())
}

func newOutboundPeerWithConfig(addr *NetAddress, switchChainRouter map[string]*ChainRouter, onPeerError func(*Peer, interface{}), ourNodePrivKey crypto.PrivKeyEd25519, config *PeerConfig) (*Peer, error) {
	conn, err := dial(addr, config)
	if err != nil {
		return nil, errors.Wrap(err, "Error creating peer")
	}

	peer, err := newPeerFromConnAndConfig(conn, true, switchChainRouter, onPeerError, ourNodePrivKey, config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return peer, nil
}

func newInboundPeer(conn net.Conn, switchChainRouter map[string]*ChainRouter, onPeerError func(*Peer, interface{}), ourNodePrivKey crypto.PrivKeyEd25519) (*Peer, error) {
	return newInboundPeerWithConfig(conn, switchChainRouter, onPeerError, ourNodePrivKey, DefaultPeerConfig())
}

func newInboundPeerWithConfig(conn net.Conn, switchChainRouter map[string]*ChainRouter, onPeerError func(*Peer, interface{}), ourNodePrivKey crypto.PrivKeyEd25519, config *PeerConfig) (*Peer, error) {
	return newPeerFromConnAndConfig(conn, false, switchChainRouter, onPeerError, ourNodePrivKey, config)
}

func newPeerFromConnAndConfig(rawConn net.Conn, outbound bool, switchChainRouter map[string]*ChainRouter, onPeerError func(*Peer, interface{}), ourNodePrivKey crypto.PrivKeyEd25519, config *PeerConfig) (*Peer, error) {
	conn := rawConn

	// Fuzz connection
	if config.Fuzz {
		// so we have time to do peer handshakes and get set up
		conn = FuzzConnAfterFromConfig(conn, 10*time.Second, config.FuzzConfig)
	}

	// Encrypt connection
	if config.AuthEnc {
		conn.SetDeadline(time.Now().Add(config.HandshakeTimeout))

		var err error
		conn, err = MakeSecretConnection(conn, ourNodePrivKey)
		if err != nil {
			return nil, errors.Wrap(err, "Error creating peer")
		}
	}

	// Key and NodeInfo are set after Handshake
	p := &Peer{
		outbound: outbound,
		conn:     conn,
		config:   config,
		Data:     cmn.NewCMap(),
	}

	p.mconn = createMConnection(conn, p, switchChainRouter, onPeerError, config.MConfig)

	p.BaseService = *cmn.NewBaseService(log.Root(), "Peer", p)

	return p, nil
}

// CloseConn should be used when the peer was created, but never started.
func (p *Peer) CloseConn() {
	p.conn.Close()
}

// makePersistent marks the peer as persistent.
func (p *Peer) makePersistent() {
	if !p.outbound {
		panic("inbound peers can't be made persistent")
	}

	p.persistent = true
}

// IsPersistent returns true if the peer is persitent, false otherwise.
func (p *Peer) IsPersistent() bool {
	return p.persistent
}

// HandshakeTimeout performs a handshake between a given node and the peer.
// NOTE: blocking
func (p *Peer) HandshakeTimeout(ourNodeInfo *NodeInfo, timeout time.Duration) error {
	// Set deadline for handshake so we don't block forever on conn.ReadFull
	p.conn.SetDeadline(time.Now().Add(timeout))

	var peerNodeInfo = new(NodeInfo)
	var err1 error
	var err2 error
	cmn.Parallel(
		func() {
			var n int
			wire.WriteBinary(ourNodeInfo, p.conn, &n, &err1)
		},
		func() {
			var n int
			wire.ReadBinary(peerNodeInfo, p.conn, maxNodeInfoSize, &n, &err2)
			log.Info("Peer handshake", " peerNodeInfo:", peerNodeInfo)
		})
	if err1 != nil {
		return errors.Wrap(err1, "Error during handshake/write")
	}
	if err2 != nil {
		return errors.Wrap(err2, "Error during handshake/read")
	}

	if p.config.AuthEnc {
		// Check that the professed PubKey matches the sconn's.
		if !peerNodeInfo.PubKey.Equals(p.PubKey()) {
			return fmt.Errorf("Ignoring connection with unmatching pubkey: %v vs %v",
				peerNodeInfo.PubKey, p.PubKey())
		}
	}

	// Remove deadline
	p.conn.SetDeadline(time.Time{})

	peerNodeInfo.RemoteAddr = p.Addr().String()
	// Add Networks Set from Array
	peerNodeInfo.Networks.nwSet = make(map[string]struct{})
	for _, network := range peerNodeInfo.Networks.NwArr {
		peerNodeInfo.Networks.nwSet[network] = struct{}{}
	}

	p.NodeInfo = peerNodeInfo
	p.Key = peerNodeInfo.PubKey.KeyString()

	return nil
}

// Addr returns peer's network address.
func (p *Peer) Addr() net.Addr {
	return p.conn.RemoteAddr()
}

// Key returns the peer's id key.
func (p *Peer) PeerKey() string {
	//	return p.nodeInfo.ListenAddr // XXX: should probably be PubKey.KeyString()
	return p.ListenAddr // XXX: should probably be PubKey.KeyString()
}

// PubKey returns peer's public key.
func (p *Peer) PubKey() crypto.PubKeyEd25519 {
	if p.config.AuthEnc {
		return p.conn.(*SecretConnection).RemotePubKey()
	}
	if p.NodeInfo == nil {
		panic("Attempt to get peer's PubKey before calling Handshake")
	}
	return p.PubKey()
}

// OnStart implements BaseService.
func (p *Peer) OnStart() error {
	p.BaseService.OnStart()
	_, err := p.mconn.Start()
	return err
}

// OnStop implements BaseService.
func (p *Peer) OnStop() {
	p.BaseService.OnStop()
	p.mconn.Stop() // stop everything and close the conn
}

// Connection returns underlying MConnection.
func (p *Peer) Connection() *MConnection {
	return p.mconn
}

// IsOutbound returns true if the connection is outbound, false otherwise.
func (p *Peer) IsOutbound() bool {
	return p.outbound
}

// Send msg to the channel identified by chID byte. Returns false if the send
// queue is full after timeout, specified by MConnection.
func (p *Peer) Send(chainID string, chID byte, msg interface{}) bool {
	if !p.IsRunning() {
		// see Switch#Broadcast, where we fetch the list of peers and loop over
		// them - while we're looping, one peer may be removed and stopped.
		return false
	}
	return p.mconn.Send(chainID, chID, msg)
}

// TrySend msg to the channel identified by chID byte. Immediately returns
// false if the send queue is full.
func (p *Peer) TrySend(chainID string, chID byte, msg interface{}) bool {
	if !p.IsRunning() {
		return false
	}
	return p.mconn.TrySend(chainID, chID, msg)
}

// CanSend returns true if the send queue is not full, false otherwise.
func (p *Peer) CanSend(chainID string, chID byte) bool {
	if !p.IsRunning() {
		return false
	}
	return p.mconn.CanSend(chainID, chID)
}

// WriteTo writes the peer's public key to w.
func (p *Peer) WriteTo(w io.Writer) (n int64, err error) {
	var n_ int
	wire.WriteString(p.Key, w, &n_, &err)
	n += int64(n_)
	return
}

// String representation.
func (p *Peer) String() string {
	if p.outbound {
		return fmt.Sprintf("Peer{%v %v out}", p.mconn, p.Key[:12])
	}

	return fmt.Sprintf("Peer{%v %v in}", p.mconn, p.Key[:12])
}

// Equals reports whenever 2 peers are actually represent the same node.
func (p *Peer) Equals(other *Peer) bool {
	return p.Key == other.Key
}

// Get the data for a given key.
func (p *Peer) Get(key string) interface{} {
	return p.Data.Get(key)
}

// IsInTheSameNetwork Check the Peer if it's in the same chain
func (p *Peer) IsInTheSameNetwork(chainID string) bool {
	_, same := p.Networks.nwSet[chainID]
	return same
}

// GetSameNetwork Return the same network slice between peer and current node
func (p *Peer) GetSameNetwork(nodeNetwork NetworkSet) []string {
	// initial the slice with cap = length of node network
	sameNetwork := make([]string, 0, len(nodeNetwork.NwArr))
	for _, network := range nodeNetwork.NwArr {
		if _, ok := p.Networks.nwSet[network]; ok {
			sameNetwork = append(sameNetwork, network)
		}
	}
	return sameNetwork
}

// AddChainChannelByChainID Add the Chain Channel into MConn
// then add the peer to each Child Chain Reactor
func (p *Peer) AddChainChannelByChainID(chainID string, chainRouter *ChainRouter) {
	p.mconn.addChainChannelByChainID(chainID, chainRouter)

	for _, reactor := range chainRouter.reactors {
		reactor.AddPeer(p)
	}
}

//------------------------------------------------------------------
// helper funcs

func dial(addr *NetAddress, config *PeerConfig) (net.Conn, error) {
	log.Info("Dialing address", "address", addr)
	conn, err := addr.DialTimeout(config.DialTimeout)
	if err != nil {
		log.Info("Failed dialing address", "address", addr, "error", err)
		return nil, err
	}
	return conn, nil
}

func createMConnection(conn net.Conn, p *Peer, switchChainRouter map[string]*ChainRouter, onPeerError func(*Peer, interface{}), config *MConnConfig) *MConnection {

	onReceive := func(chainID string, chID byte, msgBytes []byte) {

		chain, ok := switchChainRouter[chainID]
		if !ok {
			cmn.PanicSanity(cmn.Fmt("Unknown chain %s", chainID))
		}

		reactor := chain.reactorsByCh[chID]
		if reactor == nil {
			cmn.PanicSanity(cmn.Fmt("Unknown channel %X", chID))
		}
		reactor.Receive(chID, p, msgBytes)
	}

	onError := func(r interface{}) {
		onPeerError(p, r)
	}

	return NewMConnectionWithConfig(conn, switchChainRouter, onReceive, onError, config)
}
