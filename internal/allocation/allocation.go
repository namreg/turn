package allocation

import (
	"encoding/binary"
	"net"
	"sync"
	"time"

	"github.com/gortc/turn"
	"github.com/pion/logging"
	"github.com/pion/stun"
	"github.com/pion/turn/internal/ipnet"
	"github.com/pkg/errors"
)

// Allocation is tied to a FiveTuple and relays traffic
// use CreateAllocation and GetAllocation to operate
type Allocation struct {
	RelayAddr           net.Addr
	Protocol            Protocol
	TurnSocket          net.PacketConn
	RelaySocket         net.PacketConn
	fiveTuple           *FiveTuple
	permissionsLock     sync.RWMutex
	permissions         map[string]*Permission
	channelBindingsLock sync.RWMutex
	channelBindings     []*ChannelBind
	lifetimeTimer       *time.Timer
	closed              chan interface{}
	log                 logging.LeveledLogger
}

func addr2IPFingerprint(addr net.Addr) string {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.IP.String()
	case *net.TCPAddr: // Do we really need this case?
		return a.IP.String()
	}
	return "" // shoud never happen
}

// NewAllocation creates a new instance of NewAllocation.
func NewAllocation(turnSocket net.PacketConn, fiveTuple *FiveTuple, log logging.LeveledLogger) *Allocation {
	return &Allocation{
		TurnSocket:  turnSocket,
		fiveTuple:   fiveTuple,
		permissions: make(map[string]*Permission, 64),
		closed:      make(chan interface{}),
		log:         log,
	}
}

// GetPermission gets the Permission from the allocation
func (a *Allocation) GetPermission(addr net.Addr) *Permission {
	a.permissionsLock.RLock()
	defer a.permissionsLock.RUnlock()

	fingerprint := addr2IPFingerprint(addr)
	return a.permissions[fingerprint]
}

// AddPermission adds a new permission to the allocation
func (a *Allocation) AddPermission(p *Permission) {
	fingerprint := addr2IPFingerprint(p.Addr)

	a.permissionsLock.RLock()
	existedPermission, ok := a.permissions[fingerprint]
	a.permissionsLock.RUnlock()
	if ok {
		existedPermission.refresh(permissionTimeout)
		return
	}

	p.allocation = a
	a.permissionsLock.Lock()
	a.permissions[fingerprint] = p
	a.permissionsLock.Unlock()

	p.start(permissionTimeout)
}

// RemovePermission removes the net.Addr's fingerprint from the allocation's permissions
func (a *Allocation) RemovePermission(addr net.Addr) {
	a.permissionsLock.Lock()
	defer a.permissionsLock.Unlock()
	fingerprint := addr2IPFingerprint(addr)
	delete(a.permissions, fingerprint)
}

// AddChannelBind adds a new ChannelBind to the allocation, it also updates the
// permissions needed for this ChannelBind
func (a *Allocation) AddChannelBind(c *ChannelBind, lifetime time.Duration) error {
	// Check that this channel id isn't bound to another transport address, and
	// that this transport address isn't bound to another channel id.
	channelByID := a.GetChannelByID(c.ID)
	channelByPeer := a.GetChannelByAddr(c.Peer)
	if channelByID != channelByPeer {
		return errors.Errorf("You cannot use the same channel number with different peer")
	}

	// Add or refresh this channel.
	if channelByID == nil {
		a.channelBindingsLock.Lock()
		defer a.channelBindingsLock.Unlock()

		c.allocation = a
		a.channelBindings = append(a.channelBindings, c)
		c.start(lifetime)

		// Channel binds also refresh permissions.
		a.AddPermission(NewPermission(c.Peer, a.log))
	} else {
		channelByID.refresh(lifetime)

		// Channel binds also refresh permissions.
		a.AddPermission(NewPermission(channelByID.Peer, a.log))
	}

	return nil
}

// RemoveChannelBind removes the ChannelBind from this allocation by id
func (a *Allocation) RemoveChannelBind(id turn.ChannelNumber) bool {
	a.channelBindingsLock.Lock()
	defer a.channelBindingsLock.Unlock()

	for i := len(a.channelBindings) - 1; i >= 0; i-- {
		if a.channelBindings[i].ID == id {
			a.channelBindings = append(a.channelBindings[:i], a.channelBindings[i+1:]...)
			return true
		}
	}

	return false
}

// GetChannelByID gets the ChannelBind from this allocation by id
func (a *Allocation) GetChannelByID(id turn.ChannelNumber) *ChannelBind {
	a.channelBindingsLock.RLock()
	defer a.channelBindingsLock.RUnlock()
	for _, cb := range a.channelBindings {
		if cb.ID == id {
			return cb
		}
	}
	return nil
}

// GetChannelByAddr gets the ChannelBind from this allocation by net.Addr
func (a *Allocation) GetChannelByAddr(addr net.Addr) *ChannelBind {
	a.channelBindingsLock.RLock()
	defer a.channelBindingsLock.RUnlock()
	for _, cb := range a.channelBindings {
		if ipnet.AddrEqual(cb.Peer, addr) {
			return cb
		}
	}
	return nil
}

// Refresh updates the allocations lifetime
func (a *Allocation) Refresh(lifetime time.Duration) {
	if lifetime == 0 {
		if !a.lifetimeTimer.Stop() {
			a.log.Errorf("Failed to stop allocation timer for %v", a.fiveTuple)
		}
		return
	}
	if !a.lifetimeTimer.Reset(lifetime) {
		a.log.Errorf("Failed to reset allocation timer for %v", a.fiveTuple)
	}
}

// Close closes the allocation
func (a *Allocation) Close() error {
	select {
	case <-a.closed:
		return nil
	default:
	}
	close(a.closed)

	a.lifetimeTimer.Stop()

	a.permissionsLock.RLock()
	for _, p := range a.permissions {
		p.lifetimeTimer.Stop()
	}
	a.permissionsLock.RUnlock()

	a.channelBindingsLock.RLock()
	for _, c := range a.channelBindings {
		c.lifetimeTimer.Stop()
	}
	a.channelBindingsLock.RUnlock()

	return a.RelaySocket.Close()
}

//  https://tools.ietf.org/html/rfc5766#section-10.3
//  When the server receives a UDP datagram at a currently allocated
//  relayed transport address, the server looks up the allocation
//  associated with the relayed transport address.  The server then
//  checks to see whether the set of permissions for the allocation allow
//  the relaying of the UDP datagram as described in Section 8.
//
//  If relaying is permitted, then the server checks if there is a
//  channel bound to the peer that sent the UDP datagram (see
//  Section 11).  If a channel is bound, then processing proceeds as
//  described in Section 11.7.
//
//  If relaying is permitted but no channel is bound to the peer, then
//  the server forms and sends a Data indication.  The Data indication
//  MUST contain both an XOR-PEER-ADDRESS and a DATA attribute.  The DATA
//  attribute is set to the value of the 'data octets' field from the
//  datagram, and the XOR-PEER-ADDRESS attribute is set to the source
//  transport address of the received UDP datagram.  The Data indication
//  is then sent on the 5-tuple associated with the allocation.

const rtpMTU = 1500

func (a *Allocation) packetHandler(m *Manager) {
	buffer := make([]byte, rtpMTU)

	for {
		n, srcAddr, err := a.RelaySocket.ReadFrom(buffer)
		if err != nil {
			m.DeleteAllocation(a.fiveTuple)
			return
		}

		a.log.Debugf("relay socket %s received %d bytes from %s",
			a.RelaySocket.LocalAddr().String(),
			n,
			srcAddr.String())

		if channel := a.GetChannelByAddr(a.RelaySocket.LocalAddr()); channel != nil {
			channelData := make([]byte, 4)
			binary.BigEndian.PutUint16(channelData[0:], uint16(channel.ID))
			binary.BigEndian.PutUint16(channelData[2:], uint16(n))
			channelData = append(channelData, buffer[:n]...)

			if _, err = a.TurnSocket.WriteTo(channelData, a.fiveTuple.SrcAddr); err != nil {
				a.log.Errorf("Failed to send ChannelData from allocation %v %v", srcAddr, err)
			}
		} else if p := a.GetPermission(srcAddr); p != nil {
			udpAddr := srcAddr.(*net.UDPAddr)
			peerAddressAttr := turn.PeerAddress{IP: udpAddr.IP, Port: udpAddr.Port}
			dataAttr := turn.Data(buffer[:n])

			msg, err := stun.Build(stun.TransactionID, stun.NewType(stun.MethodData, stun.ClassIndication), peerAddressAttr, dataAttr)
			if err != nil {
				a.log.Errorf("Failed to send DataIndication from allocation %v %v", srcAddr, err)
			}
			a.log.Debugf("relaying message to client at %s", a.fiveTuple.SrcAddr.String())
			if _, err = a.TurnSocket.WriteTo(msg.Raw, a.fiveTuple.SrcAddr); err != nil {
				a.log.Errorf("Failed to send DataIndication from allocation %v %v", srcAddr, err)
			}

		} else {
			a.log.Errorf("Packet unhandled in relay src %v", srcAddr)
		}
	}

}
