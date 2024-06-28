package handler

import (
	"container/list"
	"errors"
	"math/big"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/Conflux-Chain/confura/node"
	"github.com/Conflux-Chain/confura/types"
	"github.com/Conflux-Chain/confura/util"
	sdk "github.com/Conflux-Chain/go-conflux-sdk"
	cfxtypes "github.com/Conflux-Chain/go-conflux-sdk/types"
	logutil "github.com/Conflux-Chain/go-conflux-util/log"
	"github.com/Conflux-Chain/go-conflux-util/viper"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/openweb3/go-rpc-provider/utils"
	"github.com/sirupsen/logrus"
)

const (
	// maxCachedBlockHashEpochs is the max number of epochs to cache their block hashes.
	maxCachedBlockHashEpochs = 100
)

// CfxGasStationHandler handles RPC requests for gas price estimation.
type CfxGasStationHandler struct {
	config             *GasStationConfig       // Gas station configuration
	status             atomic.Value            // Gas station status
	clientProvider     *node.CfxClientProvider // Client provider to get full node clients
	clients            []sdk.ClientOperator    // Clients used to get historical data
	cliIndex           int                     // Index of the main client
	fromEpoch          uint64                  // Start epoch number to sync from
	epochBlockHashList *list.List              // Linked list to store epoch block hashes
	window             *PriorityFeeWindow      // Block priority fee window
}

func MustNewCfxGasStationHandlerFromViper(cp *node.CfxClientProvider) *CfxGasStationHandler {
	var cfg GasStationConfig
	viper.MustUnmarshalKey("gasStation", &cfg)

	if !cfg.Enabled {
		return nil
	}

	// Get all clients in the group.
	clients, err := cp.GetClientsByGroup(node.GroupCfxHttp)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to get fullnode cluster")
	}

	if len(clients) == 0 {
		logrus.Fatal("No full node client available")
	}

	// Select a random client as the main client.
	cliIndex := rand.Int() % len(clients)
	// Get the latest epoch number with the main client.
	latestEpoch, err := clients[cliIndex].GetEpochNumber(cfxtypes.EpochLatestState)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to get latest epoch number")
	}

	fromEpoch := latestEpoch.ToInt().Uint64() - uint64(cfg.HistoricalPeekCount)
	h := &CfxGasStationHandler{
		config:             &cfg,
		clientProvider:     cp,
		clients:            clients,
		cliIndex:           cliIndex,
		epochBlockHashList: list.New(),
		fromEpoch:          fromEpoch,
		window:             NewPriorityFeeWindow(cfg.HistoricalPeekCount),
	}

	go h.run()
	return h
}

// run starts to sync historical data and refresh cluster nodes.
func (h *CfxGasStationHandler) run() {
	syncTicker := time.NewTimer(0)
	defer syncTicker.Stop()

	refreshTicker := time.NewTicker(clusterUpdateInterval)
	defer refreshTicker.Stop()

	etLogger := logutil.NewErrorTolerantLogger(logutil.DefaultETConfig)
	for {
		select {
		case <-syncTicker.C:
			complete, err := h.sync()
			etLogger.Log(
				logrus.WithFields(logrus.Fields{
					"status":    h.status.Load(),
					"fromEpoch": h.fromEpoch,
					"clients":   h.clients,
				}), err, "Gas Station handler sync error",
			)
			h.updateStatus(err)
			h.resetSyncTicker(syncTicker, complete, err)
		case <-refreshTicker.C:
			if err := h.refreshClusterNodes(); err != nil {
				logrus.WithError(err).Error("Gas station handler cluster refresh error")
			}
		}
	}
}

// sync synchronizes historical data from the full node cluster.
func (h *CfxGasStationHandler) sync() (complete bool, err error) {
	if len(h.clients) == 0 {
		return false, StationStatusClientUnavailable
	}

	h.cliIndex %= len(h.clients)
	for idx := h.cliIndex; ; {
		complete, err = h.trySync(h.clients[idx])
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"cliIndex": idx,
				"nodeUrl":  h.clients[idx].GetNodeURL(),
			}).WithError(err).Debug("Gas station handler sync once error")
		}

		if err == nil || utils.IsRPCJSONError(err) {
			h.cliIndex = idx
			break
		}

		idx = (idx + 1) % len(h.clients)
		if idx == h.cliIndex { // failed all nodes?
			break
		}
	}

	return complete, err
}

func (h *CfxGasStationHandler) trySync(cfx sdk.ClientOperator) (bool, error) {
	logrus.WithFields(logrus.Fields{
		"fromEpoch": h.fromEpoch,
		"nodeUrl":   cfx.GetNodeURL(),
	}).Debug("Gas station handler syncing once")

	// Get the latest epoch number.
	latestEpoch, err := cfx.GetEpochNumber(cfxtypes.EpochLatestState)
	if err != nil {
		return false, err
	}

	latestEpochNo := latestEpoch.ToInt().Uint64()
	if h.fromEpoch > latestEpochNo { // already catch-up?
		return true, nil
	}

	// Get the pivot block.
	epoch := cfxtypes.NewEpochNumberUint64(h.fromEpoch)
	pivotBlock, err := cfx.GetBlockByEpoch(epoch)
	if err != nil {
		return false, err
	}

	prevEpochBh := h.prevEpochPivotBlockHash()
	if len(prevEpochBh) > 0 && prevEpochBh != pivotBlock.ParentHash {
		logrus.WithFields(logrus.Fields{
			"prevEpochBh":          prevEpochBh,
			"pivotBlockHash":       pivotBlock.Hash,
			"pivotBlockParentHash": pivotBlock.ParentHash,
		}).Debug("Gas station handler detected reorg")

		// Reorg due to parent hash not match, remove the last epoch.
		h.handleReorg()
		h.fromEpoch--
		return false, nil
	}

	blockHashes, blocks, err := h.fetchBlocks(cfx, epoch, pivotBlock)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"pivotBlockHash": pivotBlock.Hash,
			"epoch":          epoch,
		}).WithError(err).Debug("Gas station handler fetch blocks error")
		return false, err
	}

	for i := range blocks {
		h.handleBlock(blocks[i])
	}

	h.push(blockHashes)
	h.fromEpoch++
	return false, nil
}

func (h *CfxGasStationHandler) fetchBlocks(
	cfx sdk.ClientOperator, epoch *cfxtypes.Epoch, pivotBlock *cfxtypes.Block,
) ([]cfxtypes.Hash, []*cfxtypes.Block, error) {
	// Get epoch block hashes.
	blockHashes, err := cfx.GetBlocksByEpoch(epoch)
	if err != nil {
		return nil, nil, err
	}

	pivotHash := blockHashes[len(blockHashes)-1]
	if pivotBlock.Hash != pivotHash { // abandon this epoch due to pivot switched
		return nil, nil, errors.New("pivot switched")
	}

	var blocks []*cfxtypes.Block
	for i := 0; i < len(blockHashes)-1; i++ {
		block, err := cfx.GetBlockByHashWithPivotAssumption(blockHashes[i], pivotHash, hexutil.Uint64(h.fromEpoch))
		if err != nil {
			return nil, nil, err
		}
		blocks = append(blocks, &block)
	}

	blocks = append(blocks, pivotBlock)
	return blockHashes, blocks, nil
}

func (h *CfxGasStationHandler) handleReorg() {
	var blockHashes []string
	for _, bh := range h.pop() {
		blockHashes = append(blockHashes, bh.String())
	}
	h.window.Remove(blockHashes...)

	logrus.WithField("blockHashes", blockHashes).Info("Gas station handler removed blocks due to reorg")
}

func (h *CfxGasStationHandler) handleBlock(block *cfxtypes.Block) {
	ratio, _ := big.NewInt(0).Div(block.GasUsed.ToInt(), block.GasLimit.ToInt()).Float64()
	blockFee := &BlockPriorityFee{
		number:       block.BlockNumber.ToInt().Uint64(),
		hash:         block.Hash.String(),
		baseFee:      block.BaseFeePerGas.ToInt(),
		gasUsedRatio: ratio,
	}

	var txnTips []*TxnPriorityFee
	for i := range block.Transactions {
		txn := block.Transactions[i]

		// Skip unexecuted transaction, e.g.
		// 1) already executed in previous block
		// 2) never executed, e.g. nonce mismatch
		if !util.IsTxExecutedInBlock(&txn) {
			continue
		}

		// Calculate max priority fee per gas if not set
		if txn.MaxPriorityFeePerGas == nil {
			maxFeePerGas := txn.MaxFeePerGas.ToInt()
			if maxFeePerGas == nil {
				maxFeePerGas = txn.GasPrice.ToInt()
			}

			baseFeePerGas := block.BaseFeePerGas.ToInt()
			txn.MaxPriorityFeePerGas = (*hexutil.Big)(big.NewInt(0).Sub(maxFeePerGas, baseFeePerGas))
		}
		logrus.WithFields(logrus.Fields{
			"txnHash":              txn.Hash,
			"maxPriorityFeePerGas": txn.MaxPriorityFeePerGas,
			"maxFeePerGas":         txn.MaxFeePerGas,
			"baseFeePerGas":        block.BaseFeePerGas,
			"gasPrice":             txn.GasPrice,
		}).Debug("Gas station handler calculated txn priority fee")

		txnTips = append(txnTips, &TxnPriorityFee{
			hash: txn.Hash.String(),
			tip:  txn.MaxPriorityFeePerGas.ToInt(),
		})
	}

	logrus.WithFields(logrus.Fields{
		"blockFeeInfo": blockFee,
		"execTxnCount": len(txnTips),
	}).Debug("Gas station handler pushing block")

	blockFee.Append(txnTips...)
	h.window.Push(blockFee)
}

func (h *CfxGasStationHandler) pop() []cfxtypes.Hash {
	if h.epochBlockHashList.Len() == 0 {
		return nil
	}

	lastElement := h.epochBlockHashList.Back()
	return h.epochBlockHashList.Remove(lastElement).([]cfxtypes.Hash)
}

func (h *CfxGasStationHandler) prevEpochPivotBlockHash() cfxtypes.Hash {
	if h.epochBlockHashList.Len() == 0 {
		return cfxtypes.Hash("")
	}

	blockHashes := h.epochBlockHashList.Back().Value.([]cfxtypes.Hash)
	return blockHashes[len(blockHashes)-1]
}

func (h *CfxGasStationHandler) push(blockHashes []cfxtypes.Hash) {
	h.epochBlockHashList.PushBack(blockHashes)
	if h.epochBlockHashList.Len() > maxCachedBlockHashEpochs {
		// Remove old epoch block hashes if capacity is reached
		h.epochBlockHashList.Remove(h.epochBlockHashList.Front())
	}
}

func (h *CfxGasStationHandler) updateStatus(err error) {
	if err != nil && !utils.IsRPCJSONError(err) {
		// Set the gas station as unavailable due to network error.
		h.status.Store(err)
	} else {
		h.status.Store(StationStatusOk)
	}
}

func (h *CfxGasStationHandler) refreshClusterNodes() error {
	clients, err := h.clientProvider.GetClientsByGroup(node.GroupCfxHttp)
	if err != nil {
		return err
	}

	h.clients = clients
	return nil
}

func (h *CfxGasStationHandler) resetSyncTicker(syncTicker *time.Timer, complete bool, err error) {
	switch {
	case err != nil:
		syncTicker.Reset(syncIntervalNormal)
	case complete:
		syncTicker.Reset(syncIntervalNormal)
	default:
		syncTicker.Reset(syncIntervalCatchUp)
	}
}

func (h *CfxGasStationHandler) Suggest(cfx sdk.ClientOperator) (*types.SuggestedGasFees, error) {
	if status := h.status.Load(); status != StationStatusOk {
		return nil, status.(error)
	}

	latestBlock, err := cfx.GetBlockSummaryByEpoch(cfxtypes.EpochLatestState)
	if err != nil {
		return nil, err
	}

	baseFeePerGas := latestBlock.BaseFeePerGas.ToInt()
	// Calculate the gas fee stats from the priority fee window.
	stats := h.window.Calculate(h.config.Percentiles[:])

	priorityFees := stats.AvgPercentiledPriorityFee
	if priorityFees == nil { // use gas fees directly from the blockchain if no estimation made
		oracleFee, err := cfx.GetMaxPriorityFeePerGas()
		if err != nil {
			return nil, err
		}

		for i := 0; i < 3; i++ {
			priorityFees = append(priorityFees, oracleFee.ToInt())
		}
	}

	return &types.SuggestedGasFees{
		Low: types.GasFeeEstimation{
			SuggestedMaxPriorityFeePerGas: (*hexutil.Big)(priorityFees[0]),
			SuggestedMaxFeePerGas:         (*hexutil.Big)(big.NewInt(0).Add(baseFeePerGas, priorityFees[0])),
		},
		Medium: types.GasFeeEstimation{
			SuggestedMaxPriorityFeePerGas: (*hexutil.Big)(priorityFees[1]),
			SuggestedMaxFeePerGas:         (*hexutil.Big)(big.NewInt(0).Add(baseFeePerGas, priorityFees[1])),
		},
		High: types.GasFeeEstimation{
			SuggestedMaxPriorityFeePerGas: (*hexutil.Big)(priorityFees[2]),
			SuggestedMaxFeePerGas:         (*hexutil.Big)(big.NewInt(0).Add(baseFeePerGas, priorityFees[2])),
		},
		EstimatedBaseFee:           latestBlock.BaseFeePerGas,
		NetworkCongestion:          stats.NetworkCongestion,
		LatestPriorityFeeRange:     ToHexBigSlice(stats.LatestPriorityFeeRange),
		HistoricalPriorityFeeRange: ToHexBigSlice(stats.HistoricalPriorityFeeRange),
		HistoricalBaseFeeRange:     ToHexBigSlice(stats.HistoricalBaseFeeRange),
		PriorityFeeTrend:           stats.PriorityFeeTrend,
		BaseFeeTrend:               stats.BaseFeeTrend,
	}, nil
}