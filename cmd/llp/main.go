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
	btcPriceUSDT   float64
)

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
	rootCmd.PersistentFlags().Float64Var(&btcPriceUSDT, "btc-price", 50000.0, "current BTC price in USDT")

	// Add commands
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(offerCmd)
	rootCmd.AddCommand(requestCmd)
	rootCmd.AddCommand(matchCmd)
	rootCmd.AddCommand(statusCmd)
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
	Run: func(cmd *cobra.Command, args []string) {
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
		var err error
		logger, err = setupLogger(logLevel)
		if err != nil {
			fmt.Printf("Failed to setup logger: %v\n", err)
			os.Exit(1)
		}
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

		// Create P2P node
		p2pNode, err = p2p.NewP2PNode(ctx, maddrs, logger)
		if err != nil {
			logger.Fatal("Failed to create P2P node", zap.Error(err))
		}

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

		// Initialize order book
		orderBook = orderbook.NewOrderBook(ctx, p2pNode, logger)

		// Initialize matcher
		matcher = matching.NewMatcher(orderBook, matching.DefaultMatchingConfig(), logger)

		logger.Info("LLP node started",
			zap.String("nodeID", p2pNode.GetNodeID()),
			zap.Strings("listenAddrs", listenAddrs),
			zap.Int("bootstrapPeers", len(bootstrapMaddrs)))

		// Print node information
		fmt.Printf("Node ID: %s\n", p2pNode.GetNodeID())
		fmt.Printf("Listening on: %v\n", listenAddrs)
		fmt.Printf("Bootstrap peers: %v\n", bootstrapPeers)
		fmt.Printf("Current BTC price: %.2f USDT\n", btcPriceUSDT)

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
		ltv := usdtAmount / (btcCollateral * btcPriceUSDT)
		if ltv >= types.LiquidationLTVRatio {
			fmt.Printf("Error: LTV ratio (%.2f) is too high, must be below %.2f\n",
				ltv, types.LiquidationLTVRatio)
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
			ltv := request.CalculateLTV(btcPriceUSDT)
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
		matchingOffers := orderBook.GetMatchingOffers(request, btcPriceUSDT)
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
		match, err := matcher.MatchRequest(requestID, btcPriceUSDT)
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
		// Get order book stats
		stats := orderBook.GetStats()

		fmt.Println("Lightning Loan Protocol Node Status")
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("Node ID: %s\n", p2pNode.GetNodeID())
		fmt.Printf("Connected peers: %d\n", stats["peer_count"])
		fmt.Printf("Current BTC price: %.2f USDT\n", btcPriceUSDT)
		fmt.Println("------------------------------------------------------------")
		fmt.Printf("Order Book Statistics:\n")
		fmt.Printf("  Offers: %d\n", stats["offer_count"])
		fmt.Printf("  Requests: %d\n", stats["request_count"])
		fmt.Printf("  Lenders: %d\n", stats["lender_count"])
		fmt.Printf("  Borrowers: %d\n", stats["borrower_count"])
		fmt.Printf("  Total USDT offered: %.2f\n", stats["total_usdt_offered"])
		fmt.Printf("  Total BTC requested as collateral: %.8f\n", stats["total_btc_requested"])
		fmt.Println("------------------------------------------------------------")
	},
}

// setBTCPriceCmd represents the set-btc-price command
var setBTCPriceCmd = &cobra.Command{
	Use:   "set-btc-price [price]",
	Short: "Set the current BTC price",
	Long:  `Set the current BTC price in USDT for LTV calculations.`,
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		price, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			fmt.Printf("Error: invalid price format: %v\n", err)
			return
		}

		if price <= 0 {
			fmt.Println("Error: price must be positive")
			return
		}

		btcPriceUSDT = price
		fmt.Printf("BTC price set to %.2f USDT\n", btcPriceUSDT)
	},
}

func init() {
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

	// Add set-btc-price command to root command
	rootCmd.AddCommand(setBTCPriceCmd)
}
