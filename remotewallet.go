package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	pb "decred.org/dcrwallet/v2/rpc/walletrpc"
	"github.com/decred/dcrd/rpctest"
	"github.com/decred/dcrlnd/lntest/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
)

func tlsCertFromFile(fname string) (*x509.CertPool, error) {
	b, err := ioutil.ReadFile(fname)
	if err != nil {
		return nil, err
	}
	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("credentials: failed to append certificates")
	}

	return cp, nil
}

func consumeSyncMsgs(syncStream pb.WalletLoaderService_SpvSyncClient, onSyncedChan chan struct{}) {
	for {
		msg, err := syncStream.Recv()
		if err != nil {
			// All errors are final here.
			return
		}
		if msg.Synced {
			onSyncedChan <- struct{}{}
			return
		}
	}
}

func NewSPVWallet(t testing.TB, spvAddrs []string) (*grpc.ClientConn, func()) {

	dataDir := "dcrwallet-data"
	dcrwalletExe := "dcrwallet"
	nodeName := "wallet1"
	hdSeed := []byte{31: 0xff}
	privatePass := []byte("pass")
	tlsCertPath := path.Join(dataDir, "rpc.cert")
	tlsKeyPath := path.Join(dataDir, "rpc.key")
	logFilePath := path.Join(dataDir, "out.log")

	os.RemoveAll(dataDir)
	os.Mkdir(dataDir, 0o0777)

	// Setup the args to run the underlying dcrwallet.
	port := 35555
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	args := []string{
		"--noinitialload",
		"--debuglevel=debug",
		"--simnet",
		"--nolegacyrpc",
		"--grpclisten=" + addr,
		"--appdata=" + dataDir,
		"--tlscurve=P-256",
		"--rpccert=" + tlsCertPath,
		"--rpckey=" + tlsKeyPath,
		"--clientcafile=" + tlsCertPath,
	}

	logFile, err := os.Create(logFilePath)
	if err != nil {
		t.Logf("Wallet dir: %s", dataDir)
		t.Fatalf("Unable to create dcrwallet log file: %v",
			err)
	}

	// Run dcrwallet.
	cmd := exec.Command(dcrwalletExe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	err = cmd.Start()
	if err != nil {
		t.Logf("Wallet dir: %s", dataDir)
		t.Fatalf("Unable to start dcrwallet: %v", err)
	}

	// Read the wallet TLS cert and client cert and key files.
	var caCert *x509.CertPool
	var clientCert tls.Certificate
	err = wait.NoError(func() error {
		var err error
		caCert, err = tlsCertFromFile(tlsCertPath)
		if err != nil {
			return fmt.Errorf("unable to load wallet ca cert: %v", err)
		}

		clientCert, err = tls.LoadX509KeyPair(tlsCertPath, tlsKeyPath)
		if err != nil {
			return fmt.Errorf("unable to load wallet cert and key files: %v", err)
		}

		return nil
	}, time.Second*30)
	if err != nil {
		t.Logf("Wallet dir: %s", dataDir)
		t.Fatalf("Unable to read ca cert file: %v", err)
	}

	// Setup the TLS config and credentials.
	tlsCfg := &tls.Config{
		ServerName:   "localhost",
		RootCAs:      caCert,
		Certificates: []tls.Certificate{clientCert},
	}
	creds := credentials.NewTLS(tlsCfg)

	opts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithTransportCredentials(creds),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  time.Millisecond * 20,
				Multiplier: 1,
				Jitter:     0.2,
				MaxDelay:   time.Millisecond * 20,
			},
			MinConnectTimeout: time.Millisecond * 20,
		}),
	}
	ctxb := context.Background()
	ctx, cancel := context.WithTimeout(ctxb, time.Second*30)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, opts...)
	if err != nil {
		t.Logf("Wallet dir: %s", dataDir)
		t.Fatalf("Unable to dial grpc: %v", err)
	}

	loader := pb.NewWalletLoaderServiceClient(conn)

	// Create the wallet.
	reqCreate := &pb.CreateWalletRequest{
		Seed:              hdSeed,
		PublicPassphrase:  privatePass,
		PrivatePassphrase: privatePass,
	}
	ctx, cancel = context.WithTimeout(ctxb, time.Second*30)
	defer cancel()
	_, err = loader.CreateWallet(ctx, reqCreate)
	if err != nil {
		t.Logf("Wallet dir: %s", dataDir)
		t.Fatalf("unable to create wallet: %v", err)
	}

	ctxSync, cancelSync := context.WithCancel(context.Background())
	// Run the spv syncer.
	req := &pb.SpvSyncRequest{
		SpvConnect:        spvAddrs,
		DiscoverAccounts:  true,
		PrivatePassphrase: privatePass,
	}
	syncStream, err := loader.SpvSync(ctxSync, req)
	if err != nil {
		cancelSync()
		t.Fatalf("error running rpc sync: %v", err)
	}

	// Wait for the wallet to sync.
	onSyncedChan := make(chan struct{})
	go consumeSyncMsgs(syncStream, onSyncedChan)
	select {
	case <-onSyncedChan:
		// Sync done.
	case <-time.After(time.Second * 60):
		cancelSync()
		t.Fatalf("timeout waiting for initial sync to complete")
	}

	cleanup := func() {
		cancelSync()

		if cmd.ProcessState != nil {
			return
		}

		if t.Failed() {
			t.Logf("Wallet data at %s", dataDir)
		}

		err := cmd.Process.Signal(os.Interrupt)
		if err != nil {
			t.Errorf("Error sending SIGINT to %s dcrwallet: %v",
				nodeName, err)
			return
		}

		// Wait for dcrwallet to exit or force kill it after a timeout.
		// For this, we run the wait on a goroutine and signal once it
		// has returned.
		errChan := make(chan error)
		go func() {
			errChan <- cmd.Wait()
		}()

		select {
		case err := <-errChan:
			if err != nil {
				t.Errorf("%s dcrwallet exited with an error: %v",
					nodeName, err)
			}

		case <-time.After(time.Second * 15):
			t.Errorf("%s dcrwallet timed out after SIGINT", nodeName)
			err := cmd.Process.Kill()
			if err != nil {
				t.Errorf("Error killing %s dcrwallet: %v",
					nodeName, err)
			}
		}
	}

	return conn, cleanup
}

func assertWalletSynced(t testing.TB, walletConn *grpc.ClientConn, miner *rpctest.Harness) {
	t.Helper()
	ctxb := context.Background()

	wallet := pb.NewWalletServiceClient(walletConn)

	err := wait.NoError(func() error {
		walletBlock, err := wallet.BestBlock(ctxb, &pb.BestBlockRequest{})
		if err != nil {
			return err
		}

		minerBlockHash, minerBlockHeight, err := miner.Node.GetBestBlock(ctxb)
		if err != nil {
			return err
		}

		if walletBlock.Height != uint32(minerBlockHeight) {
			return fmt.Errorf("unexpected block height -- got %d, want %d",
				walletBlock.Height, minerBlockHeight)
		}

		if !bytes.Equal(walletBlock.Hash, minerBlockHash[:]) {
			return fmt.Errorf("unexpected block hash -- got %s, want %s",
				walletBlock.Hash, minerBlockHash)
		}

		return nil
	}, time.Second*10)

	if err != nil {
		t.Fatalf("wallet not synced: %v", err)
	}
}
