package main

import (
	"bytes"
	"fmt"
	"runtime/debug"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcutil"
)

type lndTestCase func(net *networkHarness, t *testing.T)

func assertTxInBlock(block *btcutil.Block, txid *wire.ShaHash, t *testing.T) {
	for _, tx := range block.Transactions() {
		if bytes.Equal(txid[:], tx.Sha()[:]) {
			return
		}
	}

	t.Fatalf("funding tx was not included in block")
}

// getChannelHelpers returns a series of helper functions as closures which may
// be useful within tests to execute common activities such as synchronously
// waiting for channels to open/close.
func getChannelHelpers(ctxb context.Context, net *networkHarness,
	t *testing.T) (func(*lightningNode, *lightningNode, btcutil.Amount) *lnrpc.ChannelPoint,
	func(*lightningNode, *lnrpc.ChannelPoint)) {

	openChannel := func(alice *lightningNode, bob *lightningNode, amount btcutil.Amount) *lnrpc.ChannelPoint {
		chanOpenUpdate, err := net.OpenChannel(ctxb, alice, bob, amount, 1)
		if err != nil {
			t.Fatalf("unable to open channel: %v", err)
		}

		// Mine a block, then wait for Alice's node to notify us that the
		// channel has been opened. The funding transaction should be found
		// within the newly mined block.
		blockHash, err := net.Miner.Node.Generate(1)
		if err != nil {
			t.Fatalf("unable to generate block: %v", err)
		}
		block, err := net.Miner.Node.GetBlock(blockHash[0])
		if err != nil {
			t.Fatalf("unable to get block: %v", err)
		}
		fundingChanPoint, err := net.WaitForChannelOpen(chanOpenUpdate)
		if err != nil {
			t.Fatalf("error while waiting for channel open: %v", err)
		}
		fundingTxID, err := wire.NewShaHash(fundingChanPoint.FundingTxid)
		if err != nil {
			t.Fatalf("unable to create sha hash: %v", err)
		}
		assertTxInBlock(block, fundingTxID, t)

		// The channel should be listed in the peer information returned by
		// both peers.
		chanPoint := wire.OutPoint{
			Hash:  *fundingTxID,
			Index: fundingChanPoint.OutputIndex,
		}
		err = net.AssertChannelExists(ctxb, alice, &chanPoint)
		if err != nil {
			t.Fatalf("unable to assert channel existence: %v", err)
		}

		return fundingChanPoint
	}

	closeChannel := func(node *lightningNode, fundingChanPoint *lnrpc.ChannelPoint) {
		closeUpdates, err := net.CloseChannel(ctxb, node, fundingChanPoint, false)
		if err != nil {
			t.Fatalf("unable to close channel: %v", err)
		}

		// Finally, generate a single block, wait for the final close status
		// update, then ensure that the closing transaction was included in the
		// block.
		blockHash, err := net.Miner.Node.Generate(1)
		if err != nil {
			t.Fatalf("unable to generate block: %v", err)
		}
		block, err := net.Miner.Node.GetBlock(blockHash[0])
		if err != nil {
			t.Fatalf("unable to get block: %v", err)
		}

		closingTxid, err := net.WaitForChannelClose(closeUpdates)
		if err != nil {
			t.Fatalf("error while waiting for channel close: %v", err)
		}
		assertTxInBlock(block, closingTxid, t)

	}

	return openChannel, closeChannel
}

// testBasicChannelFunding performs a test exercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itself is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(net *networkHarness, t *testing.T) {
	ctxb := context.Background()
	openChannel, closeChannel := getChannelHelpers(ctxb, net, t)

	chanAmt := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	chanPoint := openChannel(net.Alice, net.Bob, chanAmt)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	closeChannel(net.Alice, chanPoint)
}

// testChannelBalance creates a new channel between Alice and  Bob, then
// checks channel balance to be equal amount specified while creation of channel.
func testChannelBalance(net *networkHarness, t *testing.T) {
	ctxb := context.Background()
	openChannel, closeChannel := getChannelHelpers(ctxb, net, t)

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node lnrpc.LightningClient, amount btcutil.Amount) {
		response, err := node.ChannelBalance(ctxb, &lnrpc.ChannelBalanceRequest{})
		if err != nil {
			t.Fatalf("unable to get channel balance: %v", err)
		}

		balance := btcutil.Amount(response.Balance)
		if balance != amount {
			t.Fatalf("channel balance wrong: %v != %v", balance, amount)
		}
	}

	// Open a channel with 0.5 BTC between Alice and Bob, ensuring the
	// channel has been opened properly.
	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
	chanPoint := openChannel(net.Alice, net.Bob, amount)

	// As this is a single funder channel, Alice's balance should be
	// exactly 0.5 BTC since now state transitions have taken place yet.
	checkChannelBalance(net.Alice, amount)

	// Since we only explicitly wait for Alice's channel open notification,
	// Bob might not yet have updated his internal state in response to
	// Alice's channel open proof. So we sleep here for a second to let Bob
	// catch up.
	// TODO(roasbeef): Bob should also watch for the channel on-chain after
	// the changes to restrict the number of pending channels are in.
	time.Sleep(time.Second)

	// Ensure Bob currently has no available balance within the channel.
	checkChannelBalance(net.Bob, 0)

	// Finally close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	closeChannel(net.Alice, chanPoint)
}

// testChannelForceClosure performs a test to exercise the behavior of "force"
// closing a channel or unilaterally broadcasting the latest local commitment
// state on-chain. The test creates a new channel between Alice and Bob, then
// force closes the channel after some cursory assertions. Within the test, two
// transactions should be broadcast on-chain, the commitment transaction itself
// (which closes the channel), and the sweep transaction a few blocks later
// once the output(s) become mature.
//
// TODO(roabeef): also add an unsettled HTLC before force closing.
func testChannelForceClosure(net *networkHarness, t *testing.T) {
	ctxb := context.Background()

	// First establish a channel ween with a capacity of 100k satoshis
	// between Alice and Bob.
	numFundingConfs := uint32(1)
	chanAmt := btcutil.Amount(10e4)
	chanOpenUpdate, err := net.OpenChannel(ctxb, net.Alice, net.Bob,
		chanAmt, numFundingConfs)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}
	if _, err := net.Miner.Node.Generate(numFundingConfs); err != nil {
		t.Fatalf("unable to mine block: %v", err)
	}
	chanPoint, err := net.WaitForChannelOpen(chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel to open: %v", err)
	}

	// Now that the channel is open, immediately execute a force closure of
	// the channel. This will also assert that the commitment transaction
	// was immediately broadcast in order to fulfill the force closure
	// request.
	closeUpdate, err := net.CloseChannel(ctxb, net.Alice, chanPoint, true)
	if err != nil {
		t.Fatalf("unable to execute force channel closure: %v", err)
	}

	// Mine a block which should confirm the commitment transaction
	// broadcast as a result of the force closure.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	closingTxID, err := net.WaitForChannelClose(closeUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}

	// Currently within the codebase, the default CSV is 4 relative blocks.
	// So generate exactly 4 new blocks.
	// TODO(roasbeef): should check default value in config here instead,
	// or make delay a param
	const defaultCSV = 4
	if _, err := net.Miner.Node.Generate(defaultCSV); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the sweeping transaction should now be broadcast. So
	// we fetch the node's mempool to ensure it has been properly
	// broadcast.
	var sweepingTXID *wire.ShaHash
	var mempool []*wire.ShaHash
mempoolPoll:
	for {
		select {
		case <-time.After(time.Second * 5):
			t.Fatalf("sweep tx not found in mempool")
		default:
			mempool, err = net.Miner.Node.GetRawMempool()
			if err != nil {
				t.Fatalf("unable to fetch node's mempool: %v", err)
			}
			if len(mempool) == 0 {
				continue
			}
			break mempoolPoll
		}
	}

	// There should be exactly one transaction within the mempool at this
	// point.
	// TODO(roasbeef): assertion may not necessarily hold with concurrent
	// test executions
	if len(mempool) != 1 {
		t.Fatalf("node's mempool is wrong size, expected 1 got %v",
			len(mempool))
	}
	sweepingTXID = mempool[0]

	// Fetch the sweep transaction, all input it's spending should be from
	// the commitment transaction which was broadcast on-chain.
	sweepTx, err := net.Miner.Node.GetRawTransaction(sweepingTXID)
	if err != nil {
		t.Fatalf("unable to fetch sweep tx: %v", err)
	}
	for _, txIn := range sweepTx.MsgTx().TxIn {
		if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
			t.Fatalf("sweep transaction not spending from commit "+
				"tx %v, instead spending %v",
				closingTxID, txIn.PreviousOutPoint)
		}
	}

	// Finally, we mine an additional block which should include the sweep
	// transaction as the input scripts and the sequence locks on the
	// inputs should be properly met.
	blockHash, err := net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}
	assertTxInBlock(block, sweepTx.Sha(), t)
}

var lndTestCases = map[string]lndTestCase{
	"basic funding flow":    testBasicChannelFunding,
	"channel force closure": testChannelForceClosure,
	"channel balance":       testChannelBalance,
}

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programmatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	var (
		btcdHarness      *rpctest.Harness
		lightningNetwork *networkHarness
		currentTest      string
		err              error
	)

	defer func() {
		// If one of the integration tests caused a panic within the main
		// goroutine, then tear down all the harnesses in order to avoid
		// any leaked processes.
		if r := recover(); r != nil {
			fmt.Println("recovering from test panic: ", r)
			if err := btcdHarness.TearDown(); err != nil {
				fmt.Println("unable to tear btcd harnesses: ", err)
			}
			if err := lightningNetwork.TearDownAll(); err != nil {
				fmt.Println("unable to tear lnd harnesses: ", err)
			}
			t.Fatalf("test %v panicked: %s", currentTest, debug.Stack())
		}
	}()

	// First create the network harness to gain access to its
	// 'OnTxAccepted' call back.
	lightningNetwork, err = newNetworkHarness()
	if err != nil {
		t.Fatalf("unable to create lightning network harness: %v", err)
	}
	defer lightningNetwork.TearDownAll()

	handlers := &btcrpcclient.NotificationHandlers{
		OnTxAccepted: lightningNetwork.OnTxAccepted,
	}

	// First create an instance of the btcd's rpctest.Harness. This will be
	// used to fund the wallets of the nodes within the test network and to
	// drive blockchain related events within the network.
	btcdHarness, err = rpctest.New(harnessNetParams, handlers, nil)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	defer btcdHarness.TearDown()
	if err = btcdHarness.SetUp(true, 50); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}
	if err := btcdHarness.Node.NotifyNewTransactions(false); err != nil {
		t.Fatalf("unable to request transaction notifications: %v", err)
	}

	// With the btcd harness created, we can now complete the
	// initialization of the network. args - list of lnd arguments,
	// example: "--debuglevel=debug"
	args := []string{}
	if err := lightningNetwork.InitializeSeedNodes(btcdHarness, args); err != nil {
		t.Fatalf("unable to initialize seed nodes: %v", err)
	}
	if err = lightningNetwork.SetUp(); err != nil {
		t.Fatalf("unable to set up test lightning network: %v", err)
	}

	t.Logf("Running %v integration tests", len(lndTestCases))
	for testName, lnTest := range lndTestCases {
		t.Logf("Executing test %v", testName)

		currentTest = testName
		lnTest(lightningNetwork, t)
	}
}
