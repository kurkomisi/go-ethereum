// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Contains the meters and timers used by the networking layer.

package p2p

import (
	"fmt"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

const (
	MetricsInboundConnects  = "p2p/InboundConnects"  // Name for the registered inbound connects meter
	MetricsInboundTraffic   = "p2p/InboundTraffic"   // Name for the registered inbound traffic meter
	MetricsOutboundConnects = "p2p/OutboundConnects" // Name for the registered outbound connects meter
	MetricsOutboundTraffic  = "p2p/OutboundTraffic"  // Name for the registered outbound traffic meter

	MeteredPeerLimit = 1024 // This amount of peers are individually metered
)

var (
	ingressConnectMeter = metrics.NewRegisteredMeter(MetricsInboundConnects, nil)  // Meter counting the ingress connections
	ingressTrafficMeter = metrics.NewRegisteredMeter(MetricsInboundTraffic, nil)   // Meter metering the cumulative ingress traffic
	egressConnectMeter  = metrics.NewRegisteredMeter(MetricsOutboundConnects, nil) // Meter counting the egress connections
	egressTrafficMeter  = metrics.NewRegisteredMeter(MetricsOutboundTraffic, nil)  // Meter metering the cumulative egress traffic

	PeerIngressRegistry = metrics.NewPrefixedChildRegistry(metrics.EphemeralRegistry, MetricsInboundTraffic+"/")  // Registry containing the peer ingress
	PeerEgressRegistry  = metrics.NewPrefixedChildRegistry(metrics.EphemeralRegistry, MetricsOutboundTraffic+"/") // Registry containing the peer egress

	meteredPeerFeed  event.Feed // Event feed for peer metrics
	meteredPeerCount int32      // Actually stored peer connection count
)

// MeteredPeerEventType is the type of peer events emitted by a metered connection.
type MeteredPeerEventType int

const (
	// PeerEncHandshakeSucceeded is the type of event emitted when a peer successfully
	// makes the encryption handshake.
	PeerEncHandshakeSucceeded MeteredPeerEventType = iota

	// PeerEncHandshakeFailed is the type of event emitted when a peer fails to
	// make the encryption handshake or disconnects before it.
	PeerEncHandshakeFailed

	// PeerProtoHandshakeSucceeded is the type of event emitted when a peer successfully
	// makes the protocol handshake.
	PeerProtoHandshakeSucceeded

	// PeerProtoHandshakeFailed is the type of event emitted when a peer fails to
	//	// make the protocol handshake or disconnects before it.
	PeerProtoHandshakeFailed

	// PeerMessageHandlingStarted is the type of event emitted when a peer starts
	// to handle the messages
	PeerMessageHandlingStarted

	// PeerDisconnected is the type of event emitted when a peer disconnects.
	PeerDisconnected
)

// MeteredPeerEvent is an event emitted when peers connect or disconnect.
type MeteredPeerEvent struct {
	Type      MeteredPeerEventType   // Type of peer event
	Name      string                 // Name of the node, including client type, version, OS, custom data
	Addr      string                 // TCP address of the peer
	Enode     string                 // Node URL
	ID        enode.ID               // Unique node identifier
	Protocols map[string]interface{} // Sub-protocol specific metadata fields
	Elapsed   time.Duration          // Time elapsed between the connection and the handshake/disconnection
	Ingress   uint64                 // Ingress count at the moment of the event
	Egress    uint64                 // Egress count at the moment of the event
	Peer      *Peer                  // Connected remote node instance
}

// Equal reports whether event and e are equal.
func (event *MeteredPeerEvent) Equal(e MeteredPeerEvent) bool {
	return event.Type == e.Type && event.Addr == e.Addr && event.ID == e.ID && event.Ingress == e.Ingress && event.Egress == e.Egress
}

// SubscribeMeteredPeerEvent registers a subscription for peer life-cycle events
// if metrics collection is enabled.
func SubscribeMeteredPeerEvent(ch chan<- MeteredPeerEvent) event.Subscription {
	return meteredPeerFeed.Subscribe(ch)
}

// meteredConn is a wrapper around a net.Conn that meters both the
// inbound and outbound network traffic.
type meteredConn struct {
	net.Conn // Network connection to wrap with metering

	connected time.Time // Connection time of the peer
	addr      *net.TCPAddr // TCP address of the peer
	id        enode.ID     // NodeID of the peer

	peer *Peer // Connected remote node instance

	// trafficMetered denotes if the peer is registered in the traffic registries.
	// Its value is true if the metered peer count doesn't reach the limit in the
	// moment of the peer's connection.
	trafficMetered bool
	ingressMeter   metrics.Meter // Meter for the read bytes of the peer
	egressMeter    metrics.Meter // Meter for the written bytes of the peer

	lock sync.RWMutex // Lock protecting the metered connection's internals
}

// newMeteredConn creates a new metered connection, bumps the ingress or egress
// connection meter and also increases the metered peer count. If the metrics
// system is disabled or the IP address is unspecified, this function returns
// the original object.
func newMeteredConn(conn net.Conn, ingress bool, addr *net.TCPAddr) net.Conn {
	// Short circuit if metrics are disabled
	if !metrics.Enabled {
		return conn
	}
	if addr == nil || addr.IP.IsUnspecified() {
		log.Warn("Peer address is unspecified")
		return conn
	}
	// Bump the connection counters and wrap the connection
	if ingress {
		ingressConnectMeter.Mark(1)
	} else {
		egressConnectMeter.Mark(1)
	}
	return &meteredConn{
		Conn:      conn,
		addr:      addr,
		connected: time.Now(),
	}
}

// Read delegates a network read to the underlying connection, bumping the common
// and the peer ingress traffic meters along the way.
func (c *meteredConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	ingressTrafficMeter.Mark(int64(n))
	c.lock.RLock()
	if c.trafficMetered {
		c.ingressMeter.Mark(int64(n))
	}
	c.lock.RUnlock()
	return n, err
}

// Write delegates a network write to the underlying connection, bumping the common
// and the peer egress traffic meters along the way.
func (c *meteredConn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	egressTrafficMeter.Mark(int64(n))
	c.lock.RLock()
	if c.trafficMetered {
		c.egressMeter.Mark(int64(n))
	}
	c.lock.RUnlock()
	return n, err
}

// encHandshakeDone is called after the connection passes the encryption
// handshake. Registers the peer to the ingress and the egress traffic
// registries using the peer's IP and node ID, also emits connect event.
func (c *meteredConn) encHandshakeDone(id enode.ID) {
	c.lock.Lock()
	c.id = id
	c.lock.Unlock()
	meteredPeerFeed.Send(MeteredPeerEvent{
		Type:    PeerEncHandshakeSucceeded,
		Addr:    c.addr.String(),
		ID:      id,
		Elapsed: time.Since(c.connected),
	})
}

// peerAdded is called after the connection passes the protocol handshake.
func (c *meteredConn) peerAdded(peer *Peer) {
	var id enode.ID
	if atomic.AddInt32(&meteredPeerCount, 1) >= MeteredPeerLimit {
		// Don't register the peer in the traffic registries.
		atomic.AddInt32(&meteredPeerCount, -1)
		c.lock.Lock()
		id, c.trafficMetered = c.id, false
		c.lock.Unlock()
		log.Warn("Metered peer count reached the limit")
	} else {
		c.lock.Lock()
		id, c.trafficMetered = c.id, true
		key := fmt.Sprintf("%s/%s", c.addr.String(), id.String())
		c.ingressMeter = metrics.NewRegisteredMeter(key, PeerIngressRegistry)
		c.egressMeter = metrics.NewRegisteredMeter(key, PeerEgressRegistry)
		c.lock.Unlock()
	}
	info := peer.Info()
	meteredPeerFeed.Send(MeteredPeerEvent{
		Type:  PeerProtoHandshakeSucceeded,
		Addr:  c.addr.String(),
		ID:    id,
		Enode: info.Enode,
		Name:  info.Name,
		Peer:  peer,
	})
}

// peerMessageHandlingStarted is called after the sub-protocol handshake
// is done and the peer is registered locally.
func (c *meteredConn) peerMessageHandlingStarted(protocols map[string]interface{}) {
	c.lock.Lock()
	id := c.id
	c.lock.Unlock()
	meteredPeerFeed.Send(MeteredPeerEvent{
		Type:      PeerMessageHandlingStarted,
		Addr:      c.addr.String(),
		ID:        id,
		Protocols: protocols,
	})
}

// Close delegates a close operation to the underlying connection, unregisters
// the peer from the traffic registries and emits close event.
func (c *meteredConn) Close() error {
	err := c.Conn.Close()
	c.lock.RLock()
	if c.id == (enode.ID{}) {
		// If the peer disconnects before/during the encryption handshake.
		c.lock.RUnlock()
		meteredPeerFeed.Send(MeteredPeerEvent{
			Type:    PeerEncHandshakeFailed,
			Addr:    c.addr.String(),
			Elapsed: time.Since(c.connected),
		})
		return err
	}
	id := c.id
	if !c.trafficMetered {
		// If the peer disconnects before/during the protocol handshake,
		// or it isn't registered in the traffic registries.
		c.lock.RUnlock()
		meteredPeerFeed.Send(MeteredPeerEvent{
			Type:    PeerProtoHandshakeFailed,
			Addr:    c.addr.String(),
			ID:      id,
		})
		return err
	}
	ingress, egress := uint64(c.ingressMeter.Count()), uint64(c.egressMeter.Count())
	c.lock.RUnlock()

	// Decrement the metered peer count
	atomic.AddInt32(&meteredPeerCount, -1)

	// Unregister the peer from the traffic registries
	key := fmt.Sprintf("%s/%s", c.addr.String(), id)
	PeerIngressRegistry.Unregister(key)
	PeerEgressRegistry.Unregister(key)

	meteredPeerFeed.Send(MeteredPeerEvent{
		Type:    PeerDisconnected,
		Addr:    c.addr.String(),
		ID:      id,
		Ingress: ingress,
		Egress:  egress,
	})
	return err
}
