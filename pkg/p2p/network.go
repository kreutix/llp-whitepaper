package p2p

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/kreutix/llp/pkg/types"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
)

const (
	// Protocol identifiers
	ProtocolID      = "/llp/1.0.0"
	OrderBookTopic  = "llp-orderbook"
	PriceFeedTopic  = "llp-pricefeed"
	MatchingTopic   = "llp-matching"
	ReputationTopic = "llp-reputation"

	// Bootstrap timeout
	BootstrapTimeout = 30 * time.Second

	// Peer discovery intervals
	PeerDiscoveryInterval = 5 * time.Minute

	// Message TTL (Time To Live)
	MessageTTL = 10 * time.Minute

	// Maximum number of peers to connect to
	MaxPeers = 50

	// Maximum message size
	MaxMessageSize = 1024 * 1024 // 1MB
)

// MessageHandler is a function type for message handlers
type MessageHandler func(sender peer.ID, msg *types.P2PMessage) error

// P2PNode represents a node in the Lightning Loan Protocol P2P network
type P2PNode struct {
	ctx           context.Context
	host          host.Host
	dht           *dht.IpfsDHT
	pubsub        *pubsub.PubSub
	topics        map[string]*pubsub.Topic
	subscriptions map[string]*pubsub.Subscription
	privateKey    crypto.PrivKey
	nodeID        string
	logger        *zap.Logger
	handlers      map[string]MessageHandler
	peersMutex    sync.RWMutex
	peers         map[peer.ID]time.Time
	orderCache    map[string]time.Time // Cache to avoid reprocessing messages
	cacheMutex    sync.RWMutex
}

// NewP2PNode creates a new P2P node for the Lightning Loan Protocol
func NewP2PNode(ctx context.Context, listenAddrs []multiaddr.Multiaddr, logger *zap.Logger) (*P2PNode, error) {
	// Generate a new private key for this node
	priv, _, err := crypto.GenerateKeyPairWithReader(crypto.Ed25519, 2048, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate key pair: %w", err)
	}

	// Create libp2p host
	h, err := libp2p.New(
		libp2p.ListenAddrs(listenAddrs...),
		libp2p.Identity(priv),
		libp2p.EnableNATService(),
		libp2p.EnableRelay(),
		libp2p.EnableAutoRelay(),
		libp2p.NATPortMap(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}

	// Create new DHT instance for peer discovery
	kadDHT, err := dht.New(ctx, h, dht.Mode(dht.ModeServer))
	if err != nil {
		return nil, fmt.Errorf("failed to create DHT: %w", err)
	}

	// Create a new PubSub service using the GossipSub router
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("failed to create pubsub: %w", err)
	}

	nodeID := h.ID().Pretty()
	logger.Info("P2P node created", zap.String("nodeID", nodeID))

	return &P2PNode{
		ctx:           ctx,
		host:          h,
		dht:           kadDHT,
		pubsub:        ps,
		topics:        make(map[string]*pubsub.Topic),
		subscriptions: make(map[string]*pubsub.Subscription),
		privateKey:    priv,
		nodeID:        nodeID,
		logger:        logger,
		handlers:      make(map[string]MessageHandler),
		peers:         make(map[peer.ID]time.Time),
		orderCache:    make(map[string]time.Time),
	}, nil
}

// Start initializes the P2P node and connects to the network
func (n *P2PNode) Start(bootstrapPeers []multiaddr.Multiaddr) error {
	// Set stream handler for the protocol
	n.host.SetStreamHandler(protocol.ID(ProtocolID), n.handleStream)

	// Bootstrap the DHT
	if err := n.dht.Bootstrap(n.ctx); err != nil {
		return fmt.Errorf("failed to bootstrap DHT: %w", err)
	}

	// Connect to bootstrap peers
	if len(bootstrapPeers) > 0 {
		n.logger.Info("Connecting to bootstrap peers", zap.Int("count", len(bootstrapPeers)))
		if err := n.connectToBootstrapPeers(bootstrapPeers); err != nil {
			n.logger.Warn("Failed to connect to some bootstrap peers", zap.Error(err))
			// Continue anyway, as we might still discover peers through DHT
		}
	}

	// Join pubsub topics
	topics := []string{OrderBookTopic, PriceFeedTopic, MatchingTopic, ReputationTopic}
	for _, topicName := range topics {
		if err := n.joinTopic(topicName); err != nil {
			return fmt.Errorf("failed to join topic %s: %w", topicName, err)
		}
	}

	// Start periodic peer discovery
	go n.startPeerDiscovery()

	// Start message processing for each subscription
	for topicName, sub := range n.subscriptions {
		go n.processMessages(topicName, sub)
	}

	// Announce ourselves to the network
	n.logger.Info("P2P node started",
		zap.String("nodeID", n.nodeID),
		zap.String("addresses", fmt.Sprintf("%v", n.host.Addrs())))

	return nil
}

// Stop gracefully shuts down the P2P node
func (n *P2PNode) Stop() error {
	// Unsubscribe from all topics
	for _, sub := range n.subscriptions {
		sub.Cancel()
	}

	// Close the host
	if err := n.host.Close(); err != nil {
		return fmt.Errorf("failed to close host: %w", err)
	}

	n.logger.Info("P2P node stopped", zap.String("nodeID", n.nodeID))
	return nil
}

// connectToBootstrapPeers connects to the provided bootstrap peers
func (n *P2PNode) connectToBootstrapPeers(bootstrapPeers []multiaddr.Multiaddr) error {
	var wg sync.WaitGroup
	var connErrors []error

	for _, peerAddr := range bootstrapPeers {
		peerInfo, err := peer.AddrInfoFromP2pAddr(peerAddr)
		if err != nil {
			connErrors = append(connErrors, fmt.Errorf("invalid peer address %s: %w", peerAddr, err))
			continue
		}

		wg.Add(1)
		go func(pi peer.AddrInfo) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(n.ctx, BootstrapTimeout)
			defer cancel()

			if err := n.host.Connect(ctx, pi); err != nil {
				n.logger.Warn("Failed to connect to bootstrap peer",
					zap.String("peer", pi.ID.Pretty()),
					zap.Error(err))
				connErrors = append(connErrors, err)
				return
			}

			n.logger.Info("Connected to bootstrap peer", zap.String("peer", pi.ID.Pretty()))
			n.peersMutex.Lock()
			n.peers[pi.ID] = time.Now()
			n.peersMutex.Unlock()
		}(*peerInfo)
	}

	wg.Wait()

	if len(connErrors) > 0 && len(connErrors) == len(bootstrapPeers) {
		return fmt.Errorf("failed to connect to any bootstrap peers: %v", connErrors)
	}

	return nil
}

// startPeerDiscovery periodically discovers new peers using DHT
func (n *P2PNode) startPeerDiscovery() {
	ticker := time.NewTicker(PeerDiscoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-n.ctx.Done():
			return
		case <-ticker.C:
			n.discoverPeers()
		}
	}
}

// discoverPeers finds new peers in the network
func (n *P2PNode) discoverPeers() {
	n.logger.Debug("Starting peer discovery")

	// Use random peer IDs to find peers through the DHT
	randomPeerID, err := peer.Decode("QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN")
	if err != nil {
		n.logger.Error("Failed to decode peer ID for discovery", zap.Error(err))
		return
	}

	peers, err := n.dht.FindPeers(n.ctx, randomPeerID)
	if err != nil {
		n.logger.Error("Failed to find peers", zap.Error(err))
		return
	}

	count := 0
	for p := range peers {
		if p.ID == n.host.ID() || len(p.Addrs) == 0 {
			continue
		}

		n.peersMutex.RLock()
		_, found := n.peers[p.ID]
		n.peersMutex.RUnlock()

		if !found {
			ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
			if err := n.host.Connect(ctx, p); err != nil {
				n.logger.Debug("Failed to connect to discovered peer",
					zap.String("peer", p.ID.Pretty()),
					zap.Error(err))
				cancel()
				continue
			}
			cancel()

			n.peersMutex.Lock()
			n.peers[p.ID] = time.Now()
			n.peersMutex.Unlock()

			count++
			n.logger.Info("Connected to new peer", zap.String("peer", p.ID.Pretty()))

			// Limit the number of new connections per discovery cycle
			if count >= 10 {
				break
			}
		}
	}

	// Prune old peers
	n.prunePeers()

	n.peersMutex.RLock()
	peerCount := len(n.peers)
	n.peersMutex.RUnlock()

	n.logger.Info("Peer discovery completed", zap.Int("connectedPeers", peerCount))
}

// prunePeers removes peers that haven't been seen for a while
func (n *P2PNode) prunePeers() {
	now := time.Now()

	n.peersMutex.Lock()
	defer n.peersMutex.Unlock()

	for id, lastSeen := range n.peers {
		if now.Sub(lastSeen) > 24*time.Hour {
			delete(n.peers, id)
			n.logger.Debug("Pruned inactive peer", zap.String("peer", id.Pretty()))
		}
	}
}

// joinTopic joins a pubsub topic and creates a subscription
func (n *P2PNode) joinTopic(topicName string) error {
	topic, err := n.pubsub.Join(topicName)
	if err != nil {
		return fmt.Errorf("failed to join topic %s: %w", topicName, err)
	}

	sub, err := topic.Subscribe()
	if err != nil {
		return fmt.Errorf("failed to subscribe to topic %s: %w", topicName, err)
	}

	n.topics[topicName] = topic
	n.subscriptions[topicName] = sub

	n.logger.Info("Joined topic", zap.String("topic", topicName))
	return nil
}

// processMessages processes incoming messages from a subscription
func (n *P2PNode) processMessages(topicName string, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(n.ctx)
		if err != nil {
			if err == n.ctx.Err() {
				return // Context cancelled, exit gracefully
			}
			n.logger.Error("Failed to read next message",
				zap.String("topic", topicName),
				zap.Error(err))
			continue
		}

		// Skip messages from ourselves
		if msg.ReceivedFrom == n.host.ID() {
			continue
		}

		// Process the message
		go n.handlePubSubMessage(topicName, msg)
	}
}

// handlePubSubMessage processes a pubsub message
func (n *P2PNode) handlePubSubMessage(topicName string, msg *pubsub.Message) {
	// Deserialize the message
	var p2pMsg types.P2PMessage
	if err := json.Unmarshal(msg.Data, &p2pMsg); err != nil {
		n.logger.Error("Failed to unmarshal message",
			zap.String("topic", topicName),
			zap.Error(err))
		return
	}

	// Check if we've seen this message before (deduplication)
	msgID := fmt.Sprintf("%s-%s-%d", p2pMsg.MessageType, p2pMsg.SenderID, p2pMsg.Timestamp.Unix())

	n.cacheMutex.Lock()
	if _, exists := n.orderCache[msgID]; exists {
		n.cacheMutex.Unlock()
		return // Already processed this message
	}

	// Add to cache with expiration
	n.orderCache[msgID] = time.Now().Add(MessageTTL)
	n.cacheMutex.Unlock()

	// Find and call the appropriate handler
	if handler, ok := n.handlers[p2pMsg.MessageType]; ok {
		if err := handler(msg.ReceivedFrom, &p2pMsg); err != nil {
			n.logger.Error("Error handling message",
				zap.String("type", p2pMsg.MessageType),
				zap.String("sender", p2pMsg.SenderID),
				zap.Error(err))
		}
	} else {
		n.logger.Debug("No handler for message type",
			zap.String("type", p2pMsg.MessageType),
			zap.String("sender", p2pMsg.SenderID))
	}

	// Periodically clean up the message cache
	if len(n.orderCache)%100 == 0 {
		go n.cleanMessageCache()
	}
}

// cleanMessageCache removes expired entries from the message cache
func (n *P2PNode) cleanMessageCache() {
	now := time.Now()

	n.cacheMutex.Lock()
	defer n.cacheMutex.Unlock()

	for msgID, expiry := range n.orderCache {
		if now.After(expiry) {
			delete(n.orderCache, msgID)
		}
	}
}

// handleStream processes an incoming stream from a peer
func (n *P2PNode) handleStream(stream network.Stream) {
	// Read the message
	buf := make([]byte, MaxMessageSize)
	len, err := stream.Read(buf)
	if err != nil {
		n.logger.Error("Failed to read from stream",
			zap.String("peer", stream.Conn().RemotePeer().Pretty()),
			zap.Error(err))
		stream.Reset()
		return
	}

	// Deserialize the message
	var p2pMsg types.P2PMessage
	if err := json.Unmarshal(buf[:len], &p2pMsg); err != nil {
		n.logger.Error("Failed to unmarshal direct message",
			zap.String("peer", stream.Conn().RemotePeer().Pretty()),
			zap.Error(err))
		stream.Reset()
		return
	}

	// Find and call the appropriate handler
	if handler, ok := n.handlers[p2pMsg.MessageType]; ok {
		if err := handler(stream.Conn().RemotePeer(), &p2pMsg); err != nil {
			n.logger.Error("Error handling direct message",
				zap.String("type", p2pMsg.MessageType),
				zap.String("sender", p2pMsg.SenderID),
				zap.Error(err))
		}
	} else {
		n.logger.Debug("No handler for direct message type",
			zap.String("type", p2pMsg.MessageType),
			zap.String("sender", p2pMsg.SenderID))
	}

	// Close the stream
	if err := stream.Close(); err != nil {
		n.logger.Error("Failed to close stream",
			zap.String("peer", stream.Conn().RemotePeer().Pretty()),
			zap.Error(err))
	}
}

// RegisterHandler registers a handler for a specific message type
func (n *P2PNode) RegisterHandler(messageType string, handler MessageHandler) {
	n.handlers[messageType] = handler
	n.logger.Debug("Registered handler", zap.String("messageType", messageType))
}

// BroadcastMessage broadcasts a message to a specific topic
func (n *P2PNode) BroadcastMessage(topicName string, messageType string, payload interface{}) error {
	topic, ok := n.topics[topicName]
	if !ok {
		return fmt.Errorf("topic %s not joined", topicName)
	}

	// Serialize the payload
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Create and sign the message
	msg := types.P2PMessage{
		MessageType: messageType,
		SenderID:    n.nodeID,
		Timestamp:   time.Now(),
		Payload:     payloadBytes,
	}

	// Sign the message (in a real implementation)
	// For MVP, we'll skip actual signing
	msg.Signature = "signature-placeholder"

	// Serialize the message
	msgBytes, err := msg.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	// Publish to the topic
	if err := topic.Publish(n.ctx, msgBytes); err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	n.logger.Debug("Broadcast message",
		zap.String("topic", topicName),
		zap.String("type", messageType))

	return nil
}

// SendDirectMessage sends a message directly to a specific peer
func (n *P2PNode) SendDirectMessage(peerID peer.ID, messageType string, payload interface{}) error {
	// Serialize the payload
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Create and sign the message
	msg := types.P2PMessage{
		MessageType: messageType,
		SenderID:    n.nodeID,
		Timestamp:   time.Now(),
		Payload:     payloadBytes,
	}

	// Sign the message (in a real implementation)
	// For MVP, we'll skip actual signing
	msg.Signature = "signature-placeholder"

	// Serialize the message
	msgBytes, err := msg.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	// Open a stream to the peer
	ctx, cancel := context.WithTimeout(n.ctx, 30*time.Second)
	defer cancel()

	stream, err := n.host.NewStream(ctx, peerID, protocol.ID(ProtocolID))
	if err != nil {
		return fmt.Errorf("failed to open stream to peer %s: %w", peerID.Pretty(), err)
	}
	defer stream.Close()

	// Write the message to the stream
	if _, err := stream.Write(msgBytes); err != nil {
		stream.Reset()
		return fmt.Errorf("failed to write to stream: %w", err)
	}

	n.logger.Debug("Sent direct message",
		zap.String("peer", peerID.Pretty()),
		zap.String("type", messageType))

	return nil
}

// GetPeerCount returns the number of connected peers
func (n *P2PNode) GetPeerCount() int {
	n.peersMutex.RLock()
	defer n.peersMutex.RUnlock()
	return len(n.peers)
}

// GetPeers returns a list of connected peer IDs
func (n *P2PNode) GetPeers() []peer.ID {
	n.peersMutex.RLock()
	defer n.peersMutex.RUnlock()

	peers := make([]peer.ID, 0, len(n.peers))
	for p := range n.peers {
		peers = append(peers, p)
	}
	return peers
}

// GetNodeID returns the node's ID
func (n *P2PNode) GetNodeID() string {
	return n.nodeID
}

// GetMultiaddrs returns the node's multiaddresses
func (n *P2PNode) GetMultiaddrs() []multiaddr.Multiaddr {
	return n.host.Addrs()
}

// PublishOrderBookEntry publishes an order book entry to the network
func (n *P2PNode) PublishOrderBookEntry(entry *types.OrderBookEntry) error {
	return n.BroadcastMessage(OrderBookTopic, "order_book_entry", entry)
}

// PublishPriceUpdate publishes a price update to the network
func (n *P2PNode) PublishPriceUpdate(priceData *types.PriceData) error {
	return n.BroadcastMessage(PriceFeedTopic, "price_update", priceData)
}

// PublishLoanMatch publishes a loan match to the network
func (n *P2PNode) PublishLoanMatch(match *types.LoanMatch) error {
	return n.BroadcastMessage(MatchingTopic, "loan_match", match)
}

// PublishReputationUpdate publishes a reputation update to the network
func (n *P2PNode) PublishReputationUpdate(update *types.ReputationUpdate) error {
	return n.BroadcastMessage(ReputationTopic, "reputation_update", update)
}
