package matching

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/kreutix/llp/pkg/orderbook"
	"github.com/kreutix/llp/pkg/types"
	"go.uber.org/zap"
)

// MatchingConfig contains configuration parameters for the matching algorithm
type MatchingConfig struct {
	// Minimum reputation score required for matching
	MinReputationScore int

	// Maximum number of lenders to match with a single request
	MaxLendersPerMatch int

	// Reputation weight factor (0.0-1.0) - higher values give more importance to reputation
	ReputationWeight float64

	// Whether to prioritize complete matches (filling the entire request)
	PrioritizeCompleteMatches bool

	// Minimum percentage of the request amount that must be matched
	MinMatchPercentage float64
}

// DefaultMatchingConfig returns a default configuration for the matcher
func DefaultMatchingConfig() MatchingConfig {
	return MatchingConfig{
		MinReputationScore:        types.DefaultReputationScore / 2, // 250
		MaxLendersPerMatch:        10,
		ReputationWeight:          0.2, // 20% weight to reputation, 80% to interest rate
		PrioritizeCompleteMatches: true,
		MinMatchPercentage:        0.8, // Match at least 80% of the requested amount
	}
}

// Matcher implements the matching algorithm for the Lightning Loan Protocol
type Matcher struct {
	orderBook *orderbook.OrderBook
	config    MatchingConfig
	logger    *zap.Logger
	// Map of user IDs to reputation scores (in a real implementation, this would be a separate service)
	reputationStore   map[string]int
	reputationMutex   sync.RWMutex // To protect reputationStore
	activeMatches     map[string]*types.LoanMatch
	activeMatchesMutex sync.RWMutex
}

// NewMatcher creates a new matcher with the given order book and configuration
func NewMatcher(ob *orderbook.OrderBook, config MatchingConfig, logger *zap.Logger) *Matcher {
	return &Matcher{
		orderBook:       ob,
		config:          config,
		logger:          logger,
		reputationStore: make(map[string]int),
		activeMatches:   make(map[string]*types.LoanMatch),
	}
}

// SetOrderBook allows setting the order book after matcher initialization.
// This is used to resolve circular dependency between Matcher and P2PNode/OrderBook.
func (m *Matcher) SetOrderBook(ob *orderbook.OrderBook) {
	m.orderBook = ob
}

// SetUserReputation sets the reputation score for a user in a thread-safe way
// SetUserReputation sets the reputation score for a user in a thread-safe way
func (m *Matcher) SetUserReputation(userID string, score int) {
	m.reputationMutex.Lock()
	defer m.reputationMutex.Unlock()
	if score < types.MinReputationScore {
		score = types.MinReputationScore
	}
	if score > types.MaxReputationScore {
		score = types.MaxReputationScore
	}
	m.reputationStore[userID] = score
	m.logger.Debug("Set reputation", zap.String("userID", userID), zap.Int("score", score))
}

// GetUserReputation gets the reputation score for a user in a thread-safe way
func (m *Matcher) GetUserReputation(userID string) int {
	m.reputationMutex.RLock()
	defer m.reputationMutex.RUnlock()
	score, exists := m.reputationStore[userID]
	if !exists {
		return types.DefaultReputationScore
	}
	return score
}

// MatchRequest attempts to match a loan request with available offers
func (m *Matcher) MatchRequest(requestID string, p2pNode *orderbook.P2PNode) (*types.LoanMatch, error) { // Changed p2p.P2PNode to orderbook.P2PNode
	if p2pNode == nil {
		return nil, fmt.Errorf("p2pNode cannot be nil")
	}
	// Get the request
	request, err := m.orderBook.GetRequest(requestID)
	if err != nil {
		return nil, fmt.Errorf("failed to get request: %w", err)
	}

	// Check if the request is still pending
	if request.Status != types.StatusPending {
		return nil, fmt.Errorf("request is not pending (status: %s)", request.Status)
	}

	// Get current BTC price from P2P node
	btcPriceUSDT := p2pNode.GetLatestBTCPrice()
	if btcPriceUSDT <= 0 {
		return nil, fmt.Errorf("invalid BTC price from P2P node: %f", btcPriceUSDT)
	}

	// Calculate current LTV
	ltv := request.CalculateLTV(btcPriceUSDT)
	if ltv <= 0 {
		return nil, fmt.Errorf("invalid LTV ratio: %f (BTC price: %f)", ltv, btcPriceUSDT)
	}

	// Get matching offers
	matchingOffers := m.orderBook.GetMatchingOffers(request, p2pNode) // Pass p2pNode
	if len(matchingOffers) == 0 {
		return nil, fmt.Errorf("no matching offers found for request %s (BTC price: %f, LTV: %f)", requestID, btcPriceUSDT, ltv)
	}

	// Get borrower reputation
	borrowerReputation := m.GetUserReputation(request.BorrowerID)
	if borrowerReputation < m.config.MinReputationScore {
		return nil, fmt.Errorf("borrower reputation score (%d) is below minimum required (%d)",
			borrowerReputation, m.config.MinReputationScore)
	}

	// Score and sort offers
	scoredOffers := m.scoreOffers(matchingOffers, request)

	// Select the best combination of offers
	selectedOffers, totalAmount, err := m.selectOffers(scoredOffers, request.USDTAmount)
	if err != nil {
		return nil, fmt.Errorf("failed to select offers: %w", err)
	}

	// Check if we have enough funds
	if totalAmount < request.USDTAmount*m.config.MinMatchPercentage {
		return nil, fmt.Errorf("insufficient matching offers: %f USDT available, %f USDT requested",
			totalAmount, request.USDTAmount)
	}

	// Create the match
	match := &types.LoanMatch{
		ID:              uuid.New().String(),
		RequestID:       request.ID,
		BorrowerID:      request.BorrowerID,
		MatchedLenders:  make([]types.MatchedLender, 0, len(selectedOffers)),
		TotalUSDTAmount: totalAmount,
		BTCCollateral:   request.BTCCollateral,
		DurationDays:    request.DurationDays,
		StartDate:       time.Now(),
		EndDate:         time.Now().AddDate(0, 0, request.DurationDays),
		Status:          types.StatusPending,
		CurrentLTV:      ltv,
		NextPaymentDue:  time.Now().AddDate(0, 0, 1), // First payment due in 1 day
	}

	// Add matched lenders
	for _, offer := range selectedOffers {
		match.MatchedLenders = append(match.MatchedLenders, types.MatchedLender{
			LenderID:         offer.offer.LenderID,
			OfferID:          offer.offer.ID,
			USDTAmount:       offer.allocatedAmount,
			DailyInterestBPS: offer.offer.DailyInterestBPS,
			LightningNodeID:  offer.offer.LightningNodeID,
		})
	}

	m.logger.Info("Created loan match",
		zap.String("matchID", match.ID),
		zap.String("requestID", request.ID),
		zap.String("borrowerID", request.BorrowerID),
		zap.Int("lenderCount", len(match.MatchedLenders)),
		zap.Float64("totalAmount", match.TotalUSDTAmount),
		zap.Float64("requestedAmount", request.USDTAmount),
		zap.Float64("ltv", ltv))

	return match, nil
}

// scoredOffer represents an offer with its score and allocated amount
type scoredOffer struct {
	offer            *types.LoanOffer
	score            float64
	allocatedAmount  float64
	lenderReputation int
}

// scoreOffers scores and sorts offers based on interest rate and reputation
func (m *Matcher) scoreOffers(offers []*types.LoanOffer, request *types.LoanRequest) []scoredOffer {
	scoredOffers := make([]scoredOffer, 0, len(offers))

	// Calculate scores for each offer
	for _, offer := range offers {
		// Get lender reputation
		lenderReputation := m.GetUserReputation(offer.LenderID)
		if lenderReputation < m.config.MinReputationScore {
			continue // Skip lenders with low reputation
		}

		// Calculate reputation factor (0-1 range)
		reputationFactor := float64(lenderReputation) / float64(types.MaxReputationScore)

		// Calculate interest rate factor (lower is better)
		// Normalize to 0-1 range where 0 is the maximum interest and 1 is 0 interest
		interestFactor := 1.0 - (float64(offer.DailyInterestBPS) / float64(request.MaxDailyInterestBPS))
		if interestFactor < 0 {
			interestFactor = 0
		}

		// Calculate combined score (higher is better)
		// Weight the factors according to configuration
		score := (interestFactor * (1 - m.config.ReputationWeight)) +
			(reputationFactor * m.config.ReputationWeight)

		// Add to scored offers
		scoredOffers = append(scoredOffers, scoredOffer{
			offer:            offer,
			score:            score,
			allocatedAmount:  0,
			lenderReputation: lenderReputation,
		})
	}

	// Sort by score (highest first)
	sort.Slice(scoredOffers, func(i, j int) bool {
		return scoredOffers[i].score > scoredOffers[j].score
	})

	return scoredOffers
}

// selectOffers selects the best combination of offers to fulfill the requested amount
func (m *Matcher) selectOffers(scoredOffers []scoredOffer, requestedAmount float64) ([]scoredOffer, float64, error) {
	if len(scoredOffers) == 0 {
		return nil, 0, fmt.Errorf("no scored offers available")
	}

	// Try to find a complete match first if configured to prioritize complete matches
	if m.config.PrioritizeCompleteMatches {
		completeMatch, totalAmount := m.findCompleteMatch(scoredOffers, requestedAmount)
		if len(completeMatch) > 0 {
			return completeMatch, totalAmount, nil
		}
	}

	// Otherwise, allocate from best offers until we reach the requested amount or run out of offers
	selectedOffers := make([]scoredOffer, 0)
	remainingAmount := requestedAmount
	totalAllocated := 0.0

	// First pass: try to allocate from each offer
	for i := range scoredOffers {
		if len(selectedOffers) >= m.config.MaxLendersPerMatch {
			break // Limit the number of lenders
		}

		offer := &scoredOffers[i]
		if remainingAmount <= 0 {
			break
		}

		// Allocate either the full offer amount or the remaining needed amount
		allocateAmount := offer.offer.USDTAmount
		if allocateAmount > remainingAmount {
			allocateAmount = remainingAmount
		}

		offer.allocatedAmount = allocateAmount
		remainingAmount -= allocateAmount
		totalAllocated += allocateAmount
		selectedOffers = append(selectedOffers, *offer)
	}

	// If we couldn't allocate the full amount but got more than the minimum percentage,
	// return what we have
	if totalAllocated >= requestedAmount*m.config.MinMatchPercentage {
		return selectedOffers, totalAllocated, nil
	}

	// If we're here, we couldn't find a good match
	if len(selectedOffers) == 0 {
		return nil, 0, fmt.Errorf("could not allocate any offers")
	}

	return selectedOffers, totalAllocated, nil
}

// findCompleteMatch tries to find a combination of offers that exactly matches the requested amount
func (m *Matcher) findCompleteMatch(scoredOffers []scoredOffer, requestedAmount float64) ([]scoredOffer, float64) {
	// This is a simplified approach. A more sophisticated algorithm would use dynamic programming
	// to find the optimal combination, but for MVP purposes, we'll use a greedy approach.

	// Sort by interest rate (lowest first) to minimize cost
	sortedByInterest := make([]scoredOffer, len(scoredOffers))
	copy(sortedByInterest, scoredOffers)
	sort.Slice(sortedByInterest, func(i, j int) bool {
		return sortedByInterest[i].offer.DailyInterestBPS < sortedByInterest[j].offer.DailyInterestBPS
	})

	selectedOffers := make([]scoredOffer, 0)
	remainingAmount := requestedAmount
	totalAllocated := 0.0

	for i := range sortedByInterest {
		if len(selectedOffers) >= m.config.MaxLendersPerMatch {
			break // Limit the number of lenders
		}

		offer := &sortedByInterest[i]
		if remainingAmount <= 0 {
			break
		}

		// Allocate either the full offer amount or the remaining needed amount
		allocateAmount := offer.offer.USDTAmount
		if allocateAmount > remainingAmount {
			allocateAmount = remainingAmount
		}

		offer.allocatedAmount = allocateAmount
		remainingAmount -= allocateAmount
		totalAllocated += allocateAmount
		selectedOffers = append(selectedOffers, *offer)
	}

	// If we allocated exactly the requested amount, return the match
	if totalAllocated == requestedAmount {
		return selectedOffers, totalAllocated
	}

	// If we're very close (within 0.1%), consider it a complete match
	if totalAllocated >= requestedAmount*0.999 && totalAllocated <= requestedAmount*1.001 {
		return selectedOffers, totalAllocated
	}

	// Otherwise, return empty to indicate no complete match found
	return []scoredOffer{}, 0
}

// CalculateTotalDailyInterest calculates the total daily interest for a match
func (m *Matcher) CalculateTotalDailyInterest(match *types.LoanMatch) float64 {
	totalInterest := 0.0
	for _, lender := range match.MatchedLenders {
		interest := lender.USDTAmount * float64(lender.DailyInterestBPS) / 10000.0
		totalInterest += interest
	}
	return totalInterest
}

// CalculateEffectiveInterestRate calculates the effective daily interest rate for a match
func (m *Matcher) CalculateEffectiveInterestRate(match *types.LoanMatch) float64 {
	if match.TotalUSDTAmount <= 0 {
		return 0
	}
	dailyInterest := m.CalculateTotalDailyInterest(match)
	return (dailyInterest / match.TotalUSDTAmount) * 10000.0 // Convert to basis points
}

// FinalizeLoanMatch finalizes a loan match by updating the status of offers and requests
func (m *Matcher) FinalizeLoanMatch(match *types.LoanMatch) error {
	// Get the request
	request, err := m.orderBook.GetRequest(match.RequestID)
	if err != nil {
		return fmt.Errorf("failed to get request: %w", err)
	}

	// Update request status
	request.Status = types.StatusActive

	// Update offer statuses and amounts
	for _, matchedLender := range match.MatchedLenders {
		offer, err := m.orderBook.GetOffer(matchedLender.OfferID)
		if err != nil {
			return fmt.Errorf("failed to get offer %s: %w", matchedLender.OfferID, err)
		}

		// Reduce the offer amount by the allocated amount
		offer.USDTAmount -= matchedLender.USDTAmount

		// If the offer is fully used, mark it as completed
		if offer.USDTAmount <= 0 {
			offer.Status = types.StatusCompleted
			// Remove from order book
			if err := m.orderBook.RemoveOffer(offer.ID); err != nil {
				m.logger.Warn("Failed to remove completed offer",
					zap.String("offerID", offer.ID),
					zap.Error(err))
			}
		}
	}

	// Remove the request from the order book
	if err := m.orderBook.RemoveRequest(request.ID); err != nil {
		m.logger.Warn("Failed to remove matched request",
			zap.String("requestID", request.ID),
			zap.Error(err))
	}

	m.logger.Info("Finalized loan match",
		zap.String("matchID", match.ID),
		zap.String("requestID", match.RequestID),
		zap.String("borrowerID", match.BorrowerID),
		zap.Int("lenderCount", len(match.MatchedLenders)),
		zap.Float64("effectiveInterestRate", m.CalculateEffectiveInterestRate(match)))

	// Add to active matches
	m.activeMatchesMutex.Lock()
	m.activeMatches[match.ID] = match
	m.activeMatchesMutex.Unlock()
	m.logger.Info("Added match to active matches", zap.String("matchID", match.ID))

	return nil
}

// GetActiveMatch retrieves an active match by its ID
func (m *Matcher) GetActiveMatch(matchID string) (*types.LoanMatch, bool) {
	m.activeMatchesMutex.RLock()
	defer m.activeMatchesMutex.RUnlock()
	match, found := m.activeMatches[matchID]
	return match, found
}

// GetAllActiveMatches retrieves all active matches
func (m *Matcher) GetAllActiveMatches() []*types.LoanMatch {
	m.activeMatchesMutex.RLock()
	defer m.activeMatchesMutex.RUnlock()
	matches := make([]*types.LoanMatch, 0, len(m.activeMatches))
	for _, match := range m.activeMatches {
		matches = append(matches, match)
	}
	return matches
}

// UpdateReputationAndFinalizeMatch updates user reputations based on loan outcome and finalizes the match.
// p2pNodeInterface is used to avoid direct import of p2p package, promoting loose coupling.
type p2pNodeInterface interface {
	PublishReputationUpdate(*types.ReputationUpdate) error
}

func (m *Matcher) UpdateReputationAndFinalizeMatch(matchID string, wasSuccessful bool, finalStatus types.LoanStatus, p2pNode p2pNodeInterface) error {
	m.activeMatchesMutex.Lock() // Ensure exclusive access for modification and removal
	defer m.activeMatchesMutex.Unlock()

	match, exists := m.activeMatches[matchID]
	if !exists {
		return fmt.Errorf("match with ID %s not found in active matches", matchID)
	}

	// Prevent re-processing if already in a terminal state by some other means (though caller should ideally prevent this)
	if match.Status == types.StatusCompleted || match.Status == types.StatusDefaulted || match.Status == types.StatusLiquidated {
		m.logger.Info("Match already in a terminal state, no further updates", zap.String("matchID", matchID), zap.String("status", string(match.Status)))
		// remove from active matches if somehow it's still there
		delete(m.activeMatches, matchID)
		return nil
	}

	var borrowerRepChange, lenderRepChange int
	var reputationReason string

	// Set final status of the match
	match.Status = finalStatus

	if wasSuccessful { // Primarily for successful repayment
		match.RepaidSuccessfully = true
		match.Defaulted = false // Explicitly set, though should be default
		borrowerRepChange = 10
		lenderRepChange = 5
		reputationReason = "Loan repaid successfully"
		m.logger.Info("Loan status updated", zap.String("matchID", matchID), zap.String("borrowerID", match.BorrowerID), zap.String("newStatus", string(finalStatus)))
	} else { // Covers defaults and liquidations for reputation purposes
		match.RepaidSuccessfully = false
		match.Defaulted = true // True for both payment default and liquidation from a repayment perspective
		borrowerRepChange = -50
		lenderRepChange = -5 // Penalize lenders slightly for association

		switch finalStatus {
		case types.StatusDefaulted:
			reputationReason = "Loan defaulted (missed payments)"
		case types.StatusLiquidated:
			reputationReason = "Loan liquidated (LTV exceeded)"
		default:
			reputationReason = "Loan ended unsuccessfully"
		}
		m.logger.Warn("Loan status updated", zap.String("matchID", matchID), zap.String("borrowerID", match.BorrowerID), zap.String("newStatus", string(finalStatus)))
	}

	// Update borrower's reputation
	oldBorrowerScore := m.GetUserReputation(match.BorrowerID)
	newBorrowerScore := oldBorrowerScore + borrowerRepChange
	m.SetUserReputation(match.BorrowerID, newBorrowerScore)
	m.logger.Info("Borrower reputation updated",
		zap.String("userID", match.BorrowerID),
		zap.Int("oldScore", oldBorrowerScore),
		zap.Int("newScore", newBorrowerScore),
		zap.String("reason", reputationReason))

	if p2pNode != nil {
		reputationUpdate := &types.ReputationUpdate{
			UserID:        match.BorrowerID,
			OldScore:      oldBorrowerScore,
			NewScore:      newBorrowerScore,
			Reason:        fmt.Sprintf("%s (Borrower)", reputationReason),
			RelatedLoanID: match.ID,
			Timestamp:     time.Now(),
		}
		if err := p2pNode.PublishReputationUpdate(reputationUpdate); err != nil {
			m.logger.Error("Failed to publish borrower reputation update", zap.Error(err), zap.String("userID", match.BorrowerID))
		}
	}

	// Update lenders' reputation
	for _, lender := range match.MatchedLenders {
		oldLenderScore := m.GetUserReputation(lender.LenderID)
		newLenderScore := oldLenderScore + lenderRepChange
		m.SetUserReputation(lender.LenderID, newLenderScore)
		m.logger.Info("Lender reputation updated",
			zap.String("userID", lender.LenderID),
			zap.Int("oldScore", oldLenderScore),
			zap.Int("newScore", newLenderScore),
			zap.String("reason", fmt.Sprintf("%s (Lender)", reputationReason)))

		if p2pNode != nil {
			lenderUpdate := &types.ReputationUpdate{
				UserID:        lender.LenderID,
				OldScore:      oldLenderScore,
				NewScore:      newLenderScore,
				Reason:        fmt.Sprintf("%s (Lender)", reputationReason),
				RelatedLoanID: match.ID,
				Timestamp:     time.Now(),
			}
			if err := p2pNode.PublishReputationUpdate(lenderUpdate); err != nil {
				m.logger.Error("Failed to publish lender reputation update", zap.Error(err), zap.String("userID", lender.LenderID))
			}
		}
	}

	// Remove the match from activeMatches as it's now completed or defaulted
	delete(m.activeMatches, matchID)
	m.logger.Info("Removed match from active matches", zap.String("matchID", matchID), zap.String("status", string(match.Status)))

	return nil
}

// CheckLTVRatio checks if the current LTV ratio is within acceptable limits
// TODO: This function seems to be unused by CLI commands directly, but good to update for internal use.
func (m *Matcher) CheckLTVRatio(match *types.LoanMatch, p2pNode *orderbook.P2PNode) (float64, bool) { // Changed p2p.P2PNode to orderbook.P2PNode
	if p2pNode == nil {
		m.logger.Warn("CheckLTVRatio called with nil p2pNode")
		return 0, false
	}
	currentBTCPriceUSDT := p2pNode.GetLatestBTCPrice()
	if currentBTCPriceUSDT <= 0 || match.BTCCollateral <= 0 {
		m.logger.Warn("CheckLTVRatio: Invalid price or collateral",
			zap.Float64("btcPrice", currentBTCPriceUSDT),
			zap.Float64("collateral", match.BTCCollateral))
		return 0, false
	}

	collateralValueUSDT := match.BTCCollateral * currentBTCPriceUSDT
	currentLTV := match.TotalUSDTAmount / collateralValueUSDT

	// Update the match LTV
	match.CurrentLTV = currentLTV

	// Check if LTV is below liquidation threshold
	return currentLTV, currentLTV < types.LiquidationLTVRatio
}

// GetMatchesForUser retrieves all loan matches for a specific user (either as borrower or lender)
func (m *Matcher) GetMatchesForUser(userID string, role types.UserRole) []*types.LoanMatch {
	// In a real implementation, this would query a database of matches
	// For MVP, this is a placeholder
	return []*types.LoanMatch{}
}

// GetMatchByID retrieves a loan match by ID (deprecated, use GetActiveMatch or a persistent store in future)
func (m *Matcher) GetMatchByID(matchID string) (*types.LoanMatch, error) {
	match, found := m.GetActiveMatch(matchID)
	if !found {
		return nil, fmt.Errorf("active match with ID %s not found", matchID)
	}
	return match, nil
}
