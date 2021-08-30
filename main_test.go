package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/rpctest"
)

func orFatal(t *testing.T, err error, fmt string, args ...interface{}) {
	t.Helper()
	if err == nil {
		return
	}
	t.Fatalf(fmt, args...)
}

func TestSPVReorg(t *testing.T) {
	ctxb := context.Background()
	rpctest.SetPathToDCRD("dcrd")
	netParams := chaincfg.SimNetParams()

	os.RemoveAll("miner1")
	os.RemoveAll("miner2")

	// Create 2 miners and join them.

	args := []string{
		"--logdir=miner1",
		"--debuglevel=debug",
	}
	miner1, err := rpctest.New(t, netParams, nil, args)
	orFatal(t, err, "unable to create harness: %v", err)
	err = miner1.SetUp(true, 0)
	orFatal(t, err, "unable to setup harness: %v", err)

	_, err = miner1.Node.Generate(ctxb, 8)
	orFatal(t, err, "unable to generate: %v", err)

	args[0] = "--logdir=miner2"
	miner2, err := rpctest.New(t, netParams, nil, args)
	orFatal(t, err, "unable to create harness 2: %v", err)
	err = miner2.SetUp(false, 0)
	orFatal(t, err, "unable to setup harness: %v", err)

	err = rpctest.ConnectNode(miner1, miner2)
	orFatal(t, err, "unable to connect miners: %v", err)
	err = rpctest.JoinNodes([]*rpctest.Harness{miner1, miner2}, rpctest.Blocks)
	orFatal(t, err, "unable to join miners: %v", err)

	// Generate a single block to see what that looks like from miner1 pov.
	time.Sleep(time.Second)
	_, err = miner2.Node.Generate(ctxb, 1)
	orFatal(t, err, "unable to generate: %v", err)
	time.Sleep(time.Second)

	t.Log("Miners created")

	// Miners are now at the same height. Create an SPV wallet connected to
	// miner1.

	spvAddrs := []string{miner1.P2PAddress()}
	wallet, cleanup := NewSPVWallet(t, spvAddrs)
	defer cleanup()

	assertWalletSynced(t, wallet, miner1)
	assertWalletSynced(t, wallet, miner2)

	t.Log("Wallet created and synced")

	// Cause a reorg: disconnect miners, mine different blocks on each,
	// reconnect and join them.

	err = rpctest.RemoveNode(ctxb, miner1, miner2)
	orFatal(t, err, "unable to disconnect miners: %v", err)

	_, err = miner1.Node.Generate(ctxb, 2)
	orFatal(t, err, "unable to generate: %v", err)
	_, err = miner2.Node.Generate(ctxb, 3)
	orFatal(t, err, "unable to generate: %v", err)

	assertWalletSynced(t, wallet, miner1)

	err = rpctest.ConnectNode(miner1, miner2)
	orFatal(t, err, "unable to connect miners: %v", err)
	err = rpctest.JoinNodes([]*rpctest.Harness{miner1, miner2}, rpctest.Blocks)
	orFatal(t, err, "unable to join miners: %v", err)

	t.Log("Caused reorg")

	// Both nodes now should be at the same tip. Wallet should also be on
	// the same tip as both.

	assertWalletSynced(t, wallet, miner1)
	assertWalletSynced(t, wallet, miner2)

	t.Log("Success!")
}

func TestSPVReorg2(t *testing.T) {
	ctxb := context.Background()
	rpctest.SetPathToDCRD("dcrd")
	netParams := chaincfg.SimNetParams()

	os.RemoveAll("miner1")
	os.RemoveAll("miner2")

	// Create 2 miners and join them.

	args := []string{
		"--logdir=miner1",
		"--debuglevel=debug",
	}
	miner1, err := rpctest.New(t, netParams, nil, args)
	orFatal(t, err, "unable to create harness: %v", err)
	err = miner1.SetUp(true, 0)
	orFatal(t, err, "unable to setup harness: %v", err)

	_, err = miner1.Node.Generate(ctxb, 8)
	orFatal(t, err, "unable to generate: %v", err)

	args[0] = "--logdir=miner2"
	miner2, err := rpctest.New(t, netParams, nil, args)
	orFatal(t, err, "unable to create harness 2: %v", err)
	err = miner2.SetUp(false, 0)
	orFatal(t, err, "unable to setup harness: %v", err)

	err = rpctest.ConnectNode(miner1, miner2)
	orFatal(t, err, "unable to connect miners: %v", err)
	err = rpctest.JoinNodes([]*rpctest.Harness{miner1, miner2}, rpctest.Blocks)
	orFatal(t, err, "unable to join miners: %v", err)

	// Generate a single block to see what that looks like from miner1 pov.
	time.Sleep(time.Second)
	_, err = miner2.Node.Generate(ctxb, 1)
	orFatal(t, err, "unable to generate: %v", err)
	time.Sleep(time.Second)

	t.Log("Miners created")

	// Miners are now at the same height. Create an SPV wallet connected to
	// both miners.

	spvAddrs := []string{miner1.P2PAddress(), miner2.P2PAddress()}
	wallet, cleanup := NewSPVWallet(t, spvAddrs)
	defer cleanup()

	assertWalletSynced(t, wallet, miner1)
	assertWalletSynced(t, wallet, miner2)

	t.Log("Wallet created and synced")

	// Disconnect miners so they mine different chains.

	err = rpctest.RemoveNode(ctxb, miner1, miner2)
	orFatal(t, err, "unable to disconnect miners: %v", err)
	time.Sleep(time.Second)

	// Now cause a reorg in the wallet.

	// m1: b - b1
	// m2:  \- b1' - b2'
	_, err = miner1.Node.Generate(ctxb, 1)
	orFatal(t, err, "unable to generate: %v", err)
	assertWalletSynced(t, wallet, miner1)

	_, err = miner2.Node.Generate(ctxb, 2)
	orFatal(t, err, "unable to generate: %v", err)
	assertWalletSynced(t, wallet, miner2)

	time.Sleep(time.Second)

	for i := 0; i < 10; i++ {
		_, err = miner1.Node.Generate(ctxb, 1)
		orFatal(t, err, "unable to generate: %v", err)
		time.Sleep(time.Millisecond * 10)
	}
	assertWalletSynced(t, wallet, miner1)

	time.Sleep(time.Second)

	for i := 0; i < 12; i++ {
		_, err = miner2.Node.Generate(ctxb, 1)
		orFatal(t, err, "unable to generate: %v", err)
		time.Sleep(time.Millisecond * 10)
	}
	assertWalletSynced(t, wallet, miner2)

	t.Log("Success!")
}
