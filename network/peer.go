// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package network

import (
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/version"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/formatting"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/version"
)

var (
	VersionPeerNak = version.NewDefaultVersion(constants.PlatformName, 1, 0, 4)
)

type peer struct {
	net *network // network this peer is part of

	// if the version message has been received and is valid. is only modified
	// on the connection's reader routine.
	gotVersion utils.AtomicBool

	// if the gotPeerList message has been received and is valid. is only
	// modified on the connection's reader routine.
	gotPeerList utils.AtomicBool

	// if the version message has been received and is valid and the peerlist
	// has been returned. is only modified on the connection's reader routine.
	connected utils.AtomicBool

	// only close the peer once
	once sync.Once

	// if the close function has been called.
	closed utils.AtomicBool

	// number of bytes currently in the send queue.
	pendingBytes int64

	// lock to ensure that closing of the sender queue is handled safely
	senderLock sync.Mutex
	// queue of messages this connection is attempting to send the peer. Is
	// closed when the connection is closed.
	sender chan []byte

	// ip may or may not be set when the peer is first started. is only modified
	// on the connection's reader routine.
	ip     utils.IPDesc
	ipLock sync.RWMutex

	// id should be set when the peer is first created.
	id ids.ShortID

	// the connection object that is used to read/write messages from
	conn net.Conn

	// version that the peer reported during the handshake
	versionStruct, versionStr utils.AtomicInterface

	// unix time of the last message sent and received respectively
	lastSent, lastReceived int64

	// Session ID the peer is trying to connect with
	incomingSessionID uint32

	tickerCloser chan struct{}
}

// assume the stateLock is held
func (p *peer) Start() error {
	if err := p.conn.SetReadDeadline(p.net.clock.Time().Add(p.net.readPeerVersionTimeout)); err != nil {
		p.net.log.Verbo("error on setting the connection read timeout %s", err)
		return err
	}

	// send the version and get a msg from receiver..
	msg, err := p.verionAck()
	if err != nil {

		// old logic (needs to be removed)
		// if we get an EOF from a versionAck, remote peer is an older versioned node.
		// it's actually (probably) closing a connection to us b/c we are already peered.
		// if that happens, then lets check if we are peered.
		if err == io.EOF {
			if p.net.isPeered(p.id) {
				return errAlreadyPeered
			}
		}
		return err
	}

	switch msg.Op() {
	case PeerList:
		// handle old self check logic
		if p.id.Equals(p.net.id) {
			return errPeerIsMyself
		}

		// if the first message is not version.  fall back to normal processing logic.
		// only acceptable option at this point would be a PeerList request
		// lets take care of the peer request
		go p.handle(msg)

		go p.ReadMessages()

		// we need to ask for a version.
		go p.Version()
		go p.WriteMessages()

		go p.requestFinishHandshake()
		go p.sendPings()

		return nil
	case Version:
	// fallthrough
	default:
		// handle old self check logic
		if p.id.Equals(p.net.id) {
			return errPeerIsMyself
		}

		// We didn't get a Version msg.  This is unexpected..
		return errVersionExpected
	}

	// parse and check if version is correct.
	peerVersionStr := msg.Get(VersionStr).(string)
	peerVersion, err := p.net.parser.Parse(peerVersionStr)
	if err != nil {
		return err
	}

	// add peer version msg..
	p.checkPeerVersion(peerVersion)

	// if the client is not running correct version then fallback logic start the normal processing.
	if peerVersion.Before(VersionPeerNak) {
		// handle old self check logic
		if p.id.Equals(p.net.id) {
			return errPeerIsMyself
		}

		// process the version message
		go p.handle(msg)

		go p.ReadMessages()

		// ask for a peer list
		go p.GetPeerList()
		go p.WriteMessages()

		go p.requestFinishHandshake()
		go p.sendPings()

		return nil
	}

	// set my IP from the Version msg.
	p.ip = msg.Get(IP).(utils.IPDesc)
	// register the peers version.
	p.versionStr.SetValue(peerVersion.String())

	return p.processVersionNak()
}

func (p *peer) processVersionNak() error {
	if p.id.Equals(p.net.id) {
		// we already peered respond to client.
		_, err := p.versionNack(SelfPeered, nil)
		if err != nil {
			// it would not matter if we didn't send the nack..
			// we will end up disconnecting the connection.
			p.net.log.Verbo("unable to send version nak %s", err)
		}
		return errPeerIsMyself
	}

	// am I already peered to them?
	if p.net.isPeered(p.id) {
		// we already peered respond to client.
		_, err := p.versionNack(AlreadyPeered, nil)
		if err != nil {
			// it would not matter if we didn't send the nack..
			// we will end up disconnecting the connection.
			p.net.log.Verbo("unable to send version nak %s", err)
		}
		return errAlreadyPeered
	}

	ips := p.net.validatorIPsNoLock()
	// We are not peered, so send client VersionNak
	msg, err := p.versionNack(Success, ips)
	if err != nil {
		return errVersionNakExpected
	}

	// test the versionNak to see if we are safe to peer.
	errorNo := msg.Get(ErrorNo).(uint32)
	switch errorNo {
	case AlreadyPeered:
		// the peer responded we are already peered to them.
		return errAlreadyPeered
	case SelfPeered:
		// the peer responded we are already peered to them.
		return errPeerIsMyself
	case Success:
		// fall through
	default:
		// not expected -- It wasn't a Success, so we punt..
		return fmt.Errorf("version nak errorNo=%d", errorNo)
	}

	peers := msg.Get(Peers).([]utils.IPDesc)

	// we now have the version and peer list
	p.gotVersion.SetValue(true)
	p.gotPeerList.SetValue(true)

	// note that we are connected. (same as tryMarkConnected)
	p.connected.SetValue(true)

	// register with network this peer connected.
	p.net.connected(p)

	go p.ReadMessages()
	go p.WriteMessages()

	go p.sendPings()

	// track the peers
	go func() {
		for _, ip := range peers {
			if !ip.Equal(p.net.ip.IP()) &&
				!ip.IsZero() &&
				(p.net.allowPrivateIPs || !ip.IsPrivate()) {

				// we need a state lock to call track
				p.net.stateLock.Lock()
				p.net.track(ip)
				p.net.stateLock.Unlock()
			}
		}
	}()

	return nil
}

func (p *peer) sendPings() {
	sendPingsTicker := time.NewTicker(p.net.pingFrequency)
	defer sendPingsTicker.Stop()

	for {
		select {
		case <-sendPingsTicker.C:
			closed := p.closed.GetValue()

			if closed {
				return
			}

			p.Ping()
		case <-p.tickerCloser:
			return
		}
	}
}

// request missing handshake messages from the peer
func (p *peer) requestFinishHandshake() {
	finishHandshakeTicker := time.NewTicker(p.net.getVersionTimeout)
	defer finishHandshakeTicker.Stop()

	for {
		select {
		case <-finishHandshakeTicker.C:
			gotVersion := p.gotVersion.GetValue()
			gotPeerList := p.gotPeerList.GetValue()
			connected := p.connected.GetValue()
			closed := p.closed.GetValue()

			if connected || closed {
				return
			}

			if !gotVersion {
				p.GetVersion()
			}
			if !gotPeerList {
				p.GetPeerList()
			}
		case <-p.tickerCloser:
			return
		}
	}
}

// attempt to read messages from the peer
func (p *peer) ReadMessages() {
	defer p.Close()

	if err := p.conn.SetReadDeadline(p.net.clock.Time().Add(p.net.pingPongTimeout)); err != nil {
		p.net.log.Verbo("error on setting the connection read timeout %s", err)
		return
	}

	pendingBuffer := wrappers.Packer{}
	readBuffer := make([]byte, p.net.readBufferSize)
	for {
		read, err := p.conn.Read(readBuffer)
		if err != nil {
			p.net.log.Verbo("error on connection read to %s %s %s", p.id, p.getIP(), err)
			return
		}

		pendingBuffer.Bytes = append(pendingBuffer.Bytes, readBuffer[:read]...)

		msgBytes := pendingBuffer.UnpackBytes()
		if pendingBuffer.Errored() {
			// if reading the bytes errored, then we haven't read the full
			// message yet
			pendingBuffer.Offset = 0
			pendingBuffer.Err = nil

			if int64(len(pendingBuffer.Bytes)) > p.net.maxMessageSize+wrappers.IntLen {
				// we have read more bytes than the max message size allows for,
				// so we should terminate this connection

				p.net.log.Verbo("error reading too many bytes on %s %s", p.id, err)
				return
			}

			// we should try to read more bytes to finish the message
			continue
		}

		// we read the full message bytes

		// set the pending bytes to any extra bytes that were read
		pendingBuffer.Bytes = pendingBuffer.Bytes[pendingBuffer.Offset:]
		// set the offset back to the start of the next message
		pendingBuffer.Offset = 0

		if int64(len(msgBytes)) > p.net.maxMessageSize {
			// if this message is longer than the max message length, then we
			// should terminate this connection

			p.net.log.Verbo("error reading too many bytes on %s %s", p.id, err)
			return
		}

		p.net.log.Verbo("parsing new message from %s:\n%s",
			p.id,
			formatting.DumpBytes{Bytes: msgBytes})

		msg, err := p.net.b.Parse(msgBytes)
		if err != nil {
			p.net.log.Debug("failed to parse new message from %s:\n%s\n%s",
				p.id,
				formatting.DumpBytes{Bytes: msgBytes},
				err)
			return
		}

		p.handle(msg)
	}
}

// attempt to write messages to the peer
func (p *peer) WriteMessages() {
	defer p.Close()

	for msg := range p.sender {
		p.net.log.Verbo("sending new message to %s:\n%s",
			p.id,
			formatting.DumpBytes{Bytes: msg})

		atomic.AddInt64(&p.pendingBytes, -int64(len(msg)))
		atomic.AddInt64(&p.net.pendingBytes, -int64(len(msg)))

		err := p.sendRaw(msg)
		if err != nil {
			p.net.log.Verbo("error writing to %s at %s due to: %s", p.id, p.ip, err)
			return
		}
		atomic.StoreInt64(&p.lastSent, p.net.clock.Time().Unix())
	}
}

// send assumes that the stateLock is not held.
func (p *peer) Send(msg Msg) bool {
	p.senderLock.Lock()
	defer p.senderLock.Unlock()

	// If the peer was closed then the sender channel was closed and we are
	// unable to send this message without panicking. So drop the message.
	if p.closed.GetValue() {
		p.net.log.Debug("dropping message to %s due to a closed connection", p.id)
		return false
	}

	// is it possible to send?
	if dropMsg := p.dropMessagePeer(); dropMsg {
		p.net.log.Debug("dropping message to %s due to a send queue with too many bytes", p.id)
		return false
	}

	msgBytes := msg.Bytes()
	msgBytesLen := int64(len(msgBytes))

	// lets assume send will be successful, we add to the network pending bytes
	// if we determine that we are being a bit restrictive, we could increase the global bandwidth?
	newPendingBytes := atomic.AddInt64(&p.net.pendingBytes, msgBytesLen)

	newConnPendingBytes := atomic.LoadInt64(&p.pendingBytes) + msgBytesLen
	if dropMsg := p.dropMessage(newConnPendingBytes, newPendingBytes); dropMsg {
		// we never sent the message, remove from pending totals
		atomic.AddInt64(&p.net.pendingBytes, -msgBytesLen)
		p.net.log.Debug("dropping message to %s due to a send queue with too many bytes", p.id)
		return false
	}

	select {
	case p.sender <- msgBytes:
		atomic.AddInt64(&p.pendingBytes, msgBytesLen)
		return true
	default:
		// we never sent the message, remove from pending totals
		atomic.AddInt64(&p.net.pendingBytes, -msgBytesLen)
		p.net.log.Debug("dropping message to %s due to a full send queue", p.id)
		return false
	}
}

// assumes the stateLock is not held
func (p *peer) handle(msg Msg) {
	p.net.heartbeat()

	currentTime := p.net.clock.Time()
	atomic.StoreInt64(&p.lastReceived, currentTime.Unix())

	if err := p.conn.SetReadDeadline(currentTime.Add(p.net.pingPongTimeout)); err != nil {
		p.net.log.Verbo("error on setting the connection read timeout %s, closing the connection", err)
		p.Close()
		return
	}

	op := msg.Op()
	msgMetrics := p.net.message(op)
	if msgMetrics == nil {
		p.net.log.Debug("dropping an unknown message from %s with op %s", p.id, op.String())
		return
	}
	msgMetrics.numReceived.Inc()

	switch op {
	case Version:
		p.version(msg)
		return
	case GetVersion:
		p.getVersion(msg)
		return
	case Ping:
		p.ping(msg)
		return
	case Pong:
		p.pong(msg)
		return
	case GetPeerList:
		p.getPeerList(msg)
		return
	case PeerList:
		p.peerList(msg)
		return
	}
	if !p.connected.GetValue() {
		p.net.log.Debug("dropping message from %s because the connection hasn't been established yet", p.id)

		// attempt to finish the handshake
		if !p.gotVersion.GetValue() {
			p.GetVersion()
		}
		if !p.gotPeerList.GetValue() {
			p.GetPeerList()
		}
		return
	}

	peerVersion := p.versionStruct.GetValue().(version.Version)
	if peerVersion.Before(minimumUnmaskedVersion) && time.Until(p.net.apricotPhase0Time) < 0 {
		p.net.log.Verbo("dropping message from un-upgraded validator %s", p.id)
		return
	}

	switch op {
	case GetAcceptedFrontier:
		p.getAcceptedFrontier(msg)
	case AcceptedFrontier:
		p.acceptedFrontier(msg)
	case GetAccepted:
		p.getAccepted(msg)
	case Accepted:
		p.accepted(msg)
	case Get:
		p.get(msg)
	case GetAncestors:
		p.getAncestors(msg)
	case Put:
		p.put(msg)
	case MultiPut:
		p.multiPut(msg)
	case PushQuery:
		p.pushQuery(msg)
	case PullQuery:
		p.pullQuery(msg)
	case Chits:
		p.chits(msg)
	default:
		p.net.log.Debug("dropping an unknown message from %s with op %s", p.id, op.String())
	}
}

func (p *peer) dropMessagePeer() bool {
	return atomic.LoadInt64(&p.pendingBytes) > p.net.maxMessageSize
}

func (p *peer) dropMessage(connPendingLen, networkPendingLen int64) bool {
	return networkPendingLen > p.net.networkPendingSendBytesToRateLimit && // Check to see if we should be enforcing any rate limiting
		p.dropMessagePeer() && // this connection should have a minimum allowed bandwidth
		(networkPendingLen > p.net.maxNetworkPendingSendBytes || // Check to see if this message would put too much memory into the network
			connPendingLen > p.net.maxNetworkPendingSendBytes/20) // Check to see if this connection is using too much memory
}

// assumes the stateLock is not held
func (p *peer) Close() { p.once.Do(p.close) }

// assumes only `peer.Close` calls this
func (p *peer) close() {
	// If the connection is closing, we can immediately cancel the ticker
	// goroutines.
	close(p.tickerCloser)

	p.closed.SetValue(true)

	if err := p.conn.Close(); err != nil {
		p.net.log.Debug("closing peer %s resulted in an error: %s", p.id, err)
	}

	p.senderLock.Lock()
	// The locks guarantee here that the sender routine will read that the peer
	// has been closed and will therefore not attempt to write on this channel.
	close(p.sender)
	p.senderLock.Unlock()

	p.net.disconnected(p)
}

// assumes the stateLock is not held
func (p *peer) GetVersion() {
	msg, err := p.net.b.GetVersion()
	p.net.log.AssertNoError(err)
	p.Send(msg)
}

// assumes the stateLock is not held
func (p *peer) Version() {
	p.net.stateLock.RLock()
	msg, err := p.net.b.Version(
		p.net.networkID,
		p.net.nodeID,
		p.net.nextSessionID[p.id.Key()],
		p.net.clock.Unix(),
		p.net.ip.IP(),
		p.net.version.String(),
	)

	p.net.stateLock.Unlock()
	p.net.log.AssertNoError(err)
	p.Send(msg)
}

// assumes the stateLock is not held
func (p *peer) GetPeerList() {
	msg, err := p.net.b.GetPeerList()
	p.net.log.AssertNoError(err)
	p.Send(msg)
}

// assumes the stateLock is not held
func (p *peer) SendPeerList() {
	ips := p.net.validatorIPs()
	p.PeerList(ips)
}

// assumes the stateLock is not held
func (p *peer) PeerList(peers []utils.IPDesc) {
	msg, err := p.net.b.PeerList(peers)
	if err != nil {
		p.net.log.Warn("failed to send PeerList message due to %s", err)
		return
	}
	p.Send(msg)
}

// assumes the stateLock is not held
func (p *peer) Ping() {
	msg, err := p.net.b.Ping()
	p.net.log.AssertNoError(err)
	if p.Send(msg) {
		p.net.ping.numSent.Inc()
	} else {
		p.net.ping.numFailed.Inc()
	}
}

// assumes the stateLock is not held
func (p *peer) Pong() {
	msg, err := p.net.b.Pong()
	p.net.log.AssertNoError(err)
	if p.Send(msg) {
		p.net.pong.numSent.Inc()
	} else {
		p.net.pong.numFailed.Inc()
	}
}

// assumes the stateLock is not held
func (p *peer) getVersion(_ Msg) { p.Version() }

// assumes the stateLock is not held
func (p *peer) version(msg Msg) {
	if p.gotVersion.GetValue() {
		p.net.log.Verbo("dropping duplicated version message from %s", p.id)
		return
	}

	if networkID := msg.Get(NetworkID).(uint32); networkID != p.net.networkID {
		p.net.log.Debug("peer's network ID doesn't match our networkID: Peer's = %d ; Ours = %d",
			networkID,
			p.net.networkID)

		p.discardIP()
		return
	}

	if nodeID := msg.Get(NodeID).(uint32); nodeID == p.net.nodeID {
		p.net.log.Debug("peer's node ID matches our nodeID")

		p.discardMyIP()
		return
	}

	// Check that our clocks are in sync
	myTime := float64(p.net.clock.Unix())
	if peerTime := float64(msg.Get(MyTime).(uint64)); math.Abs(peerTime-myTime) > p.net.maxClockDifference.Seconds() {
		if p.net.beacons.Contains(p.id) {
			p.net.log.Warn("beacon %s has a clock that is too far out of sync with mine. Peer's = %d, Ours = %d (seconds)",
				p.id,
				uint64(peerTime),
				uint64(myTime))
		} else {
			p.net.log.Debug("peer %s has a clock that is too far out of sync with mine. Peer's = %d, Ours = %d (seconds)",
				p.id,
				uint64(peerTime),
				uint64(myTime))
		}

		p.discardIP()
		return
	}

	// Check that our versions are compatible
	peerVersionStr := msg.Get(VersionStr).(string)
	peerVersion, err := p.net.parser.Parse(peerVersionStr)
	if err != nil {
		p.net.log.Debug("peer version could not be parsed due to %s", err)

		p.discardIP()
		return
	}
<<<<<<< HEAD
	if p.net.version.Before(peerVersion) {
		if p.net.beacons.Contains(p.id) {
			p.net.log.Info("beacon %s attempting to connect with newer version %s. You may want to update your client",
				p.id,
				peerVersion)
		} else {
			p.net.log.Debug("peer %s attempting to connect with newer version %s. You may want to update your client",
				p.id,
				peerVersion)
		}
	}
=======

	p.checkPeerVersion(peerVersion)

>>>>>>> version_conversation
	if err := p.net.version.Compatible(peerVersion); err != nil {
		p.net.log.Debug("peer version not compatible due to %s", err)

		if !p.net.beacons.Contains(p.id) {
			p.discardIP()
			return
		}
		p.net.log.Info("allowing beacon %s to connect with a lower version %s",
			p.id,
			peerVersion)
	}

	peerKey := p.id.Key()
	sessionID := msg.Get(SessionID).(uint32)
	p.net.stateLock.Lock()
	nextSessionID := p.net.nextSessionID[peerKey]
	_, isConnected := p.net.peers[peerKey]
	p.net.stateLock.Unlock()

	// If we already have a connection with this peer, we should only drop it
	// in favor of this new connection if the incoming session ID is 0 (meaning
	// the peer has restarted) or the incoming session ID is greater than the
	// current one (meaning the peer has disconnected and is reconnecting)
	if isConnected && sessionID != 0 && sessionID < nextSessionID {
		p.net.log.Debug("dropping connection request from %s. Incoming session ID: %d. Ours: %d",
			sessionID,
			nextSessionID,
		)
		p.discardIP()
		return
	}

	if p.ip.IsZero() {
		// we only care about the claimed IP if we don't know the IP yet
		peerIP := msg.Get(IP).(utils.IPDesc)
		addr := p.conn.RemoteAddr()
		localPeerIP, err := utils.ToIPDesc(addr.String())
		if err == nil {
			// If we don't know the peer's IP, we can't perform any verification
			if peerIP.IP.Equal(localPeerIP.IP) {
				// if the IPs match, add this ip:port pair to be tracked
				p.setIP(peerIP)
			}
		}
	}

	p.SendPeerList()

	p.versionStruct.SetValue(peerVersion)
	p.versionStr.SetValue(peerVersion.String())
	p.gotVersion.SetValue(true)

	p.versionStr = peerVersion.String()
	p.gotVersion = true
	p.incomingSessionID = sessionID
	p.tryMarkConnected()
}

// assumes the stateLock is not held
func (p *peer) getPeerList(_ Msg) {
	if p.gotVersion.GetValue() {
		p.SendPeerList()
	}
}

// assumes the stateLock is not held
func (p *peer) peerList(msg Msg) {
	ips := msg.Get(Peers).([]utils.IPDesc)

	p.gotPeerList.SetValue(true)
	p.tryMarkConnected()

	for _, ip := range ips {
		p.net.stateLock.Lock()
		if !ip.Equal(p.net.ip.IP()) &&
			!ip.IsZero() &&
			(p.net.allowPrivateIPs || !ip.IsPrivate()) {
			// TODO: only try to connect once
			p.net.track(ip)
		}
		p.net.stateLock.Unlock()
	}
}

// assumes the stateLock is not held
func (p *peer) ping(_ Msg) { p.Pong() }

// assumes the stateLock is not held
func (p *peer) pong(_ Msg) {}

// assumes the stateLock is not held
func (p *peer) getAcceptedFrontier(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)
	deadline := p.net.clock.Time().Add(time.Duration(msg.Get(Deadline).(uint64)))

	p.net.router.GetAcceptedFrontier(p.id, chainID, requestID, deadline)
}

// assumes the stateLock is not held
func (p *peer) acceptedFrontier(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)

	containerIDsBytes := msg.Get(ContainerIDs).([][]byte)
	containerIDs := make([]ids.ID, len(containerIDsBytes))
	for i, containerIDBytes := range containerIDsBytes {
		containerID, err := ids.ToID(containerIDBytes)
		if err != nil {
			p.net.log.Debug("error parsing ContainerID 0x%x: %s", containerIDBytes, err)
			return
		}
		containerIDs[i] = containerID
	}

	p.net.router.AcceptedFrontier(p.id, chainID, requestID, containerIDs)
}

// assumes the stateLock is not held
func (p *peer) getAccepted(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)
	deadline := p.net.clock.Time().Add(time.Duration(msg.Get(Deadline).(uint64)))

	containerIDsBytes := msg.Get(ContainerIDs).([][]byte)
	containerIDs := make([]ids.ID, len(containerIDsBytes))
	for i, containerIDBytes := range containerIDsBytes {
		containerID, err := ids.ToID(containerIDBytes)
		if err != nil {
			p.net.log.Debug("error parsing ContainerID 0x%x: %s", containerIDBytes, err)
			return
		}
		containerIDs[i] = containerID
	}

	p.net.router.GetAccepted(p.id, chainID, requestID, deadline, containerIDs)
}

// assumes the stateLock is not held
func (p *peer) accepted(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)

	containerIDsBytes := msg.Get(ContainerIDs).([][]byte)
	containerIDs := make([]ids.ID, len(containerIDsBytes))
	for i, containerIDBytes := range containerIDsBytes {
		containerID, err := ids.ToID(containerIDBytes)
		if err != nil {
			p.net.log.Debug("error parsing ContainerID 0x%x: %s", containerIDBytes, err)
			return
		}
		containerIDs[i] = containerID
	}

	p.net.router.Accepted(p.id, chainID, requestID, containerIDs)
}

// assumes the stateLock is not held
func (p *peer) get(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)
	deadline := p.net.clock.Time().Add(time.Duration(msg.Get(Deadline).(uint64)))
	containerID, err := ids.ToID(msg.Get(ContainerID).([]byte))
	p.net.log.AssertNoError(err)

	p.net.router.Get(p.id, chainID, requestID, deadline, containerID)
}

func (p *peer) getAncestors(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)
	deadline := p.net.clock.Time().Add(time.Duration(msg.Get(Deadline).(uint64)))
	containerID, err := ids.ToID(msg.Get(ContainerID).([]byte))
	p.net.log.AssertNoError(err)

	p.net.router.GetAncestors(p.id, chainID, requestID, deadline, containerID)
}

// assumes the stateLock is not held
func (p *peer) put(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)
	containerID, err := ids.ToID(msg.Get(ContainerID).([]byte))
	p.net.log.AssertNoError(err)
	container := msg.Get(ContainerBytes).([]byte)

	p.net.router.Put(p.id, chainID, requestID, containerID, container)
}

// assumes the stateLock is not held
func (p *peer) multiPut(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)
	containers := msg.Get(MultiContainerBytes).([][]byte)

	p.net.router.MultiPut(p.id, chainID, requestID, containers)
}

// assumes the stateLock is not held
func (p *peer) pushQuery(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)
	deadline := p.net.clock.Time().Add(time.Duration(msg.Get(Deadline).(uint64)))
	containerID, err := ids.ToID(msg.Get(ContainerID).([]byte))
	p.net.log.AssertNoError(err)
	container := msg.Get(ContainerBytes).([]byte)

	p.net.router.PushQuery(p.id, chainID, requestID, deadline, containerID, container)
}

// assumes the stateLock is not held
func (p *peer) pullQuery(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)
	deadline := p.net.clock.Time().Add(time.Duration(msg.Get(Deadline).(uint64)))
	containerID, err := ids.ToID(msg.Get(ContainerID).([]byte))
	p.net.log.AssertNoError(err)

	p.net.router.PullQuery(p.id, chainID, requestID, deadline, containerID)
}

// assumes the stateLock is not held
func (p *peer) chits(msg Msg) {
	chainID, err := ids.ToID(msg.Get(ChainID).([]byte))
	p.net.log.AssertNoError(err)
	requestID := msg.Get(RequestID).(uint32)

	containerIDsBytes := msg.Get(ContainerIDs).([][]byte)
	containerIDs := make([]ids.ID, len(containerIDsBytes))
	for i, containerIDBytes := range containerIDsBytes {
		containerID, err := ids.ToID(containerIDBytes)
		if err != nil {
			p.net.log.Debug("error parsing ContainerID 0x%x: %s", containerIDBytes, err)
			return
		}
		containerIDs[i] = containerID
	}

	p.net.router.Chits(p.id, chainID, requestID, containerIDs)
}

// assumes the stateLock is held
func (p *peer) tryMarkConnected() {
	if !p.connected && p.gotVersion && p.gotPeerList {
		// the network connected function can only be called if disconnected
		// wasn't already called
		if p.closed {
			return
		}

		key := p.id.Key()
		oldConnection, ok := p.net.peers[key]
		if ok {
			p.net.stateLock.Unlock()
			oldConnection.Close()
			p.net.stateLock.Lock()
		}
		p.connected = true

		// Next session ID = 1 + max(our next session ID, incoming session ID)
		if p.incomingSessionID > p.net.nextSessionID[key] {
			p.net.nextSessionID[key] = p.incomingSessionID
		}
		// The next connection with this peer should have session ID > current one
		p.net.nextSessionID[p.id.Key()]++ // Wrapping around to 0 is fine
		p.net.peers[key] = p
		p.net.numPeers.Set(float64(len(p.net.peers)))
		p.net.connected(p)
	}
}

func (p *peer) discardIP() {
	// By clearing the IP, we will not attempt to reconnect to this peer
	if ip := p.getIP(); !ip.IsZero() {
		p.setIP(utils.IPDesc{})

		p.net.stateLock.Lock()
		delete(p.net.disconnectedIPs, ip.String())
		p.net.stateLock.Unlock()
	}
	p.Close()
}

func (p *peer) discardMyIP() {
	// By clearing the IP, we will not attempt to reconnect to this peer
	if ip := p.getIP(); !ip.IsZero() {
		p.setIP(utils.IPDesc{})

		str := ip.String()

		p.net.stateLock.Lock()
		p.net.myIPs[str] = struct{}{}
		delete(p.net.disconnectedIPs, str)
		p.net.stateLock.Unlock()
	}
	p.Close()
}

// Read a single message from the connection.
func (p *peer) readMsg() (Msg, error) {
	pendingBuffer := wrappers.Packer{}
	readBuffer, err := p.readFull(wrappers.IntLen)
	if err != nil {
		return nil, err
	}
	if len(readBuffer) != 4 {
		return nil, fmt.Errorf("read buffer failed")
	}

	pendingBuffer.Bytes = append(pendingBuffer.Bytes, readBuffer...)

	// lets figure out the size of the message..
	size := pendingBuffer.UnpackInt()

	// now lets read the message.
	readBuffer, err = p.readFull(size)
	if err != nil {
		return nil, err
	}
	if uint32(len(readBuffer)) != size {
		return nil, fmt.Errorf("read buffer failed")
	}

	pendingBuffer.Bytes = append(pendingBuffer.Bytes, readBuffer...)

	// reset the offset after the size read.
	pendingBuffer.Offset = 0

	// unpack the bytes or error out...
	msgBytes := pendingBuffer.UnpackBytes()
	if pendingBuffer.Errored() {
		return nil, pendingBuffer.Err
	}

	msg, err := p.net.b.Parse(msgBytes)
	if err != nil {
		p.net.log.Debug("failed to parse new message from %s:\n%s\n%s",
			p.id,
			formatting.DumpBytes{Bytes: msgBytes},
			err)
		return nil, err
	}

	return msg, nil
}

// send the raw msg bytes
func (p *peer) sendRaw(msg []byte) error {

	// pack the message with a header.
	packer := wrappers.Packer{Bytes: make([]byte, len(msg)+wrappers.IntLen)}
	packer.PackBytes(msg)

	// transmit the data..
	msg = packer.Bytes
	for len(msg) > 0 {
		written, err := p.conn.Write(msg)
		if err != nil {
			return err
		}
		msg = msg[written:]
	}
	return nil
}

// read from the connection up to len bytes..  Don't stop till we got them.
func (p *peer) readFull(len uint32) ([]byte, error) {
	responseBuffer := make([]byte, 0, len)
	for len > 0 {
		readBuffer := make([]byte, len)
		read, err := p.conn.Read(readBuffer)
		if err != nil {
			p.net.log.Verbo("error on connection read to %s %s %s", p.id, p.ip, err)
			return nil, err
		}
		len -= uint32(read)
		responseBuffer = append(responseBuffer, readBuffer[:read]...)
	}
	return responseBuffer, nil
}

// Print out the peer version check message.
func (p *peer) checkPeerVersion(peerVersion version.Version) {
	if p.net.version.Before(peerVersion) {
		if p.net.beacons.Contains(p.id) {
			p.net.log.Info("beacon %s attempting to connect with newer version %s. You may want to update your client",
				p.id,
				peerVersion)
		} else {
			p.net.log.Debug("peer %s attempting to connect with newer version %s. You may want to update your client",
				p.id,
				peerVersion)
		}
	}
}

// build versionAck and send it.
func (p *peer) verionAck() (Msg, error) {
	msg, err := p.net.b.Version(
		p.net.networkID,
		p.net.nodeID,
		p.net.clock.Unix(),
		p.net.ip.IP(),
		p.net.version.String(),
	)
	p.net.log.AssertNoError(err)
	return p.sendAndReceive(msg)
}

// build versionNak and send it
func (p *peer) versionNack(peerResponse uint32, ips []utils.IPDesc) (Msg, error) {
	msg, err := p.net.b.VersionNak(
		peerResponse,
		ips,
	)
	p.net.log.AssertNoError(err)
	return p.sendAndReceive(msg)
}

// send the message and read the response.
func (p *peer) sendAndReceive(msg Msg) (Msg, error) {
	err := p.sendRaw(msg.Bytes())
	if err != nil {
		return nil, err
	}

	return p.readMsg()
}

func (p *peer) setIP(ip utils.IPDesc) {
	p.ipLock.Lock()
	defer p.ipLock.Unlock()
	p.ip = ip
}

func (p *peer) getIP() utils.IPDesc {
	p.ipLock.RLock()
	defer p.ipLock.RUnlock()
	return p.ip
}
