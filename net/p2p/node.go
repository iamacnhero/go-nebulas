// Copyright (C) 2017 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package p2p

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	mrand "math/rand"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/libp2p/go-libp2p-crypto"
	"github.com/libp2p/go-libp2p-kbucket"
	libnet "github.com/libp2p/go-libp2p-net"
	"github.com/libp2p/go-libp2p-peer"
	"github.com/libp2p/go-libp2p-peerstore"
	"github.com/libp2p/go-libp2p-swarm"
	"github.com/libp2p/go-libp2p/p2p/host/basic"
	"github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
)

// Node the node can be used as both the client and the server
type Node struct {
	host      *basichost.BasicHost
	id        peer.ID
	peerstore peerstore.Peerstore
	// key: ip + peer.ID
	conn map[string]int
	// key: ip + peer.ID
	stream       map[string]libnet.Stream
	routeTable   *kbucket.RoutingTable
	context      context.Context
	chainID      uint32
	version      uint8
	config       *Config
	running      bool
	synchronized bool
	syncList     []string
	// key: datachecksum value: []ip + peer.ID
	relayness     *lru.Cache
	relaynessLock *sync.Mutex
}

// NewNode start a local node and join the node to network
func NewNode(config *Config) (*Node, error) {

	node := &Node{}
	node.config = config
	node.context = context.Background()

	err := node.init()
	if err != nil {
		log.Error("NewNode: start node fail, can not init node", err)
		return nil, err
	}
	log.Info("NewNode: node init success")
	return node, nil
}

// Config return node config
func (node *Node) Config() *Config {
	return node.config
}

// ID return node config
func (node *Node) ID() peer.ID {
	return node.host.ID()
}

// SetSynchronized set node synchronized
func (node *Node) SetSynchronized(synchronized bool) {
	node.synchronized = synchronized
}

func (node *Node) init() error {

	ctx := node.context
	randseed := node.config.Randseed
	var r io.Reader
	if randseed == 0 {
		r = rand.Reader
	} else {
		r = mrand.New(mrand.NewSource(randseed))
	}

	priv, pub, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, r)

	if err != nil {
		return err
	}

	// Obtain Peer ID from public key
	node.id, err = peer.IDFromPublicKey(pub)
	if err != nil {
		return err
	}
	ps := peerstore.NewPeerstore()

	ps.AddPrivKey(node.id, priv)
	ps.AddPubKey(node.id, pub)
	node.peerstore = ps

	node.routeTable = kbucket.NewRoutingTable(
		node.config.bucketsize,
		kbucket.ConvertPeerID(node.id),
		node.config.latency,
		node.peerstore,
	)

	node.routeTable.Update(node.id)

	node.conn = make(map[string]int)
	node.stream = make(map[string]libnet.Stream)
	node.chainID = node.config.ChainID
	node.version = node.config.Version
	node.synchronized = false
	node.relayness, err = lru.New(node.config.RelayCacheSize)
	if err != nil {
		return err
	}
	node.relaynessLock = &sync.Mutex{}

	address, err := multiaddr.NewMultiaddr(
		fmt.Sprintf(
			"/ip4/%s/tcp/%d",
			node.config.IP,
			node.config.Port,
		),
	)
	if err != nil {
		return err
	}

	network, err := swarm.NewNetwork(
		ctx,
		[]multiaddr.Multiaddr{address},
		node.id,
		node.peerstore,
		nil,
	)

	options := &basichost.HostOpts{}

	// add nat manager
	options.NATManager = basichost.NewNATManager(network)

	log.Infof("makeHost: boot node pretty id is %s", node.id.Pretty())
	node.host, err = basichost.NewHost(node.context, network, options)
	return err
}

// SayHello Say hello to trustedNode
func (netService *NetService) SayHello(bootNode multiaddr.Multiaddr) error {
	node := netService.node
	bootAddr, bootID, err := parseAddressFromMultiaddr(bootNode)
	log.Info("SayHello: bootNode addr -> ", bootAddr)
	if err != nil {
		log.Error("SayHello: parse Address from trustedNode failed", bootNode, err)
		return err
	}
	node.peerstore.AddAddr(
		bootID,
		bootAddr,
		peerstore.TempAddrTTL,
	)
	log.Infof("SayHello: node.host.Addrs -> %s, bootAddr -> %s", node.host.Addrs()[0].String(), bootAddr.String())
	if node.host.Addrs()[0].String() != bootAddr.String() {
		for i := 0; i < 3; i++ {
			err := netService.Hello(bootID)
			if err != nil {
				log.Error("SayHello: say hello to bootNode occurs error, ", err)
				time.Sleep(time.Second)
				continue
			}
			break
		}
		if err != nil {
			log.Error("SayHello: ping to seedNode failed", bootNode, err)
		}
		log.Info("SayHello: node say hello to boot node success... ")
		node.peerstore.AddAddr(
			bootID,
			bootAddr,
			peerstore.PermanentAddrTTL)
		// Update the routing table.
		node.routeTable.Update(bootID)
	}
	return nil
}

func parseAddressFromMultiaddr(address multiaddr.Multiaddr) (multiaddr.Multiaddr, peer.ID, error) {

	addr, err := multiaddr.NewMultiaddr(
		strings.Split(address.String(), "/ipfs/")[0],
	)
	if err != nil {
		return nil, "", err
	}

	b58, err := address.ValueForProtocol(multiaddr.P_IPFS)
	if err != nil {
		return nil, "", err
	}

	id, err := peer.IDB58Decode(b58)
	if err != nil {
		return nil, "", err
	}

	return addr, id, nil

}
