package backends

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"
	ethchannel "github.com/perun-network/perun-eth-backend/channel"
	ethwallet "github.com/perun-network/perun-eth-backend/wallet"
	swallet "github.com/perun-network/perun-eth-backend/wallet/simple"
	"perun.network/go-perun/channel/multi"
	"perun.network/go-perun/wallet"
)

// SetupMultiCoordinator builds a multi.Coordinator that fans out coordinate()
// dispatches to one per-chain backend coordinator. The returned account map is
// keyed by wallet.BackendID — one entry per distinct backend type present in
// the config (ETH=1, CKB=3). Both backends sign Keccak256 of the same
// channel.State encoding with the same secp256k1 key, so coordSigs produced
// for one backend are byte-identical to those produced for the other.
func SetupMultiCoordinator(key *ecdsa.PrivateKey, coordinators []BackendCoordinatorConfig) (*multi.Coordinator, map[wallet.BackendID]wallet.Account, error) {
	eWallet := swallet.NewWallet(key)
	eacc := accounts.Account{Address: crypto.PubkeyToAddress(key.PublicKey)}
	ethAddr := ethwallet.AsWalletAddr(eacc.Address)

	coordAcc := make(map[wallet.BackendID]wallet.Account)
	coords := multi.NewCoordinator()

	for i, coordCfg := range coordinators {
		switch coordCfg.Type {
		case "eth":
			if coordCfg.ETH == nil {
				return nil, nil, fmt.Errorf("setup multi coordinator: coordinators[%d]: type=eth missing eth block", i)
			}
			ethAccount, err := eWallet.Unlock(ethAddr)
			if err != nil {
				return nil, nil, fmt.Errorf("setup multi coordinator: unlock ETH account %s: %w", ethAddr, err)
			}
			coordAcc[1] = ethAccount

			ethLedgerID := ethchannel.MakeLedgerBackendID(big.NewInt(int64(coordCfg.ETH.LedgerID)))
			coord, err := newETHCoordinator(eWallet, *coordCfg.ETH, eacc)
			if err != nil {
				return nil, nil, fmt.Errorf("setup multi coordinator: create ETH coordinator: %w", err)
			}
			coords.RegisterCoordinator(ethLedgerID, coord)

		case "ckb":
			if coordCfg.CKB == nil {
				return nil, nil, fmt.Errorf("setup multi coordinator: coordinators[%d]: type=ckb missing ckb block", i)
			}
			ledgerID, coord, ckbAcc, err := newCKBCoordinator(key, *coordCfg.CKB)
			if err != nil {
				return nil, nil, fmt.Errorf("setup multi coordinator: create CKB coordinator: %w", err)
			}
			coordAcc[3] = ckbAcc
			coords.RegisterCoordinator(ledgerID, coord)

		default:
			return nil, nil, fmt.Errorf("setup multi coordinator: coordinators[%d]: unknown type %q", i, coordCfg.Type)
		}
	}

	return coords, coordAcc, nil
}
