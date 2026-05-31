package backends

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	ethchannel "github.com/perun-network/perun-eth-backend/channel"
	"github.com/perun-network/perun-eth-backend/wallet/simple"
	swallet "github.com/perun-network/perun-eth-backend/wallet/simple"
)

const adjudicatorGasLimit = uint64(1000000)

func newETHCoordinator(w *swallet.Wallet, cfg ETHBackendConfig, eacc accounts.Account) (*ethchannel.Coordinator, error) {
	ethClient, err := ethclient.Dial(cfg.ChainURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Ethereum node: %w", err)
	}

	cb := ethchannel.NewContractBackend(
		ethClient,
		ethchannel.MakeChainID(big.NewInt(int64(cfg.LedgerID))),
		simple.NewTransactor(w, types.NewLondonSigner(big.NewInt(int64(cfg.LedgerID)))),
		1, // txFinalityDepth
	)

	ethCoordinator := ethchannel.NewCoordinator(cb, common.HexToAddress(cfg.AdjudicatorAddr), eacc.Address, eacc, adjudicatorGasLimit)
	return ethCoordinator, nil
}
