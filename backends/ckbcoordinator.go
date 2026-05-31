package backends

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"os"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	ckbaddress "github.com/nervosnetwork/ckb-sdk-go/v2/address"
	ckbrpc "github.com/nervosnetwork/ckb-sdk-go/v2/rpc"
	ckbtypes "github.com/nervosnetwork/ckb-sdk-go/v2/types"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
	"perun.network/go-perun/wallet"

	ckbbackend "perun.network/perun-ckb-backend/backend"
	ckbasset "perun.network/perun-ckb-backend/channel/asset"
	ckbcoordinator "perun.network/perun-ckb-backend/channel/coordinator"
	ckbclient "perun.network/perun-ckb-backend/client"
	ckbwallet "perun.network/perun-ckb-backend/wallet"
)

// newCKBCoordinator wires a perun-ckb-backend coordinator (the CoordinatorSubscriber
// implementation that observes RegisteredEvent/CoordinatedEvent and dispatches
// on-chain coordinate transactions) using the shared coordinator ECDSA key.
//
// CKB always uses the fixed LedgerBackendID asset.CCID{backendID=3, ledgerID="03"} —
// there is no per-chain LedgerID variability (every CKB chain shares the same
// go-perun ledger identifier). Network (mainnet/testnet/devnet) only affects
// CKB address formats and the signer's network discriminator.
func newCKBCoordinator(signingKey *ecdsa.PrivateKey, cfg CKBBackendConfig) (multi.LedgerBackendID, channel.CoordinatorSubscriber, wallet.Account, error) {
	deployment, err := loadCKBDeployment(cfg.DeploymentFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ckb coordinator: %w", err)
	}

	network, err := parseCKBNetwork(cfg.Network)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ckb coordinator: %w", err)
	}

	signerAddr, err := ckbaddress.Decode(cfg.SignerAddress)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ckb coordinator: decode signer_address %q: %w", cfg.SignerAddress, err)
	}

	// Convert ECDSA → decred secp256k1 key. Both libraries use the same curve;
	// the byte representation is interchangeable. This lets the operator
	// configure a single ECDSA key for both ETH and CKB coordinators.
	secpPriv := secp256k1.PrivKeyFromBytes(signingKey.D.Bytes())

	var signer ckbbackend.Signer
	if cfg.UseEVMSigner {
		var authContent [20]byte
		copy(authContent[:], ethcrypto.PubkeyToAddress(signingKey.PublicKey).Bytes())
		signer = ckbbackend.NewEVMSignerInstance(*signerAddr, *secpPriv, network, authContent)
	} else {
		signer = ckbbackend.NewSignerInstance(*signerAddr, *secpPriv, network)
	}

	rpcClient, err := ckbrpc.Dial(cfg.RPCURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ckb coordinator: dial %s: %w", cfg.RPCURL, err)
	}

	client, err := ckbclient.NewClient(rpcClient, signer, *deployment)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ckb coordinator: new client: %w", err)
	}

	coord := ckbcoordinator.NewCoordinator(client)
	acc := ckbwallet.NewAccountFromPrivateKey(secpPriv, deployment.DefaultLockScript.CodeHash, !cfg.UseEVMSigner)

	return ckbasset.MakeCCID(ckbasset.MakeContractID("03")), coord, acc, nil
}

func loadCKBDeployment(path string) (*ckbbackend.Deployment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read deployment_file %s: %w", path, err)
	}
	var d ckbbackend.Deployment
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse deployment_file %s: %w", path, err)
	}
	return &d, nil
}

func parseCKBNetwork(s string) (ckbtypes.Network, error) {
	switch s {
	case "mainnet":
		return ckbtypes.NetworkMain, nil
	case "testnet", "devnet":
		// CKB devnet uses the testnet address prefix and network discriminator;
		// the underlying ckb-sdk-go types.Network enum only has Main/Test.
		return ckbtypes.NetworkTest, nil
	default:
		return 0, fmt.Errorf("unknown network %q", s)
	}
}
