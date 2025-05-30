package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Constants for the Lightning Loan Protocol
const (
	// Minimum and maximum loan durations
	MinLoanDurationDays = 1
	MaxLoanDurationDays = 365

	// Minimum and maximum interest rates (daily, in basis points)
	MinDailyInterestBPS = 1    // 0.01% per day
	MaxDailyInterestBPS = 1000 // 10% per day

	// LTV (Loan-to-Value) ratio thresholds
	MinLTVRatio         = 0.3 // 30%
	DefaultLTVRatio     = 0.5 // 50%
	WarningLTVRatio     = 0.7 // 70%
	LiquidationLTVRatio = 0.8 // 80%

	// Minimum loan and collateral amounts
	MinLoanAmountUSDT = 100   // 100 USDT
	MinCollateralBTC  = 0.001 // 0.001 BTC

	// Reputation score ranges
	MinReputationScore     = 0
	MaxReputationScore     = 1000
	DefaultReputationScore = 500
)

// LoanStatus represents the current state of a loan
type LoanStatus string

const (
	StatusPending    LoanStatus = "pending"    // Waiting for match
	StatusActive     LoanStatus = "active"     // Loan is active
	StatusCompleted  LoanStatus = "completed"  // Loan was repaid successfully
	StatusLiquidated LoanStatus = "liquidated" // Collateral was liquidated
	StatusCancelled  LoanStatus = "cancelled"  // Offer/request was cancelled
	StatusDefaulted  LoanStatus = "defaulted"  // Borrower defaulted on payments
)

// UserRole identifies whether a user is a borrower or lender
type UserRole string

const (
	RoleBorrower UserRole = "borrower"
	RoleLender   UserRole = "lender"
)

// User represents a participant in the Lightning Loan Protocol
type User struct {
	ID              string     `json:"id"`               // Unique identifier (public key)
	ReputationScore int        `json:"reputation_score"` // 0-1000 score based on history
	TotalLoans      int        `json:"total_loans"`      // Total number of loans participated in
	SuccessfulLoans int        `json:"successful_loans"` // Successfully completed loans
	DefaultedLoans  int        `json:"defaulted_loans"`  // Number of defaults
	Roles           []UserRole `json:"roles"`            // Roles this user has played
}

// Validate checks if the User struct is valid
func (u *User) Validate() error {
	if u.ID == "" {
		return errors.New("user ID cannot be empty")
	}
	if u.ReputationScore < MinReputationScore || u.ReputationScore > MaxReputationScore {
		return fmt.Errorf("reputation score must be between %d and %d", MinReputationScore, MaxReputationScore)
	}
	return nil
}

// LoanOffer represents a lender's offer to provide a USDT loan
type LoanOffer struct {
	ID               string     `json:"id"`                 // Unique offer ID
	LenderID         string     `json:"lender_id"`          // ID of the lender
	USDTAmount       float64    `json:"usdt_amount"`        // Amount of USDT offered
	DurationDays     int        `json:"duration_days"`      // Loan duration in days
	DailyInterestBPS int        `json:"daily_interest_bps"` // Daily interest rate in basis points (1 BPS = 0.01%)
	MinLTVRatio      float64    `json:"min_ltv_ratio"`      // Minimum loan-to-value ratio required
	CreatedAt        time.Time  `json:"created_at"`         // When the offer was created
	ExpiresAt        time.Time  `json:"expires_at"`         // When the offer expires
	Status           LoanStatus `json:"status"`             // Current status of the offer
	LightningNodeID  string     `json:"lightning_node_id"`  // Lightning Network node ID for payments
}

// Validate checks if the LoanOffer is valid
func (o *LoanOffer) Validate() error {
	if o.LenderID == "" {
		return errors.New("lender ID cannot be empty")
	}
	if o.USDTAmount < MinLoanAmountUSDT {
		return fmt.Errorf("USDT amount must be at least %f", MinLoanAmountUSDT)
	}
	if o.DurationDays < MinLoanDurationDays || o.DurationDays > MaxLoanDurationDays {
		return fmt.Errorf("duration must be between %d and %d days", MinLoanDurationDays, MaxLoanDurationDays)
	}
	if o.DailyInterestBPS < MinDailyInterestBPS || o.DailyInterestBPS > MaxDailyInterestBPS {
		return fmt.Errorf("daily interest rate must be between %d and %d basis points",
			MinDailyInterestBPS, MaxDailyInterestBPS)
	}
	if o.MinLTVRatio < MinLTVRatio || o.MinLTVRatio > LiquidationLTVRatio {
		return fmt.Errorf("LTV ratio must be between %f and %f", MinLTVRatio, LiquidationLTVRatio)
	}
	if o.LightningNodeID == "" {
		return errors.New("Lightning Network node ID cannot be empty")
	}
	return nil
}

// CalculateDailyInterest calculates the daily interest amount in USDT
func (o *LoanOffer) CalculateDailyInterest() float64 {
	return o.USDTAmount * float64(o.DailyInterestBPS) / 10000.0
}

// LoanRequest represents a borrower's request for a USDT loan
type LoanRequest struct {
	ID                  string     `json:"id"`                     // Unique request ID
	BorrowerID          string     `json:"borrower_id"`            // ID of the borrower
	USDTAmount          float64    `json:"usdt_amount"`            // Amount of USDT requested
	BTCCollateral       float64    `json:"btc_collateral"`         // Amount of BTC offered as collateral
	MaxDailyInterestBPS int        `json:"max_daily_interest_bps"` // Maximum daily interest rate in basis points
	DurationDays        int        `json:"duration_days"`          // Requested loan duration in days
	CreatedAt           time.Time  `json:"created_at"`             // When the request was created
	ExpiresAt           time.Time  `json:"expires_at"`             // When the request expires
	Status              LoanStatus `json:"status"`                 // Current status of the request
	TaprootAddress      string     `json:"taproot_address"`        // Taproot address for collateral
	LightningNodeID     string     `json:"lightning_node_id"`      // Lightning Network node ID for payments
}

// Validate checks if the LoanRequest is valid
func (r *LoanRequest) Validate() error {
	if r.BorrowerID == "" {
		return errors.New("borrower ID cannot be empty")
	}
	if r.USDTAmount < MinLoanAmountUSDT {
		return fmt.Errorf("USDT amount must be at least %f", MinLoanAmountUSDT)
	}
	if r.BTCCollateral < MinCollateralBTC {
		return fmt.Errorf("BTC collateral must be at least %f", MinCollateralBTC)
	}
	if r.DurationDays < MinLoanDurationDays || r.DurationDays > MaxLoanDurationDays {
		return fmt.Errorf("duration must be between %d and %d days", MinLoanDurationDays, MaxLoanDurationDays)
	}
	if r.MaxDailyInterestBPS < MinDailyInterestBPS || r.MaxDailyInterestBPS > MaxDailyInterestBPS {
		return fmt.Errorf("max daily interest rate must be between %d and %d basis points",
			MinDailyInterestBPS, MaxDailyInterestBPS)
	}
	if r.TaprootAddress == "" {
		return errors.New("Taproot address cannot be empty")
	}
	if r.LightningNodeID == "" {
		return errors.New("Lightning Network node ID cannot be empty")
	}
	return nil
}

// CalculateLTV calculates the current Loan-to-Value ratio based on current BTC price
func (r *LoanRequest) CalculateLTV(btcPriceUSDT float64) float64 {
	if btcPriceUSDT <= 0 || r.BTCCollateral <= 0 {
		return 0
	}
	collateralValueUSDT := r.BTCCollateral * btcPriceUSDT
	return r.USDTAmount / collateralValueUSDT
}

// MatchedLender represents a lender that has been matched to a loan request
type MatchedLender struct {
	LenderID         string  `json:"lender_id"`          // ID of the lender
	OfferID          string  `json:"offer_id"`           // ID of the original offer
	USDTAmount       float64 `json:"usdt_amount"`        // Amount of USDT provided by this lender
	DailyInterestBPS int     `json:"daily_interest_bps"` // Daily interest rate in basis points
	LightningNodeID  string  `json:"lightning_node_id"`  // Lightning Network node ID
}

// LoanMatch represents a match between a borrower and one or more lenders
type LoanMatch struct {
	ID                string          `json:"id"`                  // Unique match ID
	RequestID         string          `json:"request_id"`          // ID of the loan request
	BorrowerID        string          `json:"borrower_id"`         // ID of the borrower
	MatchedLenders    []MatchedLender `json:"matched_lenders"`     // List of matched lenders
	TotalUSDTAmount   float64         `json:"total_usdt_amount"`   // Total USDT amount matched
	BTCCollateral     float64         `json:"btc_collateral"`      // BTC collateral amount
	DurationDays      int             `json:"duration_days"`       // Loan duration in days
	StartDate         time.Time       `json:"start_date"`          // When the loan starts
	EndDate           time.Time       `json:"end_date"`            // When the loan ends
	Status            LoanStatus      `json:"status"`              // Current status of the match
	CollateralTxID    string          `json:"collateral_tx_id"`    // Bitcoin transaction ID for collateral
	TaprootScriptHash string          `json:"taproot_script_hash"` // Hash of the Taproot script
	CurrentLTV        float64         `json:"current_ltv"`         // Current LTV ratio
	NextPaymentDue    time.Time       `json:"next_payment_due"`    // When the next interest payment is due
	PaymentsMade      int             `json:"payments_made"`       // Number of interest payments made
	PaymentsMissed    int             `json:"payments_missed"`     // Number of missed payments
}

// Validate checks if the LoanMatch is valid
func (m *LoanMatch) Validate() error {
	if m.BorrowerID == "" {
		return errors.New("borrower ID cannot be empty")
	}
	if len(m.MatchedLenders) == 0 {
		return errors.New("must have at least one matched lender")
	}

	// Check that the sum of lender amounts equals the total
	sum := 0.0
	for _, lender := range m.MatchedLenders {
		if lender.LenderID == "" {
			return errors.New("lender ID cannot be empty")
		}
		if lender.USDTAmount <= 0 {
			return errors.New("lender USDT amount must be positive")
		}
		sum += lender.USDTAmount
	}

	if sum != m.TotalUSDTAmount {
		return errors.New("sum of lender amounts does not match total USDT amount")
	}

	if m.BTCCollateral < MinCollateralBTC {
		return fmt.Errorf("BTC collateral must be at least %f", MinCollateralBTC)
	}

	if m.DurationDays < MinLoanDurationDays || m.DurationDays > MaxLoanDurationDays {
		return fmt.Errorf("duration must be between %d and %d days", MinLoanDurationDays, MaxLoanDurationDays)
	}

	return nil
}

// CalculateTotalDailyInterest calculates the total daily interest across all lenders
func (m *LoanMatch) CalculateTotalDailyInterest() float64 {
	total := 0.0
	for _, lender := range m.MatchedLenders {
		interest := lender.USDTAmount * float64(lender.DailyInterestBPS) / 10000.0
		total += interest
	}
	return total
}

// PriceData represents price information from oracles
type PriceData struct {
	BTCPriceUSDT  float64   `json:"btc_price_usdt"` // Current BTC price in USDT
	Timestamp     time.Time `json:"timestamp"`      // When the price was recorded
	SourceOracles []string  `json:"source_oracles"` // List of oracle sources
}

// OrderBookEntry represents an entry in the decentralized order book
type OrderBookEntry struct {
	EntryType string      `json:"entry_type"` // "offer" or "request"
	EntryID   string      `json:"entry_id"`   // ID of the offer or request
	UserID    string      `json:"user_id"`    // ID of the user who created the entry
	CreatedAt time.Time   `json:"created_at"` // When the entry was created
	ExpiresAt time.Time   `json:"expires_at"` // When the entry expires
	Data      interface{} `json:"data"`       // The actual offer or request data
}

// Serialize converts the OrderBookEntry to JSON bytes
func (e *OrderBookEntry) Serialize() ([]byte, error) {
	return json.Marshal(e)
}

// Deserialize parses JSON bytes into an OrderBookEntry
func (e *OrderBookEntry) Deserialize(data []byte) error {
	return json.Unmarshal(data, e)
}

// ReputationUpdate represents a change to a user's reputation
type ReputationUpdate struct {
	UserID        string    `json:"user_id"`         // ID of the user
	OldScore      int       `json:"old_score"`       // Previous reputation score
	NewScore      int       `json:"new_score"`       // New reputation score
	Reason        string    `json:"reason"`          // Reason for the update
	RelatedLoanID string    `json:"related_loan_id"` // ID of the related loan, if any
	Timestamp     time.Time `json:"timestamp"`       // When the update occurred
}

// PaymentRecord represents a record of a payment (interest or principal)
type PaymentRecord struct {
	ID            string    `json:"id"`              // Unique payment ID
	LoanMatchID   string    `json:"loan_match_id"`   // ID of the loan match
	BorrowerID    string    `json:"borrower_id"`     // ID of the borrower
	LenderID      string    `json:"lender_id"`       // ID of the lender
	AmountUSDT    float64   `json:"amount_usdt"`     // Amount in USDT
	PaymentType   string    `json:"payment_type"`    // "interest" or "principal"
	PaymentDay    int       `json:"payment_day"`     // Day number of the loan term
	LightningTxID string    `json:"lightning_tx_id"` // Lightning transaction ID
	Status        string    `json:"status"`          // "success", "failed", "pending"
	Timestamp     time.Time `json:"timestamp"`       // When the payment was made
}

// TaprootContract represents the details of a Taproot smart contract
type TaprootContract struct {
	ScriptHash       string    `json:"script_hash"`       // Hash of the Taproot script
	BorrowerPubKey   string    `json:"borrower_pub_key"`  // Borrower's public key
	LenderPubKeys    []string  `json:"lender_pub_keys"`   // Lenders' public keys
	CollateralAmount float64   `json:"collateral_amount"` // Amount of BTC collateral
	LoanAmount       float64   `json:"loan_amount"`       // Amount of USDT loan
	LTVThreshold     float64   `json:"ltv_threshold"`     // LTV threshold for liquidation
	StartDate        time.Time `json:"start_date"`        // When the contract starts
	EndDate          time.Time `json:"end_date"`          // When the contract ends
	ContractAddress  string    `json:"contract_address"`  // Taproot address of the contract
	RedeemScript     string    `json:"redeem_script"`     // Redeem script (hex-encoded)
}

// LightningPayment represents a Lightning Network payment
type LightningPayment struct {
	SenderNodeID    string    `json:"sender_node_id"`   // ID of the sender's Lightning node
	ReceiverNodeID  string    `json:"receiver_node_id"` // ID of the receiver's Lightning node
	AmountUSDT      float64   `json:"amount_usdt"`      // Amount in USDT
	PaymentHash     string    `json:"payment_hash"`     // Payment hash
	PaymentPreimage string    `json:"payment_preimage"` // Payment preimage
	Status          string    `json:"status"`           // "success", "failed", "pending"
	Timestamp       time.Time `json:"timestamp"`        // When the payment was initiated
}

// P2PMessage represents a message in the P2P network
type P2PMessage struct {
	MessageType string          `json:"message_type"` // Type of message
	SenderID    string          `json:"sender_id"`    // ID of the sender
	Timestamp   time.Time       `json:"timestamp"`    // When the message was created
	Payload     json.RawMessage `json:"payload"`      // Message payload
	Signature   string          `json:"signature"`    // Digital signature
}

// Serialize converts the P2PMessage to JSON bytes
func (m *P2PMessage) Serialize() ([]byte, error) {
	return json.Marshal(m)
}

// Deserialize parses JSON bytes into a P2PMessage
func (m *P2PMessage) Deserialize(data []byte) error {
	return json.Unmarshal(data, m)
}

// VerifySignature verifies the message signature
func (m *P2PMessage) VerifySignature(pubKey string) (bool, error) {
	// In a real implementation, this would verify the signature using the public key
	// For MVP purposes, this is a placeholder
	return true, nil
}
