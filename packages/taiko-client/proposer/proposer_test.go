package proposer

import (
	"context"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/stretchr/testify/suite"

	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings/metadata"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/driver/chain_syncer/beaconsync"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/driver/chain_syncer/blob"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/driver/state"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/internal/testutils"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/internal/utils"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/config"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/jwt"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/rpc"
	builder "github.com/taikoxyz/taiko-mono/packages/taiko-client/proposer/transaction_builder"
)

type ProposerTestSuite struct {
	testutils.ClientTestSuite
	s      *blob.Syncer
	p      *Proposer
	cancel context.CancelFunc
}

func (s *ProposerTestSuite) SetupTest() {
	s.ClientTestSuite.SetupTest()

	state2, err := state.New(context.Background(), s.RPCClient)
	s.Nil(err)

	syncer, err := blob.NewSyncer(
		context.Background(),
		s.RPCClient,
		state2,
		beaconsync.NewSyncProgressTracker(s.RPCClient.L2, 1*time.Hour),
		0,
		nil,
		nil,
	)
	s.Nil(err)
	s.s = syncer

	l1ProposerPrivKey, err := crypto.ToECDSA(common.FromHex(os.Getenv("L1_PROPOSER_PRIVATE_KEY")))
	s.Nil(err)

	p := new(Proposer)

	ctx, cancel := context.WithCancel(context.Background())
	jwtSecret, err := jwt.ParseSecretFromFile(os.Getenv("JWT_SECRET"))
	s.Nil(err)
	s.NotEmpty(jwtSecret)

	s.Nil(p.InitFromConfig(ctx, &Config{
		ClientConfig: &rpc.ClientConfig{
			L1Endpoint:        os.Getenv("L1_WS"),
			L2Endpoint:        os.Getenv("L2_HTTP"),
			L2EngineEndpoint:  os.Getenv("L2_AUTH"),
			JwtSecret:         string(jwtSecret),
			TaikoL1Address:    common.HexToAddress(os.Getenv("TAIKO_L1")),
			TaikoL2Address:    common.HexToAddress(os.Getenv("TAIKO_L2")),
			TaikoTokenAddress: common.HexToAddress(os.Getenv("TAIKO_TOKEN")),
		},
		L1ProposerPrivKey:          l1ProposerPrivKey,
		L2SuggestedFeeRecipient:    common.HexToAddress(os.Getenv("L2_SUGGESTED_FEE_RECIPIENT")),
		MinProposingInternal:       0,
		ProposeInterval:            1024 * time.Hour,
		MaxProposedTxListsPerEpoch: 1,
		ExtraData:                  "test",
		ProposeBlockTxGasLimit:     10_000_000,
		TxmgrConfigs: &txmgr.CLIConfig{
			L1RPCURL:                  os.Getenv("L1_WS"),
			NumConfirmations:          0,
			SafeAbortNonceTooLowCount: txmgr.DefaultBatcherFlagValues.SafeAbortNonceTooLowCount,
			PrivateKey:                common.Bytes2Hex(crypto.FromECDSA(l1ProposerPrivKey)),
			FeeLimitMultiplier:        txmgr.DefaultBatcherFlagValues.FeeLimitMultiplier,
			FeeLimitThresholdGwei:     txmgr.DefaultBatcherFlagValues.FeeLimitThresholdGwei,
			MinBaseFeeGwei:            txmgr.DefaultBatcherFlagValues.MinBaseFeeGwei,
			MinTipCapGwei:             txmgr.DefaultBatcherFlagValues.MinTipCapGwei,
			ResubmissionTimeout:       txmgr.DefaultBatcherFlagValues.ResubmissionTimeout,
			ReceiptQueryInterval:      1 * time.Second,
			NetworkTimeout:            txmgr.DefaultBatcherFlagValues.NetworkTimeout,
			TxSendTimeout:             txmgr.DefaultBatcherFlagValues.TxSendTimeout,
			TxNotInMempoolTimeout:     txmgr.DefaultBatcherFlagValues.TxNotInMempoolTimeout,
		},
		PrivateTxmgrConfigs: &txmgr.CLIConfig{
			L1RPCURL:                  os.Getenv("L1_WS"),
			NumConfirmations:          0,
			SafeAbortNonceTooLowCount: txmgr.DefaultBatcherFlagValues.SafeAbortNonceTooLowCount,
			PrivateKey:                common.Bytes2Hex(crypto.FromECDSA(l1ProposerPrivKey)),
			FeeLimitMultiplier:        txmgr.DefaultBatcherFlagValues.FeeLimitMultiplier,
			FeeLimitThresholdGwei:     txmgr.DefaultBatcherFlagValues.FeeLimitThresholdGwei,
			MinBaseFeeGwei:            txmgr.DefaultBatcherFlagValues.MinBaseFeeGwei,
			MinTipCapGwei:             txmgr.DefaultBatcherFlagValues.MinTipCapGwei,
			ResubmissionTimeout:       txmgr.DefaultBatcherFlagValues.ResubmissionTimeout,
			ReceiptQueryInterval:      1 * time.Second,
			NetworkTimeout:            txmgr.DefaultBatcherFlagValues.NetworkTimeout,
			TxSendTimeout:             txmgr.DefaultBatcherFlagValues.TxSendTimeout,
			TxNotInMempoolTimeout:     txmgr.DefaultBatcherFlagValues.TxNotInMempoolTimeout,
		},
	}, nil, nil))

	s.p = p
	s.cancel = cancel
}

func (s *ProposerTestSuite) TestProposeTxLists() {
	p := s.p
	ctx := p.ctx
	cfg := s.p.Config

	txBuilder := builder.NewBlobTransactionBuilder(
		p.rpc,
		p.L1ProposerPrivKey,
		cfg.TaikoL1Address,
		cfg.ProverSetAddress,
		cfg.L2SuggestedFeeRecipient,
		cfg.ProposeBlockTxGasLimit,
		cfg.ExtraData,
		config.NewChainConfig(s.p.protocolConfigs),
	)

	emptyTxListBytes, err := rlp.EncodeToBytes(types.Transactions{})
	s.Nil(err)
	txListsBytes := [][]byte{emptyTxListBytes}
	txCandidates := make([]txmgr.TxCandidate, len(txListsBytes))
	for i, txListBytes := range txListsBytes {
		compressedTxListBytes, err := utils.Compress(txListBytes)
		if err != nil {
			log.Warn("Failed to compress transactions list", "index", i, "error", err)
			break
		}

		candidate, err := txBuilder.BuildLegacy(
			p.ctx,
			p.IncludeParentMetaHash,
			compressedTxListBytes,
		)
		if err != nil {
			log.Warn("Failed to build TaikoL1.proposeBlock transaction", "error", err)
			break
		}

		// trigger the error
		candidate.GasLimit = 10_000_000

		txCandidates[i] = *candidate
	}

	for _, txCandidate := range txCandidates {
		txMgr, _ := p.txmgrSelector.Select()
		receipt, err := txMgr.Send(ctx, txCandidate)
		s.Nil(err)
		s.Nil(encoding.TryParsingCustomErrorFromReceipt(ctx, p.rpc.L1, p.proposerAddress, receipt))
	}
}

func (s *ProposerTestSuite) TestProposeOpNoEmptyBlock() {
	// TODO: Temporarily skip this test case when using l2_reth node.
	if os.Getenv("L2_NODE") == "l2_reth" {
		s.T().Skip()
	}
	defer s.Nil(s.s.ProcessL1Blocks(context.Background()))

	p := s.p

	batchSize := 100

	var err error
	for i := 0; i < batchSize; i++ {
		to := common.BytesToAddress(testutils.RandomBytes(32))
		_, err = testutils.SendDynamicFeeTx(s.RPCClient.L2, s.TestAddrPrivKey, &to, nil, nil)
		s.Nil(err)
	}

	var preBuiltTxList []*miner.PreBuiltTxList
	for i := 0; i < 3 && len(preBuiltTxList) == 0; i++ {
		preBuiltTxList, err = s.RPCClient.GetPoolContent(
			context.Background(),
			p.proposerAddress,
			p.protocolConfigs.BlockMaxGasLimit,
			rpc.BlockMaxTxListBytes,
			p.LocalAddresses,
			p.MaxProposedTxListsPerEpoch,
			0,
			p.chainConfig,
		)
		time.Sleep(time.Second)
	}
	s.Nil(err)
	s.Equal(true, len(preBuiltTxList) > 0)

	var (
		blockMinGasLimit    uint64 = math.MaxUint64
		blockMinTxListBytes uint64 = math.MaxUint64
	)
	for _, txs := range preBuiltTxList {
		if txs.EstimatedGasUsed <= blockMinGasLimit {
			blockMinGasLimit = txs.EstimatedGasUsed
		} else {
			break
		}
		if txs.BytesLength <= blockMinTxListBytes {
			blockMinTxListBytes = txs.BytesLength
		} else {
			break
		}
	}

	// Start proposer
	p.LocalAddressesOnly = false
	p.MinGasUsed = blockMinGasLimit
	p.MinTxListBytes = blockMinTxListBytes
	p.ProposeInterval = time.Second
	p.MinProposingInternal = time.Minute
	s.Nil(p.ProposeOp(context.Background()))
}

func (s *ProposerTestSuite) TestName() {
	s.Equal("proposer", s.p.Name())
}

func (s *ProposerTestSuite) TestProposeOp() {
	testCases := []struct {
		name               string
		checkProfitability bool
	}{
		{
			name:               "Without profitability check",
			checkProfitability: false,
		},
		{
			name:               "With profitability check",
			checkProfitability: true,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			// Set profitability check
			s.p.checkProfitability = tc.checkProfitability

			// Propose txs in L2 execution engine's mempool
			sink := make(chan *bindings.TaikoL1ClientBlockProposed)

			sub, err := s.p.rpc.TaikoL1.WatchBlockProposed(nil, sink, nil, nil)
			s.Nil(err)
			defer func() {
				sub.Unsubscribe()
				close(sink)
			}()

			sink2 := make(chan *bindings.TaikoL1ClientBlockProposedV2)

			sub2, err := s.p.rpc.TaikoL1.WatchBlockProposedV2(nil, sink2, nil)
			s.Nil(err)
			defer func() {
				sub2.Unsubscribe()
				close(sink2)
			}()

			to := common.BytesToAddress(testutils.RandomBytes(32))
			_, err = testutils.SendDynamicFeeTx(s.p.rpc.L2, s.TestAddrPrivKey, &to, common.Big1, nil)
			s.Nil(err)

			s.Nil(s.p.ProposeOp(context.Background()))
			s.Nil(s.s.ProcessL1Blocks(context.Background()))

			var (
				meta metadata.TaikoBlockMetaData
			)
			select {
			case event := <-sink:
				meta = metadata.NewTaikoDataBlockMetadataLegacy(event)
			case event := <-sink2:
				meta = metadata.NewTaikoDataBlockMetadataOntake(event)
			}

			s.Equal(meta.GetCoinbase(), s.p.L2SuggestedFeeRecipient)

			_, isPending, err := s.p.rpc.L1.TransactionByHash(context.Background(), meta.GetTxHash())
			s.Nil(err)
			s.False(isPending)

			receipt, err := s.p.rpc.L1.TransactionReceipt(context.Background(), meta.GetTxHash())
			s.Nil(err)
			s.Equal(types.ReceiptStatusSuccessful, receipt.Status)
		})
	}
}

func (s *ProposerTestSuite) TestProposeEmptyBlockOp() {
	s.p.MinProposingInternal = 1 * time.Second
	s.p.lastProposedAt = time.Now().Add(-10 * time.Second)
	s.Nil(s.p.ProposeOp(context.Background()))
}

func (s *ProposerTestSuite) TestProposeTxListOntake() {
	for i := 0; i < int(s.p.protocolConfigs.OntakeForkHeight); i++ {
		s.ProposeAndInsertValidBlock(s.p, s.s)
	}

	l2Head, err := s.p.rpc.L2.HeaderByNumber(context.Background(), nil)
	s.Nil(err)
	s.GreaterOrEqual(l2Head.Number.Uint64(), s.p.protocolConfigs.OntakeForkHeight)

	sink := make(chan *bindings.TaikoL1ClientBlockProposedV2)
	sub, err := s.p.rpc.TaikoL1.WatchBlockProposedV2(nil, sink, nil)
	s.Nil(err)
	defer func() {
		sub.Unsubscribe()
		close(sink)
	}()
	s.Nil(s.p.ProposeTxListOntake(context.Background(), []types.Transactions{{}, {}}))
	s.Nil(s.s.ProcessL1Blocks(context.Background()))

	var l1Height *big.Int
	for i := 0; i < 2; i++ {
		event := <-sink
		if l1Height == nil {
			l1Height = new(big.Int).SetUint64(event.Raw.BlockNumber)
			continue
		}
		s.Equal(l1Height.Uint64(), event.Raw.BlockNumber)
	}

	newL2head, err := s.p.rpc.L2.HeaderByNumber(context.Background(), nil)
	s.Nil(err)

	s.Equal(l2Head.Number.Uint64()+2, newL2head.Number.Uint64())
}

func (s *ProposerTestSuite) TestUpdateProposingTicker() {
	s.p.ProposeInterval = 1 * time.Hour
	s.NotPanics(s.p.updateProposingTicker)

	s.p.ProposeInterval = 0
	s.NotPanics(s.p.updateProposingTicker)
}

func (s *ProposerTestSuite) TestStartClose() {
	s.Nil(s.p.Start())
	s.cancel()
	s.NotPanics(func() { s.p.Close(s.p.ctx) })
}

func TestProposerTestSuite(t *testing.T) {
	suite.Run(t, new(ProposerTestSuite))
}

func (s *ProposerTestSuite) TestEstimateTotalCosts() {
	s.p.OffChainCosts = big.NewInt(500000000000) // 500 Gwei for off-chain costs
	s.p.GasNeededForProvingBlock = 3000000

	tests := []struct {
		name           string
		proposingCosts *big.Int
	}{
		{
			name:           "normal estimation",
			proposingCosts: big.NewInt(300000000000), // 300 Gwei
		},
		{
			name:           "zero proposing costs",
			proposingCosts: big.NewInt(0),
		},
	}

	for _, test := range tests {
		s.Run(test.name, func() {
			costs, err := s.p.estimateTotalCosts(test.proposingCosts)
			log.Debug("Estimated total costs", "costs", costs)

			s.NoError(err)
			s.NotNil(costs)
			s.Greater(costs.Int64(), int64(0))
		})
	}
}

func (s *ProposerTestSuite) TestIsProfitable() {
	s.p.OffChainCosts = big.NewInt(500000000000) // 500 Gwei for off-chain costs
	s.p.GasNeededForProvingBlock = 3000000

	tests := []struct {
		name           string
		txList         types.Transactions
		proposingCosts *big.Int
		expectedResult bool
		expectedError  bool
	}{
		{
			name:           "empty tx list",
			txList:         types.Transactions{},
			proposingCosts: big.NewInt(100000000000), // 100 Gwei
			expectedResult: false,
			expectedError:  false,
		},
		{
			name: "profitable tx list",
			txList: func() types.Transactions {
				txsNumber := 5
				txs := make(types.Transactions, txsNumber)
				for i := 0; i < txsNumber; i++ {
					txs[i] = types.NewTx(&types.DynamicFeeTx{
						ChainID:   big.NewInt(1),
						Nonce:     uint64(i),
						GasTipCap: big.NewInt(40000000000), // 40 Gwei gas tip cap
						GasFeeCap: big.NewInt(40000000000), // 40 Gwei gas fee cap
						Gas:       30000000,                // gas limit
						To:        &common.Address{},
						Value:     big.NewInt(0),
						Data:      nil,
					})
				}
				return txs
			}(),
			proposingCosts: big.NewInt(10000000000), // 10 Gwei
			expectedResult: true,
			expectedError:  false,
		},
		{
			name: "unprofitable tx list",
			txList: types.Transactions{
				types.NewTx(&types.DynamicFeeTx{
					ChainID:   big.NewInt(1),
					Nonce:     0,
					GasTipCap: big.NewInt(40000000),
					GasFeeCap: big.NewInt(40000000),
					Gas:       3000_000, // gas limit
					To:        &common.Address{},
					Value:     big.NewInt(0),
					Data:      nil,
				}),
			},
			proposingCosts: big.NewInt(100000000000), // 100 Gwei
			expectedResult: false,
			expectedError:  false,
		},
	}

	for _, test := range tests {
		s.Run(test.name, func() {
			txLists := []types.Transactions{test.txList}
			fees, err := s.p.calculateTotalL2TransactionsFees(txLists)
			s.NoError(err)
			profitable, err := s.p.isProfitable(fees, test.proposingCosts)

			if test.expectedError {
				s.Error(err)
				return
			}

			s.NoError(err)
			s.Equal(test.expectedResult, profitable)
		})
	}
}
