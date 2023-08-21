package miner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/abi/bind/backends"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/stretchr/testify/require"
	"math/big"
	mathrand "math/rand"
	"testing"
)

// NOTE(wazzymandias): Below is a FuzzTest contract written in Solidity and shown here as reference code
// for the generated abi and bytecode used for testing.
// The generated abi can be found in the `testdata` directory.
// The abi, bytecode, and Go bindings were generated using the following commands:
//   - docker run -v ${STATE_FUZZ_TEST_CONTRACT_DIRECTORY}:/sources
//     ethereum/solc:0.8.19 -o /sources/output --abi --bin /sources/StateFuzzTest.sol
//   - go run ./cmd/abigen/ --bin ${TARGET_STATE_FUZZ_TEST_BIN_PATH} --abi ${TARGET_STATE_FUZZ_TEST_ABI_PATH}
//     --pkg statefuzztest --out=state_fuzz_test_abigen_bindings.go
const StateFuzzTestSolidity = `	
pragma solidity 0.8.19;

contract StateFuzzTest {
    mapping(address => uint256) public balances;
    mapping(bytes32 => bytes) public storageData;
    mapping(address => bool) public isSelfDestructed;

    function createObject(bytes32 key, bytes memory value) public {
        storageData[key] = value;
    }

    function resetObject(bytes32 key) public {
        delete storageData[key];
    }

    function selfDestruct() public {
        isSelfDestructed[msg.sender] = true;
        selfdestruct(payable(msg.sender));
    }

    function changeBalance(address account, uint256 newBalance) public {
        balances[account] = newBalance;
    }

    function changeStorage(bytes32 key, bytes memory newValue) public {
        storageData[key] = newValue;
    }
}
`

func changeBalanceFuzzTestContract(nonce uint64, to, address common.Address, newBalance *big.Int) (types.TxData, error) {
	abi, err := StatefuzztestMetaData.GetAbi()
	if err != nil {
		return nil, err
	}

	data, err := abi.Pack("changeBalance", address, newBalance)
	if err != nil {
		return nil, err
	}

	return &types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(1),
		Gas:      10_000_000,
		To:       (*common.Address)(to[:]),
		Value:    big.NewInt(0),
		Data:     data,
	}, nil
}

func resetObjectFuzzTestContract(nonce uint64, address common.Address, key [32]byte) (types.TxData, error) {
	abi, err := StatefuzztestMetaData.GetAbi()
	if err != nil {
		return nil, err
	}

	data, err := abi.Pack("resetObject", key)
	if err != nil {
		return nil, err
	}

	return &types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(1),
		Gas:      10_000_000,
		To:       (*common.Address)(address[:]),
		Value:    big.NewInt(0),
		Data:     data,
	}, nil
}

func createObjectFuzzTestContract(chainID *big.Int, nonce uint64, to common.Address, key [32]byte, value []byte) (types.TxData, error) {
	abi, err := StatefuzztestMetaData.GetAbi()
	if err != nil {
		return nil, err
	}

	data, err := abi.Pack("createObject", key, value)
	if err != nil {
		return nil, err
	}

	return &types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		Gas:       100_000,
		GasFeeCap: big.NewInt(1),
		To:        (*common.Address)(to[:]),
		Value:     big.NewInt(0),
		Data:      data,
	}, nil
}

func selfDestructFuzzTestContract(chainID *big.Int, nonce uint64, to common.Address) (types.TxData, error) {
	abi, err := StatefuzztestMetaData.GetAbi()
	if err != nil {
		return nil, err
	}

	data, err := abi.Pack("selfDestruct")
	if err != nil {
		return nil, err
	}

	return &types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		Gas:       500_000,
		GasFeeCap: big.NewInt(1),
		To:        (*common.Address)(to[:]),
		Value:     big.NewInt(0),
		Data:      data,
	}, nil
}

func changeStorageFuzzTestContract(chainID *big.Int, nonce uint64, to common.Address, key [32]byte, value []byte) (types.TxData, error) {
	abi, err := StatefuzztestMetaData.GetAbi()
	if err != nil {
		return nil, err
	}

	data, err := abi.Pack("changeStorage", key, value)
	if err != nil {
		return nil, err
	}

	return &types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		Gas:       100_000,
		GasFeeCap: big.NewInt(1),
		To:        (*common.Address)(to[:]),
		Value:     big.NewInt(0),
		Data:      data,
	}, nil
}

const (
	Baseline       = 0
	SingleSnapshot = 1
	MultiSnapshot  = 2
)

type stateComparisonTestContext struct {
	Name string

	statedb   *state.StateDB
	chainData chainData
	signers   signerList

	env *environment

	envDiff *environmentDiff
	changes *envChanges

	transactions []*types.Transaction

	rootHash common.Hash
}

type stateComparisonTestContexts []stateComparisonTestContext

func (sc stateComparisonTestContexts) ValidateRootHashes(t *testing.T, expected common.Hash) {
	for _, tc := range sc {
		require.Equal(t, expected.Bytes(), tc.rootHash.Bytes(),
			"root hash mismatch for test context %s [expected: %s] [found: %s]",
			tc.Name, expected.TerminalString(), tc.rootHash.TerminalString())
	}
}

func (sc stateComparisonTestContexts) GenerateTransactions(t *testing.T, txCount int, failEveryN int) {
	for tcIndex, tc := range sc {
		signers := tc.signers
		tc.transactions = sc.generateTransactions(txCount, failEveryN, signers)
		tc.signers = signers
		require.Len(t, tc.transactions, txCount)

		sc[tcIndex] = tc
	}
}

func (sc stateComparisonTestContexts) generateTransactions(txCount int, failEveryN int, signers signerList) []*types.Transaction {
	transactions := make([]*types.Transaction, 0, txCount)
	for i := 0; i < txCount; i++ {
		var data []byte
		if failEveryN != 0 && i%failEveryN == 0 {
			data = []byte{0x01}
		} else {
			data = []byte{}
		}

		from := i % len(signers.addresses)
		tx := signers.signTx(from, params.TxGas, big.NewInt(0), big.NewInt(1),
			signers.addresses[(i+1)%len(signers.addresses)], big.NewInt(0), data)
		transactions = append(transactions, tx)
	}

	return transactions
}

func (sc stateComparisonTestContexts) UpdateRootHashes(t *testing.T) {
	for tcIndex, tc := range sc {
		if tc.envDiff != nil {
			tc.rootHash = tc.envDiff.baseEnvironment.state.IntermediateRoot(true)
		} else {
			tc.rootHash = tc.env.state.IntermediateRoot(true)
		}
		sc[tcIndex] = tc

		require.NotEmpty(t, tc.rootHash.Bytes(), "root hash is empty for test context %s", tc.Name)
	}
}

func (sc stateComparisonTestContexts) ValidateTestCases(t *testing.T, reference int) {
	expected := sc[reference]
	var (
		expectedGasPool      *core.GasPool        = expected.envDiff.baseEnvironment.gasPool
		expectedHeader       *types.Header        = expected.envDiff.baseEnvironment.header
		expectedProfit       *big.Int             = expected.envDiff.baseEnvironment.profit
		expectedTxCount      int                  = expected.envDiff.baseEnvironment.tcount
		expectedTransactions []*types.Transaction = expected.envDiff.baseEnvironment.txs
		expectedReceipts     types.Receipts       = expected.envDiff.baseEnvironment.receipts
	)
	for tcIndex, tc := range sc {
		if tcIndex == reference {
			continue
		}

		var (
			actualGasPool      *core.GasPool        = tc.env.gasPool
			actualHeader       *types.Header        = tc.env.header
			actualProfit       *big.Int             = tc.env.profit
			actualTxCount      int                  = tc.env.tcount
			actualTransactions []*types.Transaction = tc.env.txs
			actualReceipts     types.Receipts       = tc.env.receipts
		)
		if actualGasPool.Gas() != expectedGasPool.Gas() {
			t.Errorf("gas pool mismatch for test context %s [expected: %d] [found: %d]",
				tc.Name, expectedGasPool.Gas(), actualGasPool.Gas())
		}

		if actualHeader.Hash() != expectedHeader.Hash() {
			t.Errorf("header hash mismatch for test context %s [expected: %s] [found: %s]",
				tc.Name, expectedHeader.Hash().TerminalString(), actualHeader.Hash().TerminalString())
		}

		if actualProfit.Cmp(expectedProfit) != 0 {
			t.Errorf("profit mismatch for test context %s [expected: %d] [found: %d]",
				tc.Name, expectedProfit, actualProfit)
		}

		if actualTxCount != expectedTxCount {
			t.Errorf("transaction count mismatch for test context %s [expected: %d] [found: %d]",
				tc.Name, expectedTxCount, actualTxCount)
			break
		}

		if len(actualTransactions) != len(expectedTransactions) {
			t.Errorf("transaction count mismatch for test context %s [expected: %d] [found: %d]",
				tc.Name, len(expectedTransactions), len(actualTransactions))
		}

		for txIdx := 0; txIdx < len(actualTransactions); txIdx++ {
			expectedTx := expectedTransactions[txIdx]
			actualTx := actualTransactions[txIdx]

			expectedBytes, err := rlp.EncodeToBytes(expectedTx)
			if err != nil {
				t.Fatalf("failed to encode expected transaction #%d: %v", txIdx, err)
			}

			actualBytes, err := rlp.EncodeToBytes(actualTx)
			if err != nil {
				t.Fatalf("failed to encode actual transaction #%d: %v", txIdx, err)
			}

			if !bytes.Equal(expectedBytes, actualBytes) {
				t.Errorf("transaction #%d mismatch for test context %s [expected: %v] [found: %v]",
					txIdx, tc.Name, expectedTx, actualTx)
			}
		}

		if len(actualReceipts) != len(expectedReceipts) {
			t.Errorf("receipt count mismatch for test context %s [expected: %d] [found: %d]",
				tc.Name, len(expectedReceipts), len(actualReceipts))
		}
	}
}

func (sc stateComparisonTestContexts) Init(t *testing.T, gasLimit uint64) stateComparisonTestContexts {
	for i := range sc {
		tc := stateComparisonTestContext{}
		tc.statedb, tc.chainData, tc.signers = genTestSetup(gasLimit)
		tc.env = newEnvironment(tc.chainData, tc.statedb, tc.signers.addresses[0], gasLimit, big.NewInt(1))
		var err error
		switch i {
		case Baseline:
			tc.Name = "baseline"
			tc.envDiff = newEnvironmentDiff(tc.env)
		case SingleSnapshot:
			tc.Name = "single-snapshot"
			tc.changes, err = newEnvChanges(tc.env)
			_ = tc.changes.env.state.MultiTxSnapshotCommit()
		case MultiSnapshot:
			tc.Name = "multi-snapshot"
			tc.changes, err = newEnvChanges(tc.env)
			_ = tc.changes.env.state.MultiTxSnapshotCommit()
		}

		require.NoError(t, err, "failed to initialize test contexts: %v", err)
		sc[i] = tc
	}
	return sc
}

func TestStateComparisons(t *testing.T) {
	var testContexts = make(stateComparisonTestContexts, 3)

	// test commit tx
	t.Run("state-compare-commit-tx", func(t *testing.T) {
		testContexts = testContexts.Init(t, GasLimit)
		for i := 0; i < 3; i++ {
			tx1 := testContexts[i].signers.signTx(1, 21000, big.NewInt(0), big.NewInt(1),
				testContexts[i].signers.addresses[2], big.NewInt(0), []byte{})
			var (
				receipt *types.Receipt
				status  int
				err     error
			)
			switch i {
			case Baseline:
				receipt, status, err = testContexts[i].envDiff.commitTx(tx1, testContexts[i].chainData)
				testContexts[i].envDiff.applyToBaseEnv()

			case SingleSnapshot:
				require.NoError(t, testContexts[i].changes.env.state.NewMultiTxSnapshot(), "can't create multi tx snapshot: %v", err)
				receipt, status, err = testContexts[i].changes.commitTx(tx1, testContexts[i].chainData)
				require.NoError(t, err, "can't commit single snapshot tx")

				err = testContexts[i].changes.apply()
			case MultiSnapshot:
				require.NoError(t, testContexts[i].changes.env.state.NewMultiTxSnapshot(), "can't create multi tx snapshot: %v", err)
				receipt, status, err = testContexts[i].changes.commitTx(tx1, testContexts[i].chainData)
				require.NoError(t, err, "can't commit multi snapshot tx")

				err = testContexts[i].changes.apply()
			}
			require.NoError(t, err, "can't commit tx")
			require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
			require.Equal(t, 21000, int(receipt.GasUsed))
			require.Equal(t, shiftTx, status)
		}

		testContexts.UpdateRootHashes(t)
		testContexts.ValidateTestCases(t, Baseline)
		testContexts.ValidateRootHashes(t, testContexts[Baseline].rootHash)
	})

	// test bundle
	t.Run("state-compare-bundle", func(t *testing.T) {
		testContexts = testContexts.Init(t, GasLimit)
		for i, tc := range testContexts {
			var (
				signers = tc.signers
				header  = tc.env.header
				env     = tc.env
				chData  = tc.chainData
			)

			tx1 := signers.signTx(1, 21000, big.NewInt(0), big.NewInt(1), signers.addresses[2], big.NewInt(0), []byte{})
			tx2 := signers.signTx(1, 21000, big.NewInt(0), big.NewInt(1), signers.addresses[2], big.NewInt(0), []byte{})

			mevBundle := types.MevBundle{
				Txs:         types.Transactions{tx1, tx2},
				BlockNumber: header.Number,
			}

			simBundle, err := simulateBundle(env, mevBundle, chData, nil)
			require.NoError(t, err, "can't simulate bundle: %v", err)

			switch i {
			case Baseline:
				err = tc.envDiff.commitBundle(&simBundle, chData, nil, defaultAlgorithmConfig)
				if err != nil {
					break
				}
				tc.envDiff.applyToBaseEnv()

			case SingleSnapshot:
				err = tc.changes.env.state.NewMultiTxSnapshot()
				require.NoError(t, err, "can't create multi tx snapshot: %v", err)

				err = tc.changes.commitBundle(&simBundle, chData, defaultAlgorithmConfig)
				if err != nil {
					break
				}

				err = tc.changes.apply()

			case MultiSnapshot:
				err = tc.changes.env.state.NewMultiTxSnapshot()
				require.NoError(t, err, "can't create multi tx snapshot: %v", err)

				err = tc.changes.commitBundle(&simBundle, chData, defaultAlgorithmConfig)
				if err != nil {
					break
				}

				err = tc.changes.apply()
			}

			require.NoError(t, err, "can't commit bundle: %v", err)
		}

		testContexts.UpdateRootHashes(t)
		testContexts.ValidateTestCases(t, 0)
		testContexts.ValidateRootHashes(t, testContexts[Baseline].rootHash)
	})

	// test failed transactions
	t.Run("state-compare-failed-txs", func(t *testing.T) {
		// generate 100 transactions, with 50% of them failing
		var (
			txCount    = 100
			failEveryN = 2
		)
		testContexts = testContexts.Init(t, GasLimit)
		testContexts.GenerateTransactions(t, txCount, failEveryN)
		require.Len(t, testContexts[Baseline].transactions, txCount)

		for txIdx := 0; txIdx < txCount; txIdx++ {
			for ctxIdx, tc := range testContexts {
				tx := tc.transactions[txIdx]

				var commitErr error
				switch ctxIdx {
				case Baseline:
					_, _, commitErr = tc.envDiff.commitTx(tx, tc.chainData)
					tc.envDiff.applyToBaseEnv()

				case SingleSnapshot:
					err := tc.changes.env.state.NewMultiTxSnapshot()
					require.NoError(t, err, "can't create multi tx snapshot for tx %d: %v", txIdx, err)

					_, _, commitErr = tc.changes.commitTx(tx, tc.chainData)
					require.NoError(t, tc.changes.apply())
				case MultiSnapshot:
					err := tc.changes.env.state.NewMultiTxSnapshot()
					require.NoError(t, err,
						"can't create multi tx snapshot: %v", err)

					err = tc.changes.env.state.NewMultiTxSnapshot()
					require.NoError(t, err,
						"can't create multi tx snapshot: %v", err)

					_, _, commitErr = tc.changes.commitTx(tx, tc.chainData)
					require.NoError(t, tc.changes.apply())

					// NOTE(wazzymandias): At the time of writing this, the changes struct does not reset after performing
					// an apply - because the intended use of the changes struct is to create it and discard it
					// after every commit->(discard||apply) loop.
					// So for now to test multiple snapshots we apply the changes for the top of the stack and
					// then pop the underlying state snapshot from the base of the stack.
					// Otherwise, if changes are applied twice, then there can be double counting of transactions.
					require.NoError(t, tc.changes.env.state.MultiTxSnapshotCommit())
				}

				if txIdx%failEveryN == 0 {
					require.Errorf(t, commitErr, "tx %d should fail", txIdx)
				} else {
					require.NoError(t, commitErr, "tx %d should succeed, found: %v", txIdx, commitErr)
				}
			}
		}
		testContexts.UpdateRootHashes(t)
		testContexts.ValidateTestCases(t, 0)
		testContexts.ValidateRootHashes(t, testContexts[Baseline].rootHash)
	})
}

func TestBundles(t *testing.T) {
	const maxGasLimit = 1_000_000_000_000

	var testContexts = make(stateComparisonTestContexts, 3)
	testContexts.Init(t, maxGasLimit)

	// Set up FuzzTest ABI and bytecode
	abi, err := StatefuzztestMetaData.GetAbi()
	require.NoError(t, err)

	fuzzTestSolBytecode := StatefuzztestMetaData.Bin
	bytecodeBytes, err := hex.DecodeString(fuzzTestSolBytecode[2:])
	require.NoError(t, err)

	// FuzzTest constructor
	deployData, err := abi.Pack("")
	require.NoError(t, err)

	simulations := make([]*backends.SimulatedBackend, 3)
	controlFuzzTestContracts := make(map[int][]*Statefuzztest, 3)
	variantFuzzTestAddresses := make(map[int][]common.Address, 3)

	for tcIdx, tc := range testContexts {
		disk := tc.env.state.Copy().Database().DiskDB()
		db := rawdb.NewDatabase(disk)

		backend := backends.NewSimulatedBackendChain(db, tc.chainData.chain)
		simulations[tcIdx] = backend

		s := tc.signers
		controlFuzzTestContracts[tcIdx] = make([]*Statefuzztest, len(s.signers))
		variantFuzzTestAddresses[tcIdx] = make([]common.Address, len(s.signers))
		// commit transaction for deploying Fuzz Test contract
		for i, pk := range s.signers {
			deployTx := &types.LegacyTx{
				Nonce:    s.nonces[i],
				GasPrice: big.NewInt(1),
				Gas:      10_000_000,
				Value:    big.NewInt(0),
				To:       nil,
				Data:     append(bytecodeBytes, deployData...),
			}

			signTx := types.MustSignNewTx(pk, types.LatestSigner(s.config), deployTx)

			auth, err := bind.NewKeyedTransactorWithChainID(pk, tc.chainData.chainConfig.ChainID)
			require.NoError(t, err)

			// deploy Fuzz Test contract to control chain (i.e, the chain we compare the test contexts against)
			_, _, fuzz, err := DeployStatefuzztest(auth, backend)
			require.NoError(t, err)
			backend.Commit()

			controlFuzzTestContracts[tcIdx][i] = fuzz

			var receipt *types.Receipt
			switch tcIdx {
			case Baseline:
				receipt, _, err = tc.envDiff.commitTx(signTx, tc.chainData)
				require.NoError(t, err)
				tc.envDiff.applyToBaseEnv()

				_, err = tc.envDiff.baseEnvironment.state.Commit(true)
			case SingleSnapshot:
				err = tc.env.state.NewMultiTxSnapshot()
				require.NoError(t, err)

				receipt, _, err = tc.changes.commitTx(signTx, tc.chainData)
				require.NoError(t, err)

				err = tc.changes.apply()
				require.NoError(t, err)

				_, err = tc.changes.env.state.Commit(true)

			case MultiSnapshot:
				err = tc.env.state.NewMultiTxSnapshot()
				require.NoError(t, err)

				receipt, _, err = tc.changes.commitTx(signTx, tc.chainData)
				require.NoError(t, err)

				err = tc.changes.apply()
				require.NoError(t, err)

				_, err = tc.changes.env.state.Commit(true)
			}

			require.NoError(t, err)
			require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
			variantFuzzTestAddresses[tcIdx][i] = receipt.ContractAddress

			s.nonces[i]++
		}
	}
	testContexts.UpdateRootHashes(t)
	testContexts.ValidateTestCases(t, Baseline)
	testContexts.ValidateRootHashes(t, testContexts[Baseline].rootHash)

	// initialize fuzz test contract for each account with random objects through createObject function
	const createObjectCount = 100
	var randCreateObjectKeys = [createObjectCount][32]byte{}
	var randCreateObjectValues = [createObjectCount][32]byte{}
	for i := 0; i < createObjectCount; i++ {
		_, err := rand.Read(randCreateObjectKeys[i][:])
		require.NoError(t, err)

		_, err = rand.Read(randCreateObjectValues[i][:])
		require.NoError(t, err)
	}

	for tcIdx, tc := range testContexts {
		backend := simulations[tcIdx]

		t.Run(fmt.Sprintf("%s-create-object", tc.Name), func(t *testing.T) {
			signers := tc.signers
			for signerIdx, pk := range signers.signers {
				var (
					actualTransactions   = [createObjectCount]*types.Transaction{}
					expectedTransactions = [createObjectCount]*types.Transaction{}
					expectedReceipts     = [createObjectCount]*types.Receipt{}
					to                   = variantFuzzTestAddresses[tcIdx][signerIdx]
				)
				auth, err := bind.NewKeyedTransactorWithChainID(pk, tc.chainData.chainConfig.ChainID)
				require.NoError(t, err)

				for txIdx := 0; txIdx < createObjectCount; txIdx++ {
					var (
						createObjKey   = randCreateObjectKeys[txIdx]
						createObjValue = randCreateObjectValues[txIdx]
					)
					tx, err := createObjectFuzzTestContract(
						tc.chainData.chainConfig.ChainID, signers.nonces[signerIdx], to, createObjKey, createObjValue[:])
					require.NoError(t, err)

					actualTx := types.MustSignNewTx(pk, types.LatestSigner(signers.config), tx)
					actualTransactions[txIdx] = actualTx

					expectedTx, err :=
						controlFuzzTestContracts[tcIdx][signerIdx].CreateObject(auth, createObjKey, createObjValue[:])
					require.NoError(t, err)

					expectedTransactions[txIdx] = expectedTx

					require.Equal(t, expectedTx.Data(), actualTx.Data())
					require.Equal(t, expectedTx.Nonce(), actualTx.Nonce())
					require.Equal(t, expectedTx.To().String(), actualTx.To().String())

					// commit transaction for control chain (i.e, what we compare the test contexts against)
					backend.Commit()
					expectedReceipt, err := backend.TransactionReceipt(context.Background(), expectedTransactions[txIdx].Hash())
					require.NoError(t, err)
					require.Equal(t, types.ReceiptStatusSuccessful, expectedReceipt.Status)

					expectedReceipts[txIdx] = expectedReceipt

					// update nonce
					signers.nonces[signerIdx]++
				}

				for txIdx := 0; txIdx < createObjectCount; txIdx++ {
					actualTx := actualTransactions[txIdx]
					var actualReceipt *types.Receipt
					switch tcIdx {
					case Baseline:
						actualReceipt, _, err = tc.envDiff.commitTx(actualTx, tc.chainData)
						tc.envDiff.applyToBaseEnv()
					case SingleSnapshot:
						err = tc.env.state.NewMultiTxSnapshot()
						require.NoError(t, err)

						actualReceipt, _, err = tc.changes.commitTx(actualTx, tc.chainData)
						require.NoError(t, err)

						err = tc.changes.apply()
					case MultiSnapshot:
						err = tc.env.state.NewMultiTxSnapshot()
						require.NoError(t, err)

						err = tc.env.state.NewMultiTxSnapshot()
						require.NoError(t, err)

						actualReceipt, _, err = tc.changes.commitTx(actualTx, tc.chainData)
						require.NoError(t, err)

						err = tc.changes.apply()
						require.NoError(t, err)

						err = tc.env.state.MultiTxSnapshotCommit()
					}

					require.NoError(t, err)

					expectedReceipt := expectedReceipts[txIdx]
					require.Equal(t, expectedReceipt.PostState, actualReceipt.PostState)
					require.Equal(t, expectedReceipt.ContractAddress.String(), actualReceipt.ContractAddress.String())
					require.Equal(t, types.ReceiptStatusSuccessful, actualReceipt.Status, "test %s, signer %d", tc.Name, signerIdx)
				}
			}
		})
	}
	testContexts.UpdateRootHashes(t)
	testContexts.ValidateTestCases(t, Baseline)
	testContexts.ValidateRootHashes(t, testContexts[Baseline].rootHash)

	// generate bundles of transactions, where each transaction will either:
	//   - change balance
	//   - create object
	//   - self-destruct
	//   - reset object
	//   - change storage
	type TransactionOperation int
	const (
		ChangeBalance TransactionOperation = iota
		CreateObject
		SelfDestruct
		ResetObject
		ChangeStorage
	)
	const (
		bundleCount = 5
		bundleSize  = 10
	)

	bundles := [bundleCount]types.MevBundle{}
	for bundleIdx := 0; bundleIdx < bundleCount; bundleIdx++ {
		transactions := [bundleSize]*types.Transaction{}
		for txIdx := 0; txIdx < bundleSize; txIdx++ {
			var (
				// pick a random integer that represents one of the transactions we will create
				n       = mathrand.Intn(5)
				s       = testContexts[0].signers
				chainID = s.config.ChainID
				// choose a random To Address index
				toAddressRandomIdx = mathrand.Intn(len(s.signers))
				// reference the correct nonce for the associated To Address
				nonce     = s.nonces[toAddressRandomIdx]
				toAddress = s.addresses[toAddressRandomIdx]

				txData types.TxData
				err    error
			)
			switch TransactionOperation(n) {
			case ChangeBalance: // change balance
				balanceAddressRandomIdx := mathrand.Intn(len(s.signers))
				balanceAddress := s.addresses[balanceAddressRandomIdx]

				randomBalance := new(big.Int).SetUint64(mathrand.Uint64())

				txData, err = changeBalanceFuzzTestContract(nonce, toAddress, balanceAddress, randomBalance)

			case CreateObject: // create object
				var (
					key   [32]byte
					value [32]byte
				)
				_, err = rand.Read(key[:])
				require.NoError(t, err)

				_, err = rand.Read(value[:])
				require.NoError(t, err)

				txData, err = createObjectFuzzTestContract(chainID, nonce, toAddress, key, value[:])

			case SelfDestruct: // self-destruct
				txData, err = selfDestructFuzzTestContract(chainID, nonce, toAddress)

			case ResetObject: // reset object
				var (
					resetObjectRandomIdx = mathrand.Intn(createObjectCount)
					resetObjectKey       = randCreateObjectKeys[resetObjectRandomIdx]
					fuzzContractAddress  = variantFuzzTestAddresses[0][toAddressRandomIdx]
				)
				txData, err = resetObjectFuzzTestContract(nonce, fuzzContractAddress, resetObjectKey)

			case ChangeStorage: // change storage
				var (
					changeStorageRandomIdx = mathrand.Intn(createObjectCount)
					changeStorageObjectKey = randCreateObjectKeys[changeStorageRandomIdx]
					fuzzContractAddress    = variantFuzzTestAddresses[0][toAddressRandomIdx]
					value                  [32]byte
				)
				_, err = rand.Read(value[:])
				require.NoError(t, err)

				txData, err = changeStorageFuzzTestContract(chainID, nonce, fuzzContractAddress, changeStorageObjectKey, value[:])
			}
			require.NoError(t, err)

			tx := types.MustSignNewTx(s.signers[toAddressRandomIdx], types.LatestSigner(s.config), txData)
			transactions[txIdx] = tx
			s.nonces[toAddressRandomIdx]++
		}

		bundles[bundleIdx] = types.MevBundle{
			Txs: transactions[:],
		}
	}
	for tcIdx, tc := range testContexts {
		algoConf := defaultAlgorithmConfig
		algoConf.EnforceProfit = true
		switch tcIdx {
		case SingleSnapshot, MultiSnapshot:
			err = tc.env.state.NewMultiTxSnapshot()
			require.NoError(t, err)
		}

		for _, b := range bundles {
			sim, err := simulateBundle(tc.env, b, tc.chainData, nil)

			switch tcIdx {
			case Baseline:
				err = tc.envDiff.commitBundle(&sim, tc.chainData, nil, algoConf)
			case SingleSnapshot:
				err = tc.changes.commitBundle(&sim, tc.chainData, algoConf)
			case MultiSnapshot:
				err = tc.changes.commitBundle(&sim, tc.chainData, algoConf)
			}
			var pe *lowProfitError
			if errors.As(err, &pe) || (err != nil && err.Error() == "bundle mev gas price is nil") {
				continue
			} else {
				require.NoError(t, err, "test %s", tc.Name)
			}
		}

		switch tcIdx {
		case Baseline:
			tc.envDiff.applyToBaseEnv()
		case SingleSnapshot:
			err = tc.changes.apply()
			require.NoError(t, err)
		case MultiSnapshot:
			err = tc.changes.apply()
			require.NoError(t, err)
		}
	}
	testContexts.UpdateRootHashes(t)
	testContexts.ValidateTestCases(t, Baseline)
	testContexts.ValidateRootHashes(t, testContexts[Baseline].rootHash)
}
