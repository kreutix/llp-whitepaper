package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kreutix/llp/pkg/matching"
	"github.com/kreutix/llp/pkg/orderbook"
	"github.com/kreutix/llp/pkg/p2p"
	"github.com/kreutix/llp/pkg/types"
	"github.com/multiformats/go-multiaddr"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	cfgFile        string
	logLevel       string
	listenAddrs    []string
	bootstrapPeers []string
	// btcPriceUSDT   float64 // Replaced by currentBTCPrice
)

var currentBTCPrice float64

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "llp",
	Short: "Lightning Loan Protocol - Bitcoin-backed USDT loans",
	Long: `Lightning Loan Protocol (LLP) is a decentralized, trustless platform 
that enables users to borrow USDT by locking Bitcoin (BTC) as collateral.

This CLI tool allows you to participate in the LLP network as a lender or borrower,
create and manage loan offers and requests, and view the state of the order book.`,
}

// Global application state
var (
	logger    *zap.Logger
	p2pNode   *p2p.P2PNode
	orderBook *orderbook.OrderBook
	matcher   *matching.Matcher
)

func main() {
	// Execute the root command
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// Global flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.llp.yaml)")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringSliceVar(&listenAddrs, "listen", []string{"/ip4/0.0.0.0/tcp/9000"}, "addresses to listen on")
	rootCmd.PersistentFlags().StringSliceVar(&bootstrapPeers, "bootstrap", []string{}, "bootstrap peers to connect to")
	// rootCmd.PersistentFlags().Float64Var(&btcPriceUSDT, "btc-price", 50000.0, "current BTC price in USDT") // Flag moved to startCmd

	// Add commands
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(offerCmd)
	rootCmd.AddCommand(requestCmd)
	rootCmd.AddCommand(matchCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(loanCmd) // Add new loan command
}

// initConfig reads in config file and ENV variables if set
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		// Search config in home directory with name ".llp" (without extension)
		viper.AddConfigPath(home)
		viper.SetConfigName(".llp")
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

// setupLogger initializes the logger with the specified log level
func setupLogger(level string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	switch strings.ToLower(level) {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	config := zap.Config{
		Level:            zap.NewAtomicLevelAt(zapLevel),
		Development:      false,
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	return config.Build()
}

// startCmd represents the start command
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the LLP node",
	Long:  `Start the Lightning Loan Protocol node and join the P2P network.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Initialize logger early for other PreRun functions
		var err error
		logger, err = setupLogger(logLevel)
		if err != nil {
			return fmt.Errorf("failed to setup logger: %w", err)
		}
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		initialBTCPrice, _ := cmd.Flags().GetFloat64("btc-price")
		currentBTCPrice = initialBTCPrice

		// Setup context with cancellation
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Setup signal handling for graceful shutdown
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Println("\nShutting down...")
			cancel()
		}()

		// Initialize logger
		// Logger is initialized in PersistentPreRunE
		defer logger.Sync()

		// Parse listen addresses
		maddrs := make([]multiaddr.Multiaddr, 0, len(listenAddrs))
		for _, addrStr := range listenAddrs {
			addr, err := multiaddr.NewMultiaddr(addrStr)
			if err != nil {
				logger.Fatal("Invalid listen address", zap.String("addr", addrStr), zap.Error(err))
			}
			maddrs = append(maddrs, addr)
		}

		// Initialize matcher first, as it's needed by P2PNode for reputation updates
		// Order book is initialized later as it needs p2pNode
		matcher = matching.NewMatcher(nil, matching.DefaultMatchingConfig(), logger) // OrderBook is set later

		// Create P2P node, passing the matcher for reputation updates
		p2pNode, err = p2p.NewP2PNode(ctx, maddrs, logger, matcher)
		if err != nil {
			logger.Fatal("Failed to create P2P node", zap.Error(err))
		}

		// Initialize order book
		orderBook = orderbook.NewOrderBook(ctx, p2pNode, logger)
		// Now that orderBook is initialized, set it in the matcher
		matcher.SetOrderBook(orderBook) // This method needs to be added to the Matcher

		// Parse bootstrap peers
		bootstrapMaddrs := make([]multiaddr.Multiaddr, 0, len(bootstrapPeers))
		for _, peerStr := range bootstrapPeers {
			addr, err := multiaddr.NewMultiaddr(peerStr)
			if err != nil {
				logger.Fatal("Invalid bootstrap peer address", zap.String("addr", peerStr), zap.Error(err))
			}
			bootstrapMaddrs = append(bootstrapMaddrs, addr)
		}

		// Start P2P node
		if err := p2pNode.Start(bootstrapMaddrs); err != nil {
			logger.Fatal("Failed to start P2P node", zap.Error(err))
		}
		defer p2pNode.Stop()

		// Start Oracle Service
		go startOracleService(ctx, p2pNode, currentBTCPrice)

		// Initialize matcher (already done above, orderBook is set now)

		// Start Loan Processing Goroutine
		go processLoans(ctx, matcher, p2pNode, logger)

		logger.Info("LLP node started",
			zap.String("nodeID", p2pNode.GetNodeID()),
			zap.Strings("listenAddrs", listenAddrs),
			zap.Int("bootstrapPeers", len(bootstrapMaddrs)))

		// Print node information
		fmt.Printf("Node ID: %s\n", p2pNode.GetNodeID())
		fmt.Printf("Listening on: %v\n", listenAddrs)
		fmt.Printf("Bootstrap peers: %v\n", bootstrapPeers)
		fmt.Printf("Initial BTC price: %.2f USDT\n", currentBTCPrice) // Changed btcPriceUSDT to currentBTCPrice

		// Wait for context cancellation (Ctrl+C)
		<-ctx.Done()
		fmt.Println("LLP node stopped")
	},
}

// offerCmd represents the offer command
var offerCmd = &cobra.Command{
	Use:   "offer",
	Short: "Manage loan offers",
	Long:  `Create, list, and remove loan offers.`,
}

// offerCreateCmd represents the offer create command
var offerCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new loan offer",
	Long:  `Create a new loan offer as a lender.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Get offer parameters from flags
		lenderID, _ := cmd.Flags().GetString("lender-id")
		usdtAmount, _ := cmd.Flags().GetFloat64("usdt-amount")
		durationDays, _ := cmd.Flags().GetInt("duration-days")
		dailyInterestBPS, _ := cmd.Flags().GetInt("daily-interest-bps")
		minLTVRatio, _ := cmd.Flags().GetFloat64("min-ltv-ratio")
		lightningNodeID, _ := cmd.Flags().GetString("lightning-node-id")

		// Validate parameters
		if lenderID == "" {
			fmt.Println("Error: lender-id is required")
			return
		}
		if usdtAmount <= 0 {
			fmt.Println("Error: usdt-amount must be positive")
			return
		}
		if durationDays <= 0 {
			fmt.Println("Error: duration-days must be positive")
			return
		}
		if dailyInterestBPS <= 0 {
			fmt.Println("Error: daily-interest-bps must be positive")
			return
		}
		if minLTVRatio <= 0 || minLTVRatio >= 1 {
			fmt.Println("Error: min-ltv-ratio must be between 0 and 1")
			return
		}
		if lightningNodeID == "" {
			fmt.Println("Error: lightning-node-id is required")
			return
		}

		// Create offer
		offer := &types.LoanOffer{
			LenderID:         lenderID,
			USDTAmount:       usdtAmount,
			DurationDays:     durationDays,
			DailyInterestBPS: dailyInterestBPS,
			MinLTVRatio:      minLTVRatio,
			CreatedAt:        time.Now(),
			ExpiresAt:        time.Now().AddDate(0, 0, 1), // 1 day expiry by default
			Status:           types.StatusPending,
			LightningNodeID:  lightningNodeID,
		}

		// Add to order book
		if err := orderBook.AddOffer(offer); err != nil {
			fmt.Printf("Error creating offer: %v\n", err)
			return
		}

		fmt.Printf("Offer created successfully with ID: %s\n", offer.ID)
		fmt.Printf("Amount: %.2f USDT\n", offer.USDTAmount)
		fmt.Printf("Duration: %d days\n", offer.DurationDays)
		fmt.Printf("Daily interest: %d basis points (%.4f%%)\n",
			offer.DailyInterestBPS, float64(offer.DailyInterestBPS)/100)
		fmt.Printf("Min LTV ratio: %.2f\n", offer.MinLTVRatio)
		fmt.Printf("Daily interest amount: %.4f USDT\n", offer.CalculateDailyInterest())
	},
}

// offerListCmd represents the offer list command
var offerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List loan offers",
	Long:  `List all loan offers in the order book.`,
	Run: func(cmd *cobra.Command, args []string) {
		lenderID, _ := cmd.Flags().GetString("lender-id")

		var offers []*types.LoanOffer
		if lenderID != "" {
			// List offers for a specific lender
			offers = orderBook.GetOffersByUser(lenderID)
			fmt.Printf("Loan offers for lender %s:\n", lenderID)
		} else {
			// List all offers
			offers = orderBook.GetAllOffers()
			fmt.Printf("All loan offers (%d):\n", len(offers))
		}

		if len(offers) == 0 {
			fmt.Println("No offers found")
			return
		}

		// Print offers
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("%-36s %-10s %-8s %-7s %-6s\n",
			"ID", "AMOUNT", "DURATION", "INTEREST", "MIN LTV")
		fmt.Println("------------------------------------------------------------")

		for _, offer := range offers {
			fmt.Printf("%-36s %-10.2f %-8d %-7d %-6.2f\n",
				offer.ID, offer.USDTAmount, offer.DurationDays,
				offer.DailyInterestBPS, offer.MinLTVRatio)
		}
		fmt.Println("------------------------------------------------------------")
	},
}

// offerRemoveCmd represents the offer remove command
var offerRemoveCmd = &cobra.Command{
	Use:   "remove [offer-id]",
	Short: "Remove a loan offer",
	Long:  `Remove a loan offer from the order book.`,
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		offerID := args[0]

		// Remove from order book
		if err := orderBook.RemoveOffer(offerID); err != nil {
			fmt.Printf("Error removing offer: %v\n", err)
			return
		}

		fmt.Printf("Offer %s removed successfully\n", offerID)
	},
}

// requestCmd represents the request command
var requestCmd = &cobra.Command{
	Use:   "request",
	Short: "Manage loan requests",
	Long:  `Create, list, and remove loan requests.`,
}

// requestCreateCmd represents the request create command
var requestCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new loan request",
	Long:  `Create a new loan request as a borrower.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Get request parameters from flags
		borrowerID, _ := cmd.Flags().GetString("borrower-id")
		usdtAmount, _ := cmd.Flags().GetFloat64("usdt-amount")
		btcCollateral, _ := cmd.Flags().GetFloat64("btc-collateral")
		maxDailyInterestBPS, _ := cmd.Flags().GetInt("max-daily-interest-bps")
		durationDays, _ := cmd.Flags().GetInt("duration-days")
		taprootAddress, _ := cmd.Flags().GetString("taproot-address")
		lightningNodeID, _ := cmd.Flags().GetString("lightning-node-id")

		// Validate parameters
		if borrowerID == "" {
			fmt.Println("Error: borrower-id is required")
			return
		}
		if usdtAmount <= 0 {
			fmt.Println("Error: usdt-amount must be positive")
			return
		}
		if btcCollateral <= 0 {
			fmt.Println("Error: btc-collateral must be positive")
			return
		}
		if maxDailyInterestBPS <= 0 {
			fmt.Println("Error: max-daily-interest-bps must be positive")
			return
		}
		if durationDays <= 0 {
			fmt.Println("Error: duration-days must be positive")
			return
		}
		if taprootAddress == "" {
			fmt.Println("Error: taproot-address is required")
			return
		}
		if lightningNodeID == "" {
			fmt.Println("Error: lightning-node-id is required")
			return
		}

		// Calculate current LTV
		var priceForLTV float64
		if p2pNode != nil {
			priceForLTV = p2pNode.GetLatestBTCPrice()
			if priceForLTV <= 0 { // Fallback if P2P node price is not available/valid
				logger.Warn("P2P node returned invalid price, falling back to initial price for LTV check in requestCreateCmd", zap.Float64("p2pPrice", priceForLTV))
				priceForLTV = currentBTCPrice // currentBTCPrice is the initial price from flags
			}
		} else {
			// Fallback for commands run without starting the node (e.g. if 'create' was a standalone command)
			// This shouldn't happen if 'start' is always run first, but as a safeguard.
			logger.Warn("p2pNode is nil in requestCreateCmd, using initial price for LTV check.")
			priceForLTV = currentBTCPrice
		}

		ltv := usdtAmount / (btcCollateral * priceForLTV)
		if ltv >= types.LiquidationLTVRatio {
			fmt.Printf("Error: LTV ratio (%.2f) is too high (max %.2f) with current BTC price %.2f USDT.\n",
				ltv, types.LiquidationLTVRatio, priceForLTV)
			return
		}

		// Create request
		request := &types.LoanRequest{
			BorrowerID:          borrowerID,
			USDTAmount:          usdtAmount,
			BTCCollateral:       btcCollateral,
			MaxDailyInterestBPS: maxDailyInterestBPS,
			DurationDays:        durationDays,
			CreatedAt:           time.Now(),
			ExpiresAt:           time.Now().AddDate(0, 0, 1), // 1 day expiry by default
			Status:              types.StatusPending,
			TaprootAddress:      taprootAddress,
			LightningNodeID:     lightningNodeID,
		}

		// Add to order book
		if err := orderBook.AddRequest(request); err != nil {
			fmt.Printf("Error creating request: %v\n", err)
			return
		}

		fmt.Printf("Request created successfully with ID: %s\n", request.ID)
		fmt.Printf("Amount: %.2f USDT\n", request.USDTAmount)
		fmt.Printf("Collateral: %.8f BTC\n", request.BTCCollateral)
		fmt.Printf("Current LTV: %.2f\n", ltv)
		fmt.Printf("Max daily interest: %d basis points (%.4f%%)\n",
			request.MaxDailyInterestBPS, float64(request.MaxDailyInterestBPS)/100)
		fmt.Printf("Duration: %d days\n", request.DurationDays)
	},
}

// requestListCmd represents the request list command
var requestListCmd = &cobra.Command{
	Use:   "list",
	Short: "List loan requests",
	Long:  `List all loan requests in the order book.`,
	Run: func(cmd *cobra.Command, args []string) {
		borrowerID, _ := cmd.Flags().GetString("borrower-id")

		var requests []*types.LoanRequest
		if borrowerID != "" {
			// List requests for a specific borrower
			requests = orderBook.GetRequestsByUser(borrowerID)
			fmt.Printf("Loan requests for borrower %s:\n", borrowerID)
		} else {
			// List all requests
			requests = orderBook.GetAllRequests()
			fmt.Printf("All loan requests (%d):\n", len(requests))
		}

		if len(requests) == 0 {
			fmt.Println("No requests found")
			return
		}

		// Print requests
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("%-36s %-10s %-12s %-7s %-8s\n",
			"ID", "AMOUNT", "COLLATERAL", "MAX INT", "DURATION")
		fmt.Println("------------------------------------------------------------")

		for _, request := range requests {
			var priceForLTV float64
			if p2pNode != nil {
				priceForLTV = p2pNode.GetLatestBTCPrice()
				if priceForLTV <= 0 {
					priceForLTV = currentBTCPrice // Fallback
					logger.Debug("requestListCmd: P2P node price invalid, using fallback for LTV.", zap.String("requestID", request.ID))
				}
			} else {
				priceForLTV = currentBTCPrice // Fallback
				logger.Debug("requestListCmd: p2pNode is nil, using fallback for LTV.", zap.String("requestID", request.ID))
			}
			ltv := request.CalculateLTV(priceForLTV)
			fmt.Printf("%-36s %-10.2f %-12.8f %-7d %-8d\n",
				request.ID, request.USDTAmount, request.BTCCollateral,
				request.MaxDailyInterestBPS, request.DurationDays)
			fmt.Printf("  LTV: %.2f, Status: %s\n", ltv, request.Status)
		}
		fmt.Println("------------------------------------------------------------")
	},
}

// requestRemoveCmd represents the request remove command
var requestRemoveCmd = &cobra.Command{
	Use:   "remove [request-id]",
	Short: "Remove a loan request",
	Long:  `Remove a loan request from the order book.`,
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		requestID := args[0]

		// Remove from order book
		if err := orderBook.RemoveRequest(requestID); err != nil {
			fmt.Printf("Error removing request: %v\n", err)
			return
		}

		fmt.Printf("Request %s removed successfully\n", requestID)
	},
}

// matchCmd represents the match command
var matchCmd = &cobra.Command{
	Use:   "match",
	Short: "Match loan requests with offers",
	Long:  `Match loan requests with offers to create loan agreements.`,
}

// matchFindCmd represents the match find command
var matchFindCmd = &cobra.Command{
	Use:   "find [request-id]",
	Short: "Find matches for a loan request",
	Long:  `Find potential matches for a loan request without finalizing the loan.`,
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		requestID := args[0]

		// Get the request
		request, err := orderBook.GetRequest(requestID)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}

		// Find matching offers
		// TODO: Pass p2pNode to GetMatchingOffers
		matchingOffers := orderBook.GetMatchingOffers(request, p2pNode) // Pass p2pNode
		if len(matchingOffers) == 0 {
			fmt.Println("No matching offers found")
			return
		}

		fmt.Printf("Found %d matching offers for request %s:\n", len(matchingOffers), requestID)
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("%-36s %-10s %-8s %-7s %-6s\n",
			"OFFER ID", "AMOUNT", "DURATION", "INTEREST", "MIN LTV")
		fmt.Println("------------------------------------------------------------")

		for _, offer := range matchingOffers {
			fmt.Printf("%-36s %-10.2f %-8d %-7d %-6.2f\n",
				offer.ID, offer.USDTAmount, offer.DurationDays,
				offer.DailyInterestBPS, offer.MinLTVRatio)
		}
		fmt.Println("------------------------------------------------------------")
	},
}

// matchCreateCmd represents the match create command
var matchCreateCmd = &cobra.Command{
	Use:   "create [request-id]",
	Short: "Create a loan match",
	Long:  `Create a loan match by matching a request with available offers.`,
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		requestID := args[0]

		// Create the match
		// TODO: Pass p2pNode to MatchRequest
		match, err := matcher.MatchRequest(requestID, p2pNode) // Pass p2pNode
		if err != nil {
			fmt.Printf("Error creating match: %v\n", err)
			return
		}

		// Print match details
		fmt.Printf("Match created successfully with ID: %s\n", match.ID)
		fmt.Printf("Borrower: %s\n", match.BorrowerID)
		fmt.Printf("Total amount: %.2f USDT\n", match.TotalUSDTAmount)
		fmt.Printf("Collateral: %.8f BTC\n", match.BTCCollateral)
		fmt.Printf("Current LTV: %.2f\n", match.CurrentLTV)
		fmt.Printf("Duration: %d days (from %s to %s)\n",
			match.DurationDays,
			match.StartDate.Format("2006-01-02"),
			match.EndDate.Format("2006-01-02"))

		fmt.Printf("Matched with %d lenders:\n", len(match.MatchedLenders))
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("%-36s %-10s %-7s %-12s\n",
			"LENDER ID", "AMOUNT", "INTEREST", "DAILY PAYMENT")
		fmt.Println("------------------------------------------------------------")

		totalDailyInterest := 0.0
		for _, lender := range match.MatchedLenders {
			dailyInterest := lender.USDTAmount * float64(lender.DailyInterestBPS) / 10000.0
			totalDailyInterest += dailyInterest

			fmt.Printf("%-36s %-10.2f %-7d %-12.4f\n",
				lender.LenderID, lender.USDTAmount, lender.DailyInterestBPS, dailyInterest)
		}
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("Total daily interest: %.4f USDT\n", totalDailyInterest)
		fmt.Printf("Effective daily interest rate: %.4f%% (%d basis points)\n",
			totalDailyInterest/match.TotalUSDTAmount*100,
			int((totalDailyInterest/match.TotalUSDTAmount)*10000))

		// Ask for confirmation
		fmt.Print("\nDo you want to finalize this match? (y/n): ")
		var response string
		fmt.Scanln(&response)

		if strings.ToLower(response) == "y" || strings.ToLower(response) == "yes" {
			if err := matcher.FinalizeLoanMatch(match); err != nil {
				fmt.Printf("Error finalizing match: %v\n", err)
				return
			}
			fmt.Println("Match finalized successfully")
		} else {
			fmt.Println("Match not finalized")
		}
	},
}

// statusCmd represents the status command
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show node status",
	Long:  `Show the status of the LLP node and order book.`,
	Run: func(cmd *cobra.Command, args []string) {
		if p2pNode == nil || orderBook == nil || matcher == nil {
			fmt.Println("Error: Node components not initialized. Please start the node using 'start' command.")
			return
		}

		// Get order book stats
		stats := orderBook.GetStats()

		fmt.Println("Lightning Loan Protocol Node Status")
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("Node ID: %s\n", p2pNode.GetNodeID())
		fmt.Printf("Connected peers: %d\n", stats["peer_count"])

		var displayPrice float64
		if p2pNode != nil {
			displayPrice = p2pNode.GetLatestBTCPrice()
			if displayPrice <= 0 {
				logger.Warn("statusCmd: P2P node returned invalid/zero price, falling back to initial price for display.", zap.Float64("p2pPrice", displayPrice))
				displayPrice = currentBTCPrice
			}
		} else {
			displayPrice = currentBTCPrice // Should not happen if check above passes
		}
		fmt.Printf("Current BTC price: %.2f USDT\n", displayPrice)
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("Order Book Statistics:\n")
		fmt.Printf("  Offers: %d\n", stats["offer_count"])
		fmt.Printf("  Requests: %d\n", stats["request_count"])
		fmt.Printf("  Lenders: %d\n", stats["lender_count"])
		fmt.Printf("  Borrowers: %d\n", stats["borrower_count"])
		fmt.Printf("  Total USDT offered: %.2f\n", stats["total_usdt_offered"])
		fmt.Printf("  Total BTC requested as collateral: %.8f\n", stats["total_btc_requested"])
		fmt.Println("------------------------------------------------------------")

		activeMatches := matcher.GetAllActiveMatches()
		if len(activeMatches) > 0 {
			fmt.Println("Active Loans:")
			fmt.Println("-------------------------------------------------------------------------------------------------------------------------------------")
			fmt.Printf("%-36s | %-15s | %-10s | %-12s | %-7s | %-10s | %-10s | %-10s | %-10s | %-4s | %-4s\n",
				"MATCH ID", "BORROWER ID", "USDT AMT", "BTC COLL", "LTV", "STATUS", "START", "END", "NEXT DUE", "PAID", "MISSED")
			fmt.Println("-------------------------------------------------------------------------------------------------------------------------------------")
			for _, match := range activeMatches {
				fmt.Printf("%-36s | %-15s | %-10.2f | %-12.8f | %-6.2f%% | %-10s | %-10s | %-10s | %-10s | %-4d | %-4d\n",
					match.ID,
					match.BorrowerID,
					match.TotalUSDTAmount,
					match.BTCCollateral,
					match.CurrentLTV*100,
					match.Status,
					match.StartDate.Format("2006-01-02"),
					match.EndDate.Format("2006-01-02"),
					match.NextPaymentDue.Format("2006-01-02"),
					match.PaymentsMade,
					match.PaymentsMissed,
				)
			}
			fmt.Println("-------------------------------------------------------------------------------------------------------------------------------------")
		} else {
			fmt.Println("No active loans.")
			fmt.Println("------------------------------------------------------------")
		}
	},
}

// setBTCPriceCmd represents the set-btc-price command
var setBTCPriceCmd = &cobra.Command{
	Use:   "set-btc-price [price]",
	Short: "Set the current BTC price",
	Long:  `Set the current BTC price in USDT for LTV calculations.`,
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		// This command is being removed.
		fmt.Println("Error: The 'set-btc-price' command has been removed. Price is now managed by the oracle service.")
		// price, err := strconv.ParseFloat(args[0], 64)
		// if err != nil {
		// 	fmt.Printf("Error: invalid price format: %v\n", err)
		// 	return
		// }

		// if price <= 0 {
		// 	fmt.Println("Error: price must be positive")
		// 	return
		// }

		// currentBTCPrice = price // Update global, though oracle will overwrite
		// if p2pNode != nil {
		// p2pNode.UpdateLatestBTCPrice(price) // Also update the node's price if it's running
		// }
		// fmt.Printf("BTC price updated to %.2f USDT. The oracle service will manage this price dynamically.\n", price)
	},
}

// startOracleService simulates oracle price updates
func startOracleService(ctx context.Context, p2pNode *p2p.P2PNode, initialPrice float64) {
	ticker := time.NewTicker(30 * time.Second) // Configurable later
	defer ticker.Stop()

	currentBTCPrice = initialPrice // Initialize with the initial price

	logger.Info("Oracle service started", zap.Float64("initialBTCPrice", currentBTCPrice))

	for {
		select {
		case <-ctx.Done():
			logger.Info("Oracle service stopping")
			return
		case <-ticker.C:
			// Simulate price change (+/- 0.1% to 1%)
			changePercent := (0.1 + (1.0-0.1)*time.Now().UnixNano()%900/1000.0) / 100.0 // pseudo-random between 0.001 and 0.01
			if time.Now().UnixNano()%2 == 0 {
				currentBTCPrice *= (1 + changePercent)
			} else {
				currentBTCPrice *= (1 - changePercent)
			}

			logger.Info("Oracle updated BTC price", zap.Float64("newPrice", currentBTCPrice))

			priceData := types.PriceData{
				BTCPriceUSDT: currentBTCPrice,
				Timestamp:    time.Now(),
				Source:       "MockOracle",
			}

			// Publish price data
			if p2pNode != nil {
				if p2pNode.GetHost() != nil { // Check if host is initialized
					err := p2pNode.PublishPriceUpdate(priceData)
					if err != nil {
						logger.Error("Failed to publish price update via P2P", zap.Error(err))
					} else {
						// Also update the node's internally tracked price directly for immediate consistency
						// This is because the published price might take time to propagate back to this node
						// or if this node is the oracle, it should have the latest price.
						p2pNode.UpdateLatestBTCPrice(currentBTCPrice)
					}
				} else {
					logger.Warn("Oracle service: p2pNode host is not initialized, skipping price publish")
				}
			} else {
				logger.Warn("Oracle service: p2pNode is nil, skipping price publish")
			}
		}
	}
}

func init() {
	// Ensure p2pNode is initialized before commands that need it are run.
	// Cobra PersistentPreRunE can be used on parent commands if subcommands need shared setup.
	// For commands like 'request create', 'match find', 'match create', 'status', 'request list',
	// they ideally need the p2pNode.
	// The 'start' command initializes p2pNode. Other commands are meant to be run *after* 'start'
	// or in a context where the node is running.
	// The current structure implies these commands are run via the CLI against a running node process,
	// but the code executes them within the same process. This might need refactoring in a real-world scenario
	// to separate client CLI commands from the server/node daemon.
	// For now, we assume p2pNode will be non-nil if 'start' has been successfully executed.

	startCmd.Flags().Float64("btc-price", 50000.0, "initial BTC price in USDT for the oracle service")
	// Add subcommands to offer command
	offerCmd.AddCommand(offerCreateCmd)
	offerCmd.AddCommand(offerListCmd)
	offerCmd.AddCommand(offerRemoveCmd)

	// Add flags to offer create command
	offerCreateCmd.Flags().String("lender-id", "", "ID of the lender")
	offerCreateCmd.Flags().Float64("usdt-amount", 0, "Amount of USDT to offer")
	offerCreateCmd.Flags().Int("duration-days", 30, "Loan duration in days")
	offerCreateCmd.Flags().Int("daily-interest-bps", 10, "Daily interest rate in basis points (1 BPS = 0.01%)")
	offerCreateCmd.Flags().Float64("min-ltv-ratio", 0.5, "Minimum loan-to-value ratio required")
	offerCreateCmd.Flags().String("lightning-node-id", "", "Lightning Network node ID for payments")

	// Add flags to offer list command
	offerListCmd.Flags().String("lender-id", "", "Filter offers by lender ID")

	// Add subcommands to request command
	requestCmd.AddCommand(requestCreateCmd)
	requestCmd.AddCommand(requestListCmd)
	requestCmd.AddCommand(requestRemoveCmd)

	// Add flags to request create command
	requestCreateCmd.Flags().String("borrower-id", "", "ID of the borrower")
	requestCreateCmd.Flags().Float64("usdt-amount", 0, "Amount of USDT requested")
	requestCreateCmd.Flags().Float64("btc-collateral", 0, "Amount of BTC offered as collateral")
	requestCreateCmd.Flags().Int("max-daily-interest-bps", 20, "Maximum daily interest rate in basis points")
	requestCreateCmd.Flags().Int("duration-days", 30, "Requested loan duration in days")
	requestCreateCmd.Flags().String("taproot-address", "", "Taproot address for collateral")
	requestCreateCmd.Flags().String("lightning-node-id", "", "Lightning Network node ID for payments")

	// Add flags to request list command
	requestListCmd.Flags().String("borrower-id", "", "Filter requests by borrower ID")

	// Add subcommands to match command
	matchCmd.AddCommand(matchFindCmd)
	matchCmd.AddCommand(matchCreateCmd)

	// Add reputation command and its subcommands
	rootCmd.AddCommand(reputationCmd)
	reputationCmd.AddCommand(reputationGetCmd)
	reputationCmd.AddCommand(reputationSetCmd)

	// Add loan command and its subcommands
	loanCmd.AddCommand(loanInfoCmd)
	loanCmd.AddCommand(loanMarkRepaidCmd)
	loanCmd.AddCommand(loanMarkDefaultedCmd)
	// rootCmd.AddCommand(loanCmd) // loanCmd is added to rootCmd in init() where it's declared

	// Add set-btc-price command to root command
	// rootCmd.AddCommand(setBTCPriceCmd) // Removing this command
}

// loanCmd represents the base command for loan-specific actions.
var loanCmd = &cobra.Command{
	Use:   "loan",
	Short: "Manage and view loan details",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if logger == nil {
			var err error
			logger, err = setupLogger(logLevel)
			if err != nil {
				return fmt.Errorf("failed to setup logger for loan command: %w", err)
			}
		}
		if matcher == nil || p2pNode == nil { // p2pNode needed for UpdateReputationAndFinalizeMatch
			logger.Warn("Matcher or P2PNode not initialized. Loan commands may not function correctly unless 'start' is active.")
		}
		return nil
	},
}

// loanInfoCmd allows viewing details of a specific loan.
var loanInfoCmd = &cobra.Command{
	Use:   "info [match-id]",
	Short: "Get detailed information about a specific loan match",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		matchID := args[0]
		if matcher == nil {
			fmt.Println("Error: Matcher not initialized. Please start the node first.")
			return
		}

		match, found := matcher.GetActiveMatch(matchID)
		if !found {
			// TODO: Extend to check historical/finalized matches if/when Matcher supports it.
			fmt.Printf("Error: Active loan match with ID '%s' not found.\n", matchID)
			return
		}

		fmt.Printf("Loan Match Details (ID: %s):\n", match.ID)
		fmt.Printf("  Request ID: %s\n", match.RequestID)
		fmt.Printf("  Borrower ID: %s\n", match.BorrowerID)
		fmt.Printf("  Total USDT Amount: %.2f\n", match.TotalUSDTAmount)
		fmt.Printf("  BTC Collateral: %.8f\n", match.BTCCollateral)
		fmt.Printf("  Duration: %d days\n", match.DurationDays)
		fmt.Printf("  Start Date: %s\n", match.StartDate.Format("2006-01-02 15:04:05"))
		fmt.Printf("  End Date: %s\n", match.EndDate.Format("2006-01-02 15:04:05"))
		fmt.Printf("  Status: %s\n", match.Status)
		fmt.Printf("  Current LTV: %.2f%%\n", match.CurrentLTV*100)
		fmt.Printf("  Next Payment Due: %s\n", match.NextPaymentDue.Format("2006-01-02"))
		fmt.Printf("  Payments Made: %d\n", match.PaymentsMade)
		fmt.Printf("  Payments Missed: %d\n", match.PaymentsMissed)
		fmt.Printf("  Repaid Successfully: %t\n", match.RepaidSuccessfully)
		fmt.Printf("  Defaulted: %t\n", match.Defaulted)
		if !match.LastPaymentAttemptDate.IsZero() {
			fmt.Printf("  Last Payment Attempt: %s\n", match.LastPaymentAttemptDate.Format("2006-01-02 15:04:05"))
		}
		fmt.Println("  Matched Lenders:")
		for i, lender := range match.MatchedLenders {
			fmt.Printf("    Lender %d:\n", i+1)
			fmt.Printf("      ID: %s\n", lender.LenderID)
			fmt.Printf("      Offer ID: %s\n", lender.OfferID)
			fmt.Printf("      USDT Amount: %.2f\n", lender.USDTAmount)
			fmt.Printf("      Daily Interest (BPS): %d\n", lender.DailyInterestBPS)
			fmt.Printf("      Lightning Node ID: %s\n", lender.LightningNodeID)
		}
		fmt.Println("------------------------------------------------------------")
	},
}

// loanMarkRepaidCmd allows manually marking a loan as repaid for testing.
var loanMarkRepaidCmd = &cobra.Command{
	Use:   "mark-repaid [match-id]",
	Short: "Manually mark a loan as successfully repaid (for testing)",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		matchID := args[0]
		if matcher == nil || p2pNode == nil {
			fmt.Println("Error: Matcher or P2PNode not initialized. Please start the node first.")
			return
		}
		err := matcher.UpdateReputationAndFinalizeMatch(matchID, true, types.StatusCompleted, p2pNode)
		if err != nil {
			fmt.Printf("Error marking loan as repaid: %v\n", err)
			return
		}
		fmt.Printf("Loan match '%s' marked as repaid successfully.\n", matchID)
	},
}

// loanMarkDefaultedCmd allows manually marking a loan as defaulted for testing.
var loanMarkDefaultedCmd = &cobra.Command{
	Use:   "mark-defaulted [match-id]",
	Short: "Manually mark a loan as defaulted (for testing)",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		matchID := args[0]
		if matcher == nil || p2pNode == nil {
			fmt.Println("Error: Matcher or P2PNode not initialized. Please start the node first.")
			return
		}
		err := matcher.UpdateReputationAndFinalizeMatch(matchID, false, types.StatusDefaulted, p2pNode)
		if err != nil {
			fmt.Printf("Error marking loan as defaulted: %v\n", err)
			return
		}
		fmt.Printf("Loan match '%s' marked as defaulted.\n", matchID)
	},
}


const (
	defaultLoanProcessingInterval = 1 * time.Minute
	defaultMissedPaymentsThreshold = 3
)

// processLoans handles the lifecycle of active loans, such as daily payments and completion.
func processLoans(ctx context.Context, m *matching.Matcher, p2pNode *p2p.P2PNode, appLogger *zap.Logger) {
	// Use a specific logger for this goroutine for clarity, could also pass appLogger directly
	loanLogger := appLogger.Named("loan_processor")
	loanLogger.Info("Loan processing service started")

	// TODO: Make ticker interval configurable via startCmd flags
	ticker := time.NewTicker(defaultLoanProcessingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			loanLogger.Info("Loan processing service stopping")
			return
		case <-ticker.C:
			loanLogger.Debug("Processing active loans...")
			activeMatches := m.GetAllActiveMatches()
			if len(activeMatches) == 0 {
				loanLogger.Debug("No active loans to process.")
				continue
			}

			loanLogger.Info("Processing active loans", zap.Int("count", len(activeMatches)))

			now := time.Now()
			for _, match := range activeMatches {
				if match.Status != types.StatusActive {
					loanLogger.Debug("Skipping non-active loan", zap.String("matchID", match.ID), zap.String("status", string(match.Status)))
					continue
				}

				// LTV Check Logic (before payment logic for the day)
				currentPrice := p2pNode.GetLatestBTCPrice()
				if currentPrice <= 0 {
					loanLogger.Warn("Invalid BTC price from P2P node, skipping LTV check for this cycle", zap.Float64("price", currentPrice), zap.String("matchID", match.ID))
				} else {
					collateralValueUSDT := match.BTCCollateral * currentPrice
					newLTV := 0.0
					if collateralValueUSDT > 0 {
						newLTV = match.TotalUSDTAmount / collateralValueUSDT
					} else {
						// This case implies BTC price is positive but collateral is zero, or BTC price is zero.
						// If BTC price is positive and collateral is zero, LTV is effectively infinite or undefined.
						// If BTC price is zero (already handled by currentPrice <=0), this path isn't taken.
						loanLogger.Warn("Collateral value is zero, LTV is effectively infinite", zap.String("matchID", match.ID), zap.Float64("collateralBTC", match.BTCCollateral))
						// Consider extremely high LTV for liquidation if collateral is missing with active loan.
						// For now, if collateralValueUSDT is 0 and TotalUSDTAmount > 0, it's a guaranteed liquidation.
						if match.TotalUSDTAmount > 0 {
							newLTV = 999 // Effectively infinite LTV for liquidation check
						}
					}

					oldLTV := match.CurrentLTV
					match.CurrentLTV = newLTV
					if oldLTV != newLTV { // Log only if LTV changed
						loanLogger.Info("LTV updated", zap.String("matchID", match.ID), zap.Float64("oldLTV", oldLTV), zap.Float64("newLTV", newLTV), zap.Float64("btcPrice", currentPrice))
					}

					if newLTV >= types.LiquidationLTVRatio {
						loanLogger.Warn("Loan LTV above threshold, liquidating",
							zap.String("matchID", match.ID),
							zap.Float64("ltv", newLTV),
							zap.Float64("threshold", types.LiquidationLTVRatio))
						match.Status = types.StatusLiquidated // Set status before calling finalize
						// The UpdateReputationAndFinalizeMatch function will use the status from the match object if it's already terminal.
						// Or, we pass types.StatusLiquidated directly as the third argument.
						err := m.UpdateReputationAndFinalizeMatch(match.ID, false, types.StatusLiquidated, p2pNode)
						if err != nil {
							loanLogger.Error("Error finalizing liquidated match", zap.String("matchID", match.ID), zap.Error(err))
						}
						continue // Move to next match as this one is finalized
					}
				}

				// Daily Payment Logic - only if still active after LTV check
				if match.Status == types.StatusActive && now.After(match.NextPaymentDue) &&
					(match.LastPaymentAttemptDate.IsZero() || now.YearDay() != match.LastPaymentAttemptDate.YearDay() || now.Year() != match.LastPaymentAttemptDate.Year()) {
					loanLogger.Info("Payment due", zap.String("matchID", match.ID), zap.Time("nextPaymentDue", match.NextPaymentDue))
					match.LastPaymentAttemptDate = now

					// Simulate payment attempt (e.g., 90% success rate)
					// In a real system, this would involve interacting with a payment processor or Lightning node.
					paymentSuccessful := true // Placeholder, replace with rand.Float32() > 0.1 for simulation
					if time.Now().UnixNano()%10 == 0 { // Simulate 10% failure rate more deterministically for now
						paymentSuccessful = false
					}


					if paymentSuccessful {
						match.PaymentsMade++
						match.NextPaymentDue = match.NextPaymentDue.AddDate(0, 0, 1) // Advance by one day
						loanLogger.Info("Payment successful",
							zap.String("matchID", match.ID),
							zap.Int("paymentsMade", match.PaymentsMade),
							zap.Time("newNextPaymentDue", match.NextPaymentDue))
					} else {
						match.PaymentsMissed++
						loanLogger.Warn("Payment failed",
							zap.String("matchID", match.ID),
							zap.Int("paymentsMissed", match.PaymentsMissed))

						// TODO: Make missedPaymentsThreshold configurable
						if match.PaymentsMissed >= defaultMissedPaymentsThreshold {
							loanLogger.Error("Loan defaulted due to missed payments",
								zap.String("matchID", match.ID),
								zap.Int("paymentsMissed", match.PaymentsMissed))
							// Pass types.StatusDefaulted as the specific final status
							err := m.UpdateReputationAndFinalizeMatch(match.ID, false, types.StatusDefaulted, p2pNode)
							if err != nil {
								loanLogger.Error("Error finalizing defaulted match", zap.String("matchID", match.ID), zap.Error(err))
							}
							continue // Match finalized
						}
					}
				}

				// Loan Completion Logic - only if still active
				if match.Status == types.StatusActive && now.After(match.EndDate) {
					if match.PaymentsMissed == 0 {
						loanLogger.Info("Loan completed successfully", zap.String("matchID", match.ID))
						// Pass types.StatusCompleted
						err := m.UpdateReputationAndFinalizeMatch(match.ID, true, types.StatusCompleted, p2pNode)
						if err != nil {
							loanLogger.Error("Error finalizing successful match", zap.String("matchID", match.ID), zap.Error(err))
						}
					} else {
						loanLogger.Warn("Loan ended with outstanding missed payments, considered defaulted",
							zap.String("matchID", match.ID),
							zap.Int("paymentsMissed", match.PaymentsMissed))
						// Pass types.StatusDefaulted
						err := m.UpdateReputationAndFinalizeMatch(match.ID, false, types.StatusDefaulted, p2pNode)
						if err != nil {
							loanLogger.Error("Error finalizing match that ended with missed payments", zap.String("matchID", match.ID), zap.Error(err))
						}
					}
					continue // Match finalized
				}
				// Persist changes to match (if not done by UpdateReputationAndFinalizeMatch for ongoing changes)
				// Currently, m.activeMatches holds pointers, so modifications to 'match' fields like PaymentsMade, CurrentLTV etc.,
				// are directly reflected in the map. If UpdateReputationAndFinalizeMatch wasn't called (e.g. successful payment but not EOL),
				// these changes are implicitly saved in memory. For persistence, an explicit save step would be needed here or within Matcher methods.
			}
		}
	}
}


// reputationCmd represents the reputation command group
var reputationCmd = &cobra.Command{
	Use:   "reputation",
	Short: "Manage and view user reputations",
	Long:  `Allows viewing and manually setting user reputation scores for testing purposes.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// This ensures that logger is initialized.
		// It also implies that for these commands to work meaningfully,
		// the 'start' command should have been run to initialize the 'matcher'.
		if logger == nil {
			var err error
			logger, err = setupLogger(logLevel) // Use global logLevel
			if err != nil {
				return fmt.Errorf("failed to setup logger for reputation command: %w", err)
			}
		}
		if matcher == nil {
			// This is a simple CLI tool context. In a real app, matcher would be initialized
			// by the running node. Here, we rely on 'start' having been run.
			// We could try to initialize a temporary one, but it wouldn't reflect the node's state.
			logger.Warn("Matcher not initialized. Reputation commands may not reflect actual node state unless 'start' is active.")
			// For now, we'll allow the command to proceed, it might operate on a nil matcher if start wasn't run.
		}
		return nil
	},
}

// reputationGetCmd represents the command to get a user's reputation
var reputationGetCmd = &cobra.Command{
	Use:   "get [user-id]",
	Short: "Get a user's reputation score",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		userID := args[0]
		if matcher == nil {
			fmt.Println("Error: Matcher not initialized. Please start the node first.")
			return
		}
		score := matcher.GetUserReputation(userID)
		fmt.Printf("Reputation score for user %s: %d\n", userID, score)
	},
}

// reputationSetCmd represents the command to set a user's reputation
var reputationSetCmd = &cobra.Command{
	Use:   "set [user-id] [score]",
	Short: "Set a user's reputation score (for testing)",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		userID := args[0]
		scoreStr := args[1]
		score, err := strconv.Atoi(scoreStr)
		if err != nil {
			fmt.Printf("Error: invalid score '%s': %v\n", scoreStr, err)
			return
		}

		if matcher == nil {
			fmt.Println("Error: Matcher not initialized. Please start the node first.")
			return
		}
		// Note: SetUserReputation in matcher already logs the action
		matcher.SetUserReputation(userID, score)
		fmt.Printf("Reputation score for user %s set to %d\n", userID, score)
	},
}
