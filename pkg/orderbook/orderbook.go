package orderbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kreutix/llp/pkg/p2p"
	"github.com/kreutix/llp/pkg/types"
	"github.com/libp2p/go-libp2p-core/peer"
	"go.uber.org/zap"
)

const (
	// Message types for order book
	MsgTypeAddOffer       = "add_offer"
	MsgTypeRemoveOffer    = "remove_offer"
	MsgTypeAddRequest     = "add_request"
	MsgTypeRemoveRequest  = "remove_request"
	MsgTypeOrderBookSync  = "orderbook_sync"
	MsgTypeOrderBookQuery = "orderbook_query"

	// Cleanup interval for expired entries
	CleanupInterval = 5 * time.Minute

	// Maximum number of entries to return in a sync response
	MaxSyncEntries = 1000

	// Maximum age of entries to consider valid
	MaxEntryAge = 24 * time.Hour
)

// OrderBook represents the distributed order book for loan offers and requests
type OrderBook struct {
	ctx               context.Context
	p2pNode           *p2p.P2PNode
	logger            *zap.Logger
	offersMutex       sync.RWMutex
	offers            map[string]*types.LoanOffer // key: offerID
	requestsMutex     sync.RWMutex
	requests          map[string]*types.LoanRequest // key: requestID
	entriesMutex      sync.RWMutex
	entries           map[string]*types.OrderBookEntry // key: entryID
	userOffersMutex   sync.RWMutex
	userOffers        map[string][]string // key: userID, value: list of offerIDs
	userRequestsMutex sync.RWMutex
	userRequests      map[string][]string // key: userID, value: list of requestIDs
	syncInProgress    bool
	syncMutex         sync.Mutex
}

// NewOrderBook creates a new distributed order book
func NewOrderBook(ctx context.Context, p2pNode *p2p.P2PNode, logger *zap.Logger) *OrderBook {
	ob := &OrderBook{
		ctx:          ctx,
		p2pNode:      p2pNode,
		logger:       logger,
		offers:       make(map[string]*types.LoanOffer),
		requests:     make(map[string]*types.LoanRequest),
		entries:      make(map[string]*types.OrderBookEntry),
		userOffers:   make(map[string][]string),
		userRequests: make(map[string][]string),
	}

	// Register message handlers
	p2pNode.RegisterHandler(MsgTypeAddOffer, ob.handleAddOffer)
	p2pNode.RegisterHandler(MsgTypeRemoveOffer, ob.handleRemoveOffer)
	p2pNode.RegisterHandler(MsgTypeAddRequest, ob.handleAddRequest)
	p2pNode.RegisterHandler(MsgTypeRemoveRequest, ob.handleRemoveRequest)
	p2pNode.RegisterHandler(MsgTypeOrderBookSync, ob.handleOrderBookSync)
	p2pNode.RegisterHandler(MsgTypeOrderBookQuery, ob.handleOrderBookQuery)

	// Start periodic cleanup of expired entries
	go ob.startCleanupRoutine()

	return ob
}

// startCleanupRoutine periodically removes expired entries
func (ob *OrderBook) startCleanupRoutine() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ob.ctx.Done():
			return
		case <-ticker.C:
			ob.cleanupExpiredEntries()
		}
	}
}

// cleanupExpiredEntries removes expired offers and requests
func (ob *OrderBook) cleanupExpiredEntries() {
	now := time.Now()
	expiredOffers := []string{}
	expiredRequests := []string{}

	// Find expired offers
	ob.offersMutex.RLock()
	for id, offer := range ob.offers {
		if now.After(offer.ExpiresAt) {
			expiredOffers = append(expiredOffers, id)
		}
	}
	ob.offersMutex.RUnlock()

	// Find expired requests
	ob.requestsMutex.RLock()
	for id, request := range ob.requests {
		if now.After(request.ExpiresAt) {
			expiredRequests = append(expiredRequests, id)
		}
	}
	ob.requestsMutex.RUnlock()

	// Remove expired offers
	for _, id := range expiredOffers {
		ob.removeOffer(id, true)
	}

	// Remove expired requests
	for _, id := range expiredRequests {
		ob.removeRequest(id, true)
	}

	if len(expiredOffers) > 0 || len(expiredRequests) > 0 {
		ob.logger.Info("Cleaned up expired entries",
			zap.Int("expiredOffers", len(expiredOffers)),
			zap.Int("expiredRequests", len(expiredRequests)))
	}
}

// AddOffer adds a new loan offer to the order book
func (ob *OrderBook) AddOffer(offer *types.LoanOffer) error {
	// Validate the offer
	if err := offer.Validate(); err != nil {
		return fmt.Errorf("invalid offer: %w", err)
	}

	// Generate ID if not provided
	if offer.ID == "" {
		offer.ID = uuid.New().String()
	}

	// Set created time if not set
	if offer.CreatedAt.IsZero() {
		offer.CreatedAt = time.Now()
	}

	// Set expires time if not set
	if offer.ExpiresAt.IsZero() {
		offer.ExpiresAt = offer.CreatedAt.Add(24 * time.Hour)
	}

	// Set initial status
	if offer.Status == "" {
		offer.Status = types.StatusPending
	}

	// Create order book entry
	entry := &types.OrderBookEntry{
		EntryType: "offer",
		EntryID:   offer.ID,
		UserID:    offer.LenderID,
		CreatedAt: offer.CreatedAt,
		ExpiresAt: offer.ExpiresAt,
		Data:      offer,
	}

	// Add to local state
	ob.offersMutex.Lock()
	ob.offers[offer.ID] = offer
	ob.offersMutex.Unlock()

	ob.entriesMutex.Lock()
	ob.entries[offer.ID] = entry
	ob.entriesMutex.Unlock()

	ob.userOffersMutex.Lock()
	ob.userOffers[offer.LenderID] = append(ob.userOffers[offer.LenderID], offer.ID)
	ob.userOffersMutex.Unlock()

	// Broadcast to network
	if err := ob.p2pNode.BroadcastMessage(p2p.OrderBookTopic, MsgTypeAddOffer, entry); err != nil {
		ob.logger.Error("Failed to broadcast offer",
			zap.String("offerID", offer.ID),
			zap.Error(err))
		// Continue anyway, as the offer is still valid locally
	}

	ob.logger.Info("Added loan offer",
		zap.String("offerID", offer.ID),
		zap.String("lenderID", offer.LenderID),
		zap.Float64("amount", offer.USDTAmount),
		zap.Int("interestBPS", offer.DailyInterestBPS))

	return nil
}

// AddRequest adds a new loan request to the order book
func (ob *OrderBook) AddRequest(request *types.LoanRequest) error {
	// Validate the request
	if err := request.Validate(); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}

	// Generate ID if not provided
	if request.ID == "" {
		request.ID = uuid.New().String()
	}

	// Set created time if not set
	if request.CreatedAt.IsZero() {
		request.CreatedAt = time.Now()
	}

	// Set expires time if not set
	if request.ExpiresAt.IsZero() {
		request.ExpiresAt = request.CreatedAt.Add(24 * time.Hour)
	}

	// Set initial status
	if request.Status == "" {
		request.Status = types.StatusPending
	}

	// Create order book entry
	entry := &types.OrderBookEntry{
		EntryType: "request",
		EntryID:   request.ID,
		UserID:    request.BorrowerID,
		CreatedAt: request.CreatedAt,
		ExpiresAt: request.ExpiresAt,
		Data:      request,
	}

	// Add to local state
	ob.requestsMutex.Lock()
	ob.requests[request.ID] = request
	ob.requestsMutex.Unlock()

	ob.entriesMutex.Lock()
	ob.entries[request.ID] = entry
	ob.entriesMutex.Unlock()

	ob.userRequestsMutex.Lock()
	ob.userRequests[request.BorrowerID] = append(ob.userRequests[request.BorrowerID], request.ID)
	ob.userRequestsMutex.Unlock()

	// Broadcast to network
	if err := ob.p2pNode.BroadcastMessage(p2p.OrderBookTopic, MsgTypeAddRequest, entry); err != nil {
		ob.logger.Error("Failed to broadcast request",
			zap.String("requestID", request.ID),
			zap.Error(err))
		// Continue anyway, as the request is still valid locally
	}

	ob.logger.Info("Added loan request",
		zap.String("requestID", request.ID),
		zap.String("borrowerID", request.BorrowerID),
		zap.Float64("amount", request.USDTAmount),
		zap.Float64("collateral", request.BTCCollateral))

	return nil
}

// RemoveOffer removes a loan offer from the order book
func (ob *OrderBook) RemoveOffer(offerID string) error {
	return ob.removeOffer(offerID, false)
}

// removeOffer is an internal method to remove an offer with optional broadcasting
func (ob *OrderBook) removeOffer(offerID string, skipBroadcast bool) error {
	ob.offersMutex.RLock()
	offer, exists := ob.offers[offerID]
	ob.offersMutex.RUnlock()

	if !exists {
		return fmt.Errorf("offer with ID %s not found", offerID)
	}

	// Remove from local state
	ob.offersMutex.Lock()
	delete(ob.offers, offerID)
	ob.offersMutex.Unlock()

	ob.entriesMutex.Lock()
	delete(ob.entries, offerID)
	ob.entriesMutex.Unlock()

	// Update user offers
	ob.userOffersMutex.Lock()
	userOffers := ob.userOffers[offer.LenderID]
	for i, id := range userOffers {
		if id == offerID {
			// Remove this offer ID from the user's list
			ob.userOffers[offer.LenderID] = append(userOffers[:i], userOffers[i+1:]...)
			break
		}
	}
	ob.userOffersMutex.Unlock()

	// Broadcast to network if not skipped
	if !skipBroadcast {
		if err := ob.p2pNode.BroadcastMessage(p2p.OrderBookTopic, MsgTypeRemoveOffer, map[string]string{
			"offer_id": offerID,
		}); err != nil {
			ob.logger.Error("Failed to broadcast offer removal",
				zap.String("offerID", offerID),
				zap.Error(err))
			// Continue anyway, as the offer is still removed locally
		}
	}

	ob.logger.Info("Removed loan offer", zap.String("offerID", offerID))
	return nil
}

// RemoveRequest removes a loan request from the order book
func (ob *OrderBook) RemoveRequest(requestID string) error {
	return ob.removeRequest(requestID, false)
}

// removeRequest is an internal method to remove a request with optional broadcasting
func (ob *OrderBook) removeRequest(requestID string, skipBroadcast bool) error {
	ob.requestsMutex.RLock()
	request, exists := ob.requests[requestID]
	ob.requestsMutex.RUnlock()

	if !exists {
		return fmt.Errorf("request with ID %s not found", requestID)
	}

	// Remove from local state
	ob.requestsMutex.Lock()
	delete(ob.requests, requestID)
	ob.requestsMutex.Unlock()

	ob.entriesMutex.Lock()
	delete(ob.entries, requestID)
	ob.entriesMutex.Unlock()

	// Update user requests
	ob.userRequestsMutex.Lock()
	userRequests := ob.userRequests[request.BorrowerID]
	for i, id := range userRequests {
		if id == requestID {
			// Remove this request ID from the user's list
			ob.userRequests[request.BorrowerID] = append(userRequests[:i], userRequests[i+1:]...)
			break
		}
	}
	ob.userRequestsMutex.Unlock()

	// Broadcast to network if not skipped
	if !skipBroadcast {
		if err := ob.p2pNode.BroadcastMessage(p2p.OrderBookTopic, MsgTypeRemoveRequest, map[string]string{
			"request_id": requestID,
		}); err != nil {
			ob.logger.Error("Failed to broadcast request removal",
				zap.String("requestID", requestID),
				zap.Error(err))
			// Continue anyway, as the request is still removed locally
		}
	}

	ob.logger.Info("Removed loan request", zap.String("requestID", requestID))
	return nil
}

// GetOffer retrieves a loan offer by ID
func (ob *OrderBook) GetOffer(offerID string) (*types.LoanOffer, error) {
	ob.offersMutex.RLock()
	defer ob.offersMutex.RUnlock()

	offer, exists := ob.offers[offerID]
	if !exists {
		return nil, fmt.Errorf("offer with ID %s not found", offerID)
	}

	return offer, nil
}

// GetRequest retrieves a loan request by ID
func (ob *OrderBook) GetRequest(requestID string) (*types.LoanRequest, error) {
	ob.requestsMutex.RLock()
	defer ob.requestsMutex.RUnlock()

	request, exists := ob.requests[requestID]
	if !exists {
		return nil, fmt.Errorf("request with ID %s not found", requestID)
	}

	return request, nil
}

// GetOffersByUser retrieves all loan offers by a specific user
func (ob *OrderBook) GetOffersByUser(userID string) []*types.LoanOffer {
	ob.userOffersMutex.RLock()
	offerIDs := ob.userOffers[userID]
	ob.userOffersMutex.RUnlock()

	offers := make([]*types.LoanOffer, 0, len(offerIDs))
	ob.offersMutex.RLock()
	for _, id := range offerIDs {
		if offer, exists := ob.offers[id]; exists {
			offers = append(offers, offer)
		}
	}
	ob.offersMutex.RUnlock()

	return offers
}

// GetRequestsByUser retrieves all loan requests by a specific user
func (ob *OrderBook) GetRequestsByUser(userID string) []*types.LoanRequest {
	ob.userRequestsMutex.RLock()
	requestIDs := ob.userRequests[userID]
	ob.userRequestsMutex.RUnlock()

	requests := make([]*types.LoanRequest, 0, len(requestIDs))
	ob.requestsMutex.RLock()
	for _, id := range requestIDs {
		if request, exists := ob.requests[id]; exists {
			requests = append(requests, request)
		}
	}
	ob.requestsMutex.RUnlock()

	return requests
}

// GetAllOffers retrieves all active loan offers
func (ob *OrderBook) GetAllOffers() []*types.LoanOffer {
	ob.offersMutex.RLock()
	defer ob.offersMutex.RUnlock()

	offers := make([]*types.LoanOffer, 0, len(ob.offers))
	for _, offer := range ob.offers {
		offers = append(offers, offer)
	}

	return offers
}

// GetAllRequests retrieves all active loan requests
func (ob *OrderBook) GetAllRequests() []*types.LoanRequest {
	ob.requestsMutex.RLock()
	defer ob.requestsMutex.RUnlock()

	requests := make([]*types.LoanRequest, 0, len(ob.requests))
	for _, request := range ob.requests {
		requests = append(requests, request)
	}

	return requests
}

// GetMatchingOffers finds offers that match a loan request
func (ob *OrderBook) GetMatchingOffers(request *types.LoanRequest, p2pNode *p2p.P2PNode) []*types.LoanOffer {
	if request == nil || p2pNode == nil {
		ob.logger.Warn("GetMatchingOffers called with nil request or p2pNode")
		return nil
	}

	currentBTCPrice := p2pNode.GetLatestBTCPrice()
	if currentBTCPrice <= 0 {
		ob.logger.Warn("GetMatchingOffers: Invalid BTC price from P2P node", zap.Float64("price", currentBTCPrice))
		return nil // Cannot calculate LTV with invalid price
	}

	// Calculate current LTV
	ltv := request.CalculateLTV(currentBTCPrice)
	if ltv <= 0 {
		ob.logger.Debug("GetMatchingOffers: Skipping due to invalid LTV",
			zap.String("requestID", request.ID),
			zap.Float64("ltv", ltv),
			zap.Float64("btcPrice", currentBTCPrice))
		return nil // Invalid LTV
	}

	ob.offersMutex.RLock()
	defer ob.offersMutex.RUnlock()

	matchingOffers := make([]*types.LoanOffer, 0)
	for _, offer := range ob.offers {
		// Check if offer matches request criteria
		if offer.Status != types.StatusPending {
			continue // Skip non-pending offers
		}

		if offer.DurationDays < request.DurationDays {
			continue // Duration too short
		}

		if offer.DailyInterestBPS > request.MaxDailyInterestBPS {
			continue // Interest rate too high
		}

		if ltv < offer.MinLTVRatio {
			continue // LTV ratio too low
		}

		if offer.USDTAmount <= 0 {
			continue // Invalid amount
		}

		// This offer matches the criteria
		matchingOffers = append(matchingOffers, offer)
	}

	return matchingOffers
}

// SyncWithPeers initiates a sync with peers to ensure order book consistency
func (ob *OrderBook) SyncWithPeers() error {
	ob.syncMutex.Lock()
	if ob.syncInProgress {
		ob.syncMutex.Unlock()
		return errors.New("sync already in progress")
	}
	ob.syncInProgress = true
	ob.syncMutex.Unlock()

	defer func() {
		ob.syncMutex.Lock()
		ob.syncInProgress = false
		ob.syncMutex.Unlock()
	}()

	// Get list of peers
	peers := ob.p2pNode.GetPeers()
	if len(peers) == 0 {
		return errors.New("no peers to sync with")
	}

	ob.logger.Info("Starting order book sync", zap.Int("peerCount", len(peers)))

	// Send sync request to a random subset of peers
	maxPeersToSync := 5
	if len(peers) < maxPeersToSync {
		maxPeersToSync = len(peers)
	}

	for i := 0; i < maxPeersToSync; i++ {
		peerID := peers[i]
		if err := ob.p2pNode.SendDirectMessage(peerID, MsgTypeOrderBookQuery, map[string]interface{}{
			"timestamp": time.Now().Unix(),
		}); err != nil {
			ob.logger.Error("Failed to send sync request to peer",
				zap.String("peerID", peerID.Pretty()),
				zap.Error(err))
			continue
		}
	}

	return nil
}

// handleAddOffer handles an incoming add offer message
func (ob *OrderBook) handleAddOffer(sender peer.ID, msg *types.P2PMessage) error {
	var entry types.OrderBookEntry
	if err := json.Unmarshal(msg.Payload, &entry); err != nil {
		return fmt.Errorf("failed to unmarshal offer entry: %w", err)
	}

	// Verify entry type
	if entry.EntryType != "offer" {
		return fmt.Errorf("expected offer entry, got %s", entry.EntryType)
	}

	// Extract the offer data
	offerData, ok := entry.Data.(map[string]interface{})
	if !ok {
		return errors.New("invalid offer data format")
	}

	// Convert to LoanOffer
	offerBytes, err := json.Marshal(offerData)
	if err != nil {
		return fmt.Errorf("failed to marshal offer data: %w", err)
	}

	var offer types.LoanOffer
	if err := json.Unmarshal(offerBytes, &offer); err != nil {
		return fmt.Errorf("failed to unmarshal offer: %w", err)
	}

	// Check if we already have this offer
	ob.offersMutex.RLock()
	_, exists := ob.offers[offer.ID]
	ob.offersMutex.RUnlock()

	if exists {
		return nil // Already have this offer, ignore
	}

	// Validate the offer
	if err := offer.Validate(); err != nil {
		return fmt.Errorf("invalid offer from peer: %w", err)
	}

	// Add to local state (without broadcasting)
	ob.offersMutex.Lock()
	ob.offers[offer.ID] = &offer
	ob.offersMutex.Unlock()

	ob.entriesMutex.Lock()
	ob.entries[offer.ID] = &entry
	ob.entriesMutex.Unlock()

	ob.userOffersMutex.Lock()
	ob.userOffers[offer.LenderID] = append(ob.userOffers[offer.LenderID], offer.ID)
	ob.userOffersMutex.Unlock()

	ob.logger.Debug("Received new offer from peer",
		zap.String("offerID", offer.ID),
		zap.String("peer", sender.Pretty()))

	return nil
}

// handleRemoveOffer handles an incoming remove offer message
func (ob *OrderBook) handleRemoveOffer(sender peer.ID, msg *types.P2PMessage) error {
	var payload map[string]string
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("failed to unmarshal remove offer payload: %w", err)
	}

	offerID, ok := payload["offer_id"]
	if !ok {
		return errors.New("missing offer_id in remove offer message")
	}

	// Check if we have this offer
	ob.offersMutex.RLock()
	offer, exists := ob.offers[offerID]
	ob.offersMutex.RUnlock()

	if !exists {
		return nil // Don't have this offer, ignore
	}

	// Remove from local state (without broadcasting)
	ob.offersMutex.Lock()
	delete(ob.offers, offerID)
	ob.offersMutex.Unlock()

	ob.entriesMutex.Lock()
	delete(ob.entries, offerID)
	ob.entriesMutex.Unlock()

	// Update user offers
	ob.userOffersMutex.Lock()
	userOffers := ob.userOffers[offer.LenderID]
	for i, id := range userOffers {
		if id == offerID {
			// Remove this offer ID from the user's list
			ob.userOffers[offer.LenderID] = append(userOffers[:i], userOffers[i+1:]...)
			break
		}
	}
	ob.userOffersMutex.Unlock()

	ob.logger.Debug("Removed offer based on peer message",
		zap.String("offerID", offerID),
		zap.String("peer", sender.Pretty()))

	return nil
}

// handleAddRequest handles an incoming add request message
func (ob *OrderBook) handleAddRequest(sender peer.ID, msg *types.P2PMessage) error {
	var entry types.OrderBookEntry
	if err := json.Unmarshal(msg.Payload, &entry); err != nil {
		return fmt.Errorf("failed to unmarshal request entry: %w", err)
	}

	// Verify entry type
	if entry.EntryType != "request" {
		return fmt.Errorf("expected request entry, got %s", entry.EntryType)
	}

	// Extract the request data
	requestData, ok := entry.Data.(map[string]interface{})
	if !ok {
		return errors.New("invalid request data format")
	}

	// Convert to LoanRequest
	requestBytes, err := json.Marshal(requestData)
	if err != nil {
		return fmt.Errorf("failed to marshal request data: %w", err)
	}

	var request types.LoanRequest
	if err := json.Unmarshal(requestBytes, &request); err != nil {
		return fmt.Errorf("failed to unmarshal request: %w", err)
	}

	// Check if we already have this request
	ob.requestsMutex.RLock()
	_, exists := ob.requests[request.ID]
	ob.requestsMutex.RUnlock()

	if exists {
		return nil // Already have this request, ignore
	}

	// Validate the request
	if err := request.Validate(); err != nil {
		return fmt.Errorf("invalid request from peer: %w", err)
	}

	// Add to local state (without broadcasting)
	ob.requestsMutex.Lock()
	ob.requests[request.ID] = &request
	ob.requestsMutex.Unlock()

	ob.entriesMutex.Lock()
	ob.entries[request.ID] = &entry
	ob.entriesMutex.Unlock()

	ob.userRequestsMutex.Lock()
	ob.userRequests[request.BorrowerID] = append(ob.userRequests[request.BorrowerID], request.ID)
	ob.userRequestsMutex.Unlock()

	ob.logger.Debug("Received new request from peer",
		zap.String("requestID", request.ID),
		zap.String("peer", sender.Pretty()))

	return nil
}

// handleRemoveRequest handles an incoming remove request message
func (ob *OrderBook) handleRemoveRequest(sender peer.ID, msg *types.P2PMessage) error {
	var payload map[string]string
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("failed to unmarshal remove request payload: %w", err)
	}

	requestID, ok := payload["request_id"]
	if !ok {
		return errors.New("missing request_id in remove request message")
	}

	// Check if we have this request
	ob.requestsMutex.RLock()
	request, exists := ob.requests[requestID]
	ob.requestsMutex.RUnlock()

	if !exists {
		return nil // Don't have this request, ignore
	}

	// Remove from local state (without broadcasting)
	ob.requestsMutex.Lock()
	delete(ob.requests, requestID)
	ob.requestsMutex.Unlock()

	ob.entriesMutex.Lock()
	delete(ob.entries, requestID)
	ob.entriesMutex.Unlock()

	// Update user requests
	ob.userRequestsMutex.Lock()
	userRequests := ob.userRequests[request.BorrowerID]
	for i, id := range userRequests {
		if id == requestID {
			// Remove this request ID from the user's list
			ob.userRequests[request.BorrowerID] = append(userRequests[:i], userRequests[i+1:]...)
			break
		}
	}
	ob.userRequestsMutex.Unlock()

	ob.logger.Debug("Removed request based on peer message",
		zap.String("requestID", requestID),
		zap.String("peer", sender.Pretty()))

	return nil
}

// handleOrderBookSync handles an incoming order book sync message
func (ob *OrderBook) handleOrderBookSync(sender peer.ID, msg *types.P2PMessage) error {
	var payload struct {
		Entries []types.OrderBookEntry `json:"entries"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("failed to unmarshal orderbook sync payload: %w", err)
	}

	syncCount := 0
	for _, entry := range payload.Entries {
		// Skip entries that are too old
		if time.Since(entry.CreatedAt) > MaxEntryAge {
			continue
		}

		// Process based on entry type
		switch entry.EntryType {
		case "offer":
			// Extract and add the offer
			offerData, ok := entry.Data.(map[string]interface{})
			if !ok {
				continue // Invalid format, skip
			}

			offerBytes, err := json.Marshal(offerData)
			if err != nil {
				continue // Marshaling error, skip
			}

			var offer types.LoanOffer
			if err := json.Unmarshal(offerBytes, &offer); err != nil {
				continue // Unmarshaling error, skip
			}

			// Check if we already have this offer
			ob.offersMutex.RLock()
			_, exists := ob.offers[offer.ID]
			ob.offersMutex.RUnlock()

			if !exists {
				// Add to local state (without broadcasting)
				if err := offer.Validate(); err == nil {
					ob.offersMutex.Lock()
					ob.offers[offer.ID] = &offer
					ob.offersMutex.Unlock()

					ob.entriesMutex.Lock()
					ob.entries[offer.ID] = &entry
					ob.entriesMutex.Unlock()

					ob.userOffersMutex.Lock()
					ob.userOffers[offer.LenderID] = append(ob.userOffers[offer.LenderID], offer.ID)
					ob.userOffersMutex.Unlock()

					syncCount++
				}
			}

		case "request":
			// Extract and add the request
			requestData, ok := entry.Data.(map[string]interface{})
			if !ok {
				continue // Invalid format, skip
			}

			requestBytes, err := json.Marshal(requestData)
			if err != nil {
				continue // Marshaling error, skip
			}

			var request types.LoanRequest
			if err := json.Unmarshal(requestBytes, &request); err != nil {
				continue // Unmarshaling error, skip
			}

			// Check if we already have this request
			ob.requestsMutex.RLock()
			_, exists := ob.requests[request.ID]
			ob.requestsMutex.RUnlock()

			if !exists {
				// Add to local state (without broadcasting)
				if err := request.Validate(); err == nil {
					ob.requestsMutex.Lock()
					ob.requests[request.ID] = &request
					ob.requestsMutex.Unlock()

					ob.entriesMutex.Lock()
					ob.entries[request.ID] = &entry
					ob.entriesMutex.Unlock()

					ob.userRequestsMutex.Lock()
					ob.userRequests[request.BorrowerID] = append(ob.userRequests[request.BorrowerID], request.ID)
					ob.userRequestsMutex.Unlock()

					syncCount++
				}
			}
		}
	}

	if syncCount > 0 {
		ob.logger.Info("Synced order book entries from peer",
			zap.Int("syncedEntries", syncCount),
			zap.String("peer", sender.Pretty()))
	}

	return nil
}

// handleOrderBookQuery handles a query for order book entries
func (ob *OrderBook) handleOrderBookQuery(sender peer.ID, msg *types.P2PMessage) error {
	var payload struct {
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("failed to unmarshal orderbook query: %w", err)
	}

	// Get all entries
	ob.entriesMutex.RLock()
	entries := make([]types.OrderBookEntry, 0, len(ob.entries))
	for _, entry := range ob.entries {
		entries = append(entries, *entry)
	}
	ob.entriesMutex.RUnlock()

	// Limit the number of entries to avoid large messages
	if len(entries) > MaxSyncEntries {
		// Sort by creation time (most recent first) and take only the most recent
		// For simplicity in the MVP, we'll just take a random subset
		entries = entries[:MaxSyncEntries]
	}

	// Send the response
	response := struct {
		Entries []types.OrderBookEntry `json:"entries"`
	}{
		Entries: entries,
	}

	if err := ob.p2pNode.SendDirectMessage(sender, MsgTypeOrderBookSync, response); err != nil {
		return fmt.Errorf("failed to send orderbook sync response: %w", err)
	}

	ob.logger.Debug("Sent order book entries to peer",
		zap.Int("entryCount", len(entries)),
		zap.String("peer", sender.Pretty()))

	return nil
}

// GetStats returns statistics about the order book
func (ob *OrderBook) GetStats() map[string]interface{} {
	ob.offersMutex.RLock()
	offerCount := len(ob.offers)
	ob.offersMutex.RUnlock()

	ob.requestsMutex.RLock()
	requestCount := len(ob.requests)
	ob.requestsMutex.RUnlock()

	ob.userOffersMutex.RLock()
	lenderCount := len(ob.userOffers)
	ob.userOffersMutex.RUnlock()

	ob.userRequestsMutex.RLock()
	borrowerCount := len(ob.userRequests)
	ob.userRequestsMutex.RUnlock()

	// Calculate total USDT offered
	totalUSDTOffered := 0.0
	ob.offersMutex.RLock()
	for _, offer := range ob.offers {
		totalUSDTOffered += offer.USDTAmount
	}
	ob.offersMutex.RUnlock()

	// Calculate total BTC collateral requested
	totalBTCRequested := 0.0
	ob.requestsMutex.RLock()
	for _, request := range ob.requests {
		totalBTCRequested += request.BTCCollateral
	}
	ob.requestsMutex.RUnlock()

	return map[string]interface{}{
		"offer_count":         offerCount,
		"request_count":       requestCount,
		"lender_count":        lenderCount,
		"borrower_count":      borrowerCount,
		"total_usdt_offered":  totalUSDTOffered,
		"total_btc_requested": totalBTCRequested,
		"peer_count":          ob.p2pNode.GetPeerCount(),
	}
}
