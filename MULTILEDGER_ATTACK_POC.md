# CKB ↔ ETH Multi-Ledger Channel PoC — Handover

This document is the handover for the standalone PoC repository (`multiledger-poc/`)
that demonstrates **secure cross-chain virtual channels between Nervos CKB and
Ethereum ETH, built over CKB-ETH parent multi-ledger channels**, and shows how
the [`cross-chain-coordinator`](.) service prevents divergent settlement on
either layer.

Two end-to-end scenarios are implemented:

1. **Sub-channel over a CKB-ETH parent** — the simplest topology that exposes the
   divergent-settlement attack and the coordinator's protection guarantee. Two
   participants, one parent multi-ledger channel, one nested sub-channel.
2. **Virtual channel via an Ingrid intermediary** — the end-goal Perun topology.
   Alice ↔ Ingrid and Ingrid ↔ Bob run CKB-ETH parent multi-ledger channels;
   an Alice ↔ Bob virtual channel sits on top, mediated by Ingrid. Its safety
   derives entirely from the coordinator's uniform settlement of each parent.

Both scenarios run against a local Hardhat node (ETH) and a local `ckb run`
devnet (CKB). The coordinator is constructed in-process via `service.New(...)`
from the sibling [`cross-chain-coordinator`](.) repo.

---

## Table of Contents

1. [Background](#1-background)
2. [Attack model](#2-attack-model)
3. [Coordinator protection model](#3-coordinator-protection-model)
4. [Prerequisites](#4-prerequisites)
5. [Project layout](#5-project-layout)
6. [Chain setup](#6-chain-setup)
7. [Running the coordinator service](#7-running-the-coordinator-service)
8. [Contract reference](#8-contract-reference)
9. [Go module setup](#9-go-module-setup)
10. [PoC implementation — sub-channel](#10-poc-implementation--sub-channel)
11. [PoC implementation — virtual channel](#11-poc-implementation--virtual-channel)
12. [Running the PoC](#12-running-the-poc)
13. [Known limitations](#13-known-limitations)
14. [Key invariants](#14-key-invariants)

---

## 1. Background

A **multi-ledger channel** is a Perun payment channel whose assets live on two
heterogeneous chains. In this PoC: ETH on Ethereum (via Hardhat) and CKByte on
Nervos CKB (via local `ckb run`). Each chain hosts its own adjudicator
(`Adjudicator.sol` for ETH, the Perun Channel Type Script `PCTS` for CKB) and
maintains its own dispute window. The two adjudicators have no direct
communication path — that independence is what the divergent-settlement
attack exploits.

```
Chain A: Ethereum (Hardhat, chainID 1337)        Chain B: Nervos CKB (devnet)
─────────────────────────────────────────       ─────────────────────────────
Adjudicator.sol  AssetHolderETH.sol              PCTS  PCLS  PFLS  +  CKByte cells
      │  Asset1 (ETH on Ethereum)                       │  Asset2 (CKByte on CKB)
      └────────── multi-ledger channel (logical, Perun) ──────────┘
                          Alice ←─────────────→ Bob
```

A **sub-channel** is a Perun channel whose funds are locked inside a parent
channel's `Locked` allocation. It settles purely off-chain unless the parent
disputes, in which case the sub-channel's terminal state is honoured during
parent settlement.

A **virtual channel** is a Perun construction where two parties (Alice, Bob)
exchange state via an intermediary (Ingrid) without an on-chain Alice ↔ Bob
parent — instead the virtual channel hangs off the Alice ↔ Ingrid and
Ingrid ↔ Bob parents. Safety reduces to the safety of those two parents.

---

## 2. Attack model

### Balance setup (ETH and CKByte, asset 1 = ETH on chain A, asset 2 = CKByte on chain B)

| Version                                 | Alice A1 | Bob A1 | Alice A2 | Bob A2 | Bob total |
| --------------------------------------- | -------- | ------ | -------- | ------ | --------- |
| v0 (init)                               | 8 ETH    | 2 ETH  | 2 CKB·k  | 8 CKB·k| 10        |
| v1 (agreed)                             | 5        | 5      | 3        | 7      | 12        |
| v2 (Bob's secret)                       | 1        | 9      | 5        | 5      | 14        |
| **Attack outcome** (v2 on ETH, v1 on CKB)| **1**   | **9**  | **3**    | **7**  | **16**    |

(`CKB·k` denotes thousands of CKBytes — see §10.1 for the
`asset.CKByteToShannon` conversion used in code.)

Bob profits **+4 units** above the honest v1 baseline by registering different
versions on each chain.

### Steps

```
t0  Alice & Bob agree on v1 off-chain. Bob secretly retains both signatures for
    a fabricated v2 (Alice never sees it); Alice's local machine stays at v1.
t1  Bob registers v1 on CKB.                  (CKB dispute window opens at v1.)
t2  CKB challenge window expires.             (v1 frozen on CKB.)
t3  Bob registers v2 on ETH.                  (Ethereum had no prior register → v2 accepted.)
t4  ETH challenge window expires.             (v2 frozen on ETH.)
t5  Bob withdraws v2 on ETH → 9 ETH.
    Bob withdraws v1 on CKB → 7 CKB·k.
    Alice receives 1 ETH + 3 CKB·k instead of 5 ETH + 3 CKB·k.
```

Each chain's `registerSingle` only checks that the new version exceeds whatever
is registered **on that chain**. There is no global synchronisation forcing a
single canonical version across chains.

The virtual-channel variant of the attack is identical at the parent layer
(Bob diverges parent B's settlement), but the profit gets extracted from the
virtual channel's locked balance during parent settlement — see §11.

---

## 3. Coordinator protection model

A trusted third-party (TTP) coordinator `C` is encoded into the channel
parameters at open time via `client.WithCoordinator(...)`. The on-chain phase
lifecycle becomes:

```
register()   ──► DISPUTE      (any participant, per chain, challenge window opens)
[timeout]    ──► FROZEN       (window expires; registerSingle rejects new states)
coordinate() ──► COORDINATED  (coordinator only, after dispute timeout,
                               selects the highest-version canonical state)
conclude()   ──► CONCLUDED    (any participant, funds released)
```

What the coordinator does:

1. Subscribe to on-chain events for the channel on **every** registered chain.
2. Wait until each chain delivers a `RegisteredEvent` whose timeout has elapsed.
3. Pick the **highest version** seen across all chains as the canonical state.
4. Call `multi.Coordinator.Coordinate(canonical)` — fans out concurrently to
   every chain via `errgroup`.
5. Each chain's `coordinateSingle` (or `coordinate` cell on CKB) accepts the
   canonical version (≥ its stored version) and transitions to `COORDINATED`.
   Once `COORDINATED`, `register` rejects further state submission.

**Guarantee:** both chains lock to the same version before any withdrawal. If
Bob registered v2 on ETH and v1 on CKB, the coordinator coordinates v2 on both
chains so the payout is uniform.

For a sub-channel: the coordinator collects the sub-channel's signed states
during parent coordination and includes them in the `Coordinate` call, so the
sub-channel settles uniformly too.

For a virtual channel: the coordinator runs over **both** parent channels (Alice
↔ Ingrid and Ingrid ↔ Bob) independently. The virtual channel's terminal state
is enforced through each parent's `Locked` allocation; uniform parent
settlement on every chain preserves the virtual channel's balance.

---

## 4. Prerequisites

| Tool           | Version         | Purpose                                                    |
| -------------- | --------------- | ---------------------------------------------------------- |
| Go             | ≥ 1.24          | PoC implementation                                         |
| Node.js        | ≥ 18            | Hardhat                                                    |
| Hardhat        | ≥ 2.22          | Local Ethereum chain                                       |
| solc           | ≥ 0.8.15        | Compile contracts (`pragma solidity ^0.8.15`)              |
| `ckb`          | ≥ 0.115         | Local CKB devnet                                           |
| `ckb-cli`      | ≥ 1.5           | Account management for CKB                                 |
| `capsule`      | ≥ 0.10          | Deploy perun-ckb scripts (`PCTS`, `PCLS`, `PFLS`, …)       |

Go dependencies (per-module versions in §9):

```
github.com/perun-network/perun-eth-backend       # Ethereum adjudicator/funder/wallet
perun.network/perun-ckb-backend                  # CKB adjudicator/funder/wallet/coordinator (coordination branch)
perun.network/go-perun                            # framework — fork pinned via replace
github.com/ethereum/go-ethereum v1.17.2           # ethclient, accounts
github.com/nervosnetwork/ckb-sdk-go/v2            # CKB RPC client (perun-network fork)
github.com/decred/dcrd/dcrec/secp256k1/v4         # shared secp256k1 key type
github.com/libp2p/go-libp2p                       # coordinator peer.ID
github.com/miguelmota/go-ethereum-hdwallet v0.1.1 # Hardhat mnemonic derivation
github.com/stretchr/testify v1.11.1               # assertions
polycry.pt/poly-go                                # sync primitives
```

The coordinator-enabled `Adjudicator.sol` lives in
`perun-eth-backend/bindings/contracts/`. The Perun CKB scripts live as
pre-compiled binaries under `perun-ckb-backend/system_scripts/` and are
deployed via the recipe in `perun-ckb-backend/devnet/`.

---

## 5. Project layout

```
multiledger-poc/                     ← this PoC repository
├── hardhat/
│   ├── hardhat.config.js
│   ├── contracts/                   copy from perun-eth-backend/bindings/contracts/
│   ├── scripts/deploy.js
│   └── addresses.json               written by deploy.js (git-ignored)
│
├── ckb-devnet/
│   ├── ckb.toml                     dev chain config
│   ├── deployment.json              written by capsule + post-processing (git-ignored)
│   └── system_scripts.json          mirror of perun-ckb-backend/system_scripts/default_scripts.json
│
├── poc/
│   ├── helpers.go                   ETHChainConfig, CKBChainConfig, LoadChains, AdvanceTime, key derivation
│   ├── participant.go               Participant type wiring ETH + CKB into multi.Adjudicator/Funder
│   ├── coordinator.go               StartCoordinator(t, ethChain, ckbChain) using service.New
│   ├── attack_sub_test.go           TestAttackNoCoordinator / TestAttackWithCoordinator (sub-channel)
│   └── attack_virtual_test.go       TestVirtualAttackNoCoordinator / TestVirtualAttackWithCoordinator
│
├── go.mod
└── go.sum

cross-chain-coordinator/             ← sibling repo (this directory)
                                      coordinator service constructed in-process by StartCoordinator
```

---

## 6. Chain setup

### 6.1 Hardhat (Ethereum chain)

`hardhat/hardhat.config.js`:

```javascript
require("@nomicfoundation/hardhat-toolbox");

module.exports = {
  solidity: "0.8.26",
  networks: {
    eth: {
      url: "http://127.0.0.1:8545",
      chainId: 1337,
      accounts: { mnemonic: "test test test test test test test test test test test junk", count: 5 },
    },
  },
};
```

Account indices: `[0]` deployer, `[1]` Alice, `[2]` Bob, `[3]` Charlie
(coordinator), `[4]` Ingrid (used by the virtual-channel scenario only).

```bash
npx hardhat node --port 8545
# exposes ws://127.0.0.1:8545  (coordinator + tests dial via ws://)
```

`hardhat/scripts/deploy.js` deploys `Adjudicator` + `AssetHolderETH`, then
appends `{adjudicator, assetHolder}` under key `"eth"` in `hardhat/addresses.json`.

### 6.2 CKB devnet

`ckb-devnet/ckb.toml` — a standard `ckb init -c dev --ba-arg <addr>` config,
with the Perun script cells pre-funded via `ckb-cli wallet transfer`. The
perun-ckb-backend repo's `devnet/deploy_contracts.sh` automates this:

```bash
# Inside the perun-ckb-backend checkout, on the coordination branch:
ckb run -C ckb-devnet/ &                                              # start node, listens on 127.0.0.1:8114
./devnet/deploy_contracts.sh ./ckb-devnet/migrations ./system_scripts # deploys PCTS, PCLS, PFLS, VCTS, VCLS
# Post-process the migration into a single deployment JSON:
go run ./cmd/get_address  > ./ckb-devnet/deployment.json   # or copy the JSON from channel/test/GetDeployment
```

Result: `ckb-devnet/deployment.json` is the `backend.Deployment` JSON the
coordinator service consumes via `CKBBackendConfig.DeploymentFile`. The
participants in the PoC also load it (for their `Funder` and `Adjudicator`).

### 6.3 Address book (`hardhat/addresses.json` + `ckb-devnet/deployment.json`)

`hardhat/addresses.json`:

```json
{ "eth": { "adjudicator": "0x...", "assetHolder": "0x..." } }
```

`ckb-devnet/deployment.json` follows the `backend.Deployment` JSON tags
(see `perun-ckb-backend/backend/deployment.go`).

### 6.4 Advancing time

Ethereum: `evm_increaseTime` + `evm_mine` via JSON-RPC (helper below).

CKB: **there is no `evm_increaseTime` analog.** CKB dispute timeouts are
measured in block height and progress only as the node produces blocks.
Strategies:

- Use a short `challengeDuration` (e.g. 30 blocks at the devnet's
  `BlockInterval ≈ 500 ms` ⇒ 15 s wall-clock).
- Optionally use `generate_epochs` RPC to bump epoch number — but the
  dispute timer is `block.timestamp`-based, which also advances naturally
  during devnet mining.

```go
// poc/helpers.go — ETH only.
func AdvanceTime(ctx context.Context, rpcURL string, seconds uint64) error {
    c, err := rpc.DialContext(ctx, rpcURL)
    if err != nil { return err }
    defer c.Close()
    if err := c.CallContext(ctx, nil, "evm_increaseTime", hexutil.EncodeUint64(seconds)); err != nil {
        return fmt.Errorf("evm_increaseTime: %w", err)
    }
    return c.CallContext(ctx, nil, "evm_mine")
}

// CKB: simply wait the wall-clock challengeDuration.
func WaitCKBChallenge(challengeSeconds uint64) {
    time.Sleep(time.Duration(challengeSeconds+1) * time.Second)
}
```

---

## 7. Running the coordinator service

The coordinator is the sibling repo `cross-chain-coordinator/`. The PoC's
defended tests **construct it in-process** via `service.New(...)`; only the
ETH and CKB chains need to be running externally. The CLI in
`cross-chain-coordinator/main.go` is for production deployments.

### 7.1 Programmatic API (`cross-chain-coordinator/service`)

```go
package service

// New wires every backend coordinator and starts the libp2p relay host.
//   coordinators: per-chain config (tagged-union; "eth" or "ckb" per entry).
//   signingKey:   ECDSA key signing coordinator certificates on every chain.
//                 Both ETH and CKB sign Keccak256 of the same channel.State,
//                 so one key suffices for both backends.
//   libp2pKey:    stable identity key for this coordinator's peer.ID.
func New(
    coordinators []backends.BackendCoordinatorConfig,
    signingKey   *ecdsa.PrivateKey,
    libp2pKey    libp2pcrypto.PrivKey,
) (*Service, error)

type Service struct { *coordinator.CoordinatorHost }
func (s *Service) PeerID() peer.ID                  // dial target for RelayCoordinatorNotifier
func (s *Service) Close() error                     // libp2p shutdown
func (s *Service) Wait(timeout time.Duration) error // drain in-flight coordinate() calls
```

Internally `New`:

1. `backends.SetupMultiCoordinator` walks the `coordinators` slice and, for each
   entry, calls `newETHCoordinator` or `newCKBCoordinator` based on `Type`.
   Each backend coordinator is registered into a single `*multi.Coordinator`.
2. `coordinator.SetupRelayCoordinator` brings up a libp2p host with no listen
   addresses, dials the Perun relay (`relay.perun.network:5574`, peer ID
   `QmcxeYpYpYPX4J3478YZUaxFytYfUDbNe1jUWVYeZjL3gY`), reserves a slot (renewed
   every 4 min), and registers three stream handlers
   (`/coordinator/notify-watch-{ledger,sub,stop}/1.0.0`).
3. `svc.PeerID()` is the dial target every participant's
   `RelayCoordinatorNotifier` uses.

### 7.2 Tagged-union config shape

```go
type BackendCoordinatorConfig struct {
    Type string            // "eth" | "ckb"
    ETH  *ETHBackendConfig // populated when Type == "eth"
    CKB  *CKBBackendConfig // populated when Type == "ckb"
}

type ETHBackendConfig struct {
    LedgerID        uint64 // chain ID (1337)
    ChainURL        string // ws:// or wss://
    AdjudicatorAddr string // deployed Adjudicator address
}

type CKBBackendConfig struct {
    RPCURL         string // http:// or https:// — CKB JSON-RPC
    DeploymentFile string // path to JSON-serialised backend.Deployment
    Network        string // "mainnet" | "testnet" | "devnet"
    SignerAddress  string // operator's CKB address (ckb1... / ckt1...)
    UseEVMSigner   bool   // true → omni-lock EVM auth; false → secp256k1_blake160
}
```

Validation rules (`backends.Config.Validate`):

- `private_key_path` non-empty
- exactly one of `eth:`/`ckb:` per entry, matching `type`
- ETH: `chain_url` uses `ws://` or `wss://`; `adjudicator_addr` non-empty;
  unique per `ledger_id`
- CKB: `rpc_url` uses `http://` or `https://`; `deployment_file` non-empty;
  `signer_address` non-empty; `network ∈ {mainnet, testnet, devnet}`; at most
  one CKB entry (CKB's `LedgerBackendID` is fixed at `asset.CCID{3, "03"}`)

### 7.3 Production CLI (optional)

For deployments outside the PoC, the CLI in `cross-chain-coordinator/main.go`
loads the same config from YAML and stays alive until SIGINT:

```bash
cd cross-chain-coordinator
go run . -mode keygen -keyfile sign_private.key                            # one-time libp2p key
openssl rand -hex 32 > coord_ecdsa.key                                     # one-time ECDSA key
go run . -mode relay -keyfile sign_private.key -config devnet_config.yaml  # logs the peer.ID at startup
```

YAML example mirroring the tagged-union shape:

```yaml
private_key_path: ./coord_ecdsa.key
coordinators:
  - type: eth
    eth:
      ledger_id: 1337
      chain_url: "ws://127.0.0.1:8545"
      adjudicator_addr: "0xDEADBEEF..."
  - type: ckb
    ckb:
      rpc_url: "http://127.0.0.1:8114"
      deployment_file: "./ckb_deployment.json"
      network: "devnet"
      signer_address: "ckt1qyqd0rfh3..."
      use_evm_signer: true
```

---

## 8. Contract reference

### 8.1 Ethereum (Adjudicator.sol)

```solidity
struct SignedState { Channel.Params params; Channel.State state; bytes[] sigs; }

function register(SignedState memory channel, SignedState[] memory subChannels) external;
function coordinate(SignedState memory channel, SignedState[] memory subChannels, bytes[] memory coordSigs) external;
function conclude(Channel.Params memory params, Channel.State memory state, Channel.State[] memory subStates) external;
function concludeFinal(Channel.Params memory params, Channel.State memory state, bytes[] memory sigs) external;
```

`Channel.Params` includes `address coordinator` (zero if no coordinator).
Multi-ledger eligibility (`MultiLedger.sol`): coordination required when
`coordinator != address(0)` AND the state has assets on more than one chain.

Key revert reasons:

| Check                                                  | Revert message                       |
| ------------------------------------------------------ | ------------------------------------ |
| `coordinate()` requires prior `register()`             | `"not registered"`                   |
| `coordinate()` requires `block.timestamp ≥ timeout`    | `"refutation timeout not passed"`    |
| `coordinate()` requires valid coordinator ECDSA sig    | `"invalid coordinator signature"`    |
| `coordinate()` requires state version ≥ stored version | `"invalid version"`                  |
| `register()` in COORDINATED phase                       | `"incorrect phase"`                  |
| Multi-ledger `conclude()` without coordination          | `"coordinated settlement required"`  |

### 8.2 CKB (Perun script cells)

The CKB-side adjudicator is split across three script cells:

| Script | Role |
| --- | --- |
| **PCTS** (Perun Channel Type Script) | enforces the phase machine on the channel cell; rejects `register`/`coordinate`/`conclude` transactions that violate phase or version invariants |
| **PCLS** (Perun Channel Lock Script) | locks the channel cell; unlock requires either dispute window expiry or a valid `coordinate` / `concludeFinal` witness |
| **PFLS** (Perun Funds Lock Script) | locks the actual CKByte / SUDT funds cells; unlock requires the parent channel to be `CONCLUDED` |

(Virtual-channel variants `VCTS`, `VCLS` exist for the nested layer.)

The on-chain coordinate path verifies three signatures from the witness:
`SigA` (party A), `SigB` (party B), and `CoordSig` (coordinator's). The CKB
backend's `coordinator.Coordinate` builds the transaction; verification is
performed by the PCTS at consensus time.

Unlike ETH there is no symbolic revert message — failures surface as
transaction rejections. The most common ones:

| Layer | Cause |
| --- | --- |
| PCTS | dispute window not elapsed for the channel cell |
| PCTS | coordinator signature does not match the address baked into channel state |
| PCTS | coordinate version below the channel's currently-disputed version |
| PCLS | unlock witness fails signature verification |

---

## 9. Go module setup

```
module github.com/your-org/multiledger-poc

go 1.24

require (
    github.com/perun-network/perun-eth-backend      v0.0.0
    perun.network/perun-ckb-backend                 v0.0.0
    perun.network/go-perun                          v0.15.1-0.20260408121133-2daea3fa699a
    github.com/ethereum/go-ethereum                 v1.17.2
    github.com/nervosnetwork/ckb-sdk-go/v2          v2.2.0
    github.com/decred/dcrd/dcrec/secp256k1/v4       v4.4.0
    github.com/libp2p/go-libp2p                     v0.48.0
    github.com/miguelmota/go-ethereum-hdwallet      v0.1.1
    github.com/stretchr/testify                     v1.11.1
    polycry.pt/poly-go                              v0.0.0-20220301085937-fb9d71b45a37
)

replace (
    perun.network/go-perun                    => github.com/NhoxxKienn/go-perun                v0.0.0-20260526062537-a05990e2cb40
    github.com/perun-network/perun-eth-backend => github.com/NhoxxKienn/perun-eth-backend      v0.6.1-0.20260525091241-e1f6c19121e0
    perun.network/perun-ckb-backend           => github.com/NhoxxKienn/perun-ckb-backend      v0.0.0-20260531113006-5aa8111e501a
    github.com/nervosnetwork/ckb-sdk-go/v2    => github.com/perun-network/ckb-sdk-go/v2       v2.2.1-0.20260530044933-548463b5d86f
)
```

Versions **must match** `cross-chain-coordinator/go.mod`; otherwise the
`peer.ID` types or `RelayCoordinatorNotifier` constructor signature will not
line up.

---

## 10. PoC implementation — sub-channel

### 10.1 Chain helpers

```go
// poc/helpers.go
package poc

import (
    "context"
    "crypto/ecdsa"
    "encoding/json"
    "fmt"
    "math/big"
    "os"
    "time"

    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/common/hexutil"
    "github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/ethclient"
    "github.com/ethereum/go-ethereum/rpc"
    hdwallet "github.com/miguelmota/go-ethereum-hdwallet"

    ckbrpc "github.com/nervosnetwork/ckb-sdk-go/v2/rpc"
    ckbbackend "perun.network/perun-ckb-backend/backend"
)

const hardhatMnemonic = "test test test test test test test test test test test junk"

// ETHChainConfig describes one Ethereum chain handle.
type ETHChainConfig struct {
    RPC       string
    ChainID   *big.Int
    Client    *ethclient.Client
    AdjAddr   common.Address
    AssetAddr common.Address
}

// CKBChainConfig describes the CKB devnet handle and the deployed Perun scripts.
type CKBChainConfig struct {
    RPCURL        string
    Network       string // "devnet"
    DeploymentRaw []byte
    Deployment    ckbbackend.Deployment
    Client        ckbrpc.Client
}

func NewETHChainConfig(rpcURL string, chainID int64, adjAddr, assetAddr string) (*ETHChainConfig, error) {
    c, err := ethclient.Dial(rpcURL)
    if err != nil { return nil, err }
    return &ETHChainConfig{
        RPC: rpcURL, ChainID: big.NewInt(chainID), Client: c,
        AdjAddr: common.HexToAddress(adjAddr), AssetAddr: common.HexToAddress(assetAddr),
    }, nil
}

func NewCKBChainConfig(rpcURL, deploymentPath, network string) (*CKBChainConfig, error) {
    raw, err := os.ReadFile(deploymentPath)
    if err != nil { return nil, fmt.Errorf("read ckb deployment: %w", err) }
    var d ckbbackend.Deployment
    if err := json.Unmarshal(raw, &d); err != nil { return nil, fmt.Errorf("parse ckb deployment: %w", err) }
    c, err := ckbrpc.Dial(rpcURL)
    if err != nil { return nil, err }
    return &CKBChainConfig{
        RPCURL: rpcURL, Network: network,
        DeploymentRaw: raw, Deployment: d, Client: c,
    }, nil
}

func MakeSigner(chainID *big.Int) types.Signer { return types.LatestSignerForChainID(chainID) }

func AdvanceTime(ctx context.Context, rpcURL string, seconds uint64) error {
    c, err := rpc.DialContext(ctx, rpcURL)
    if err != nil { return err }
    defer c.Close()
    if err := c.CallContext(ctx, nil, "evm_increaseTime", hexutil.EncodeUint64(seconds)); err != nil {
        return fmt.Errorf("evm_increaseTime: %w", err)
    }
    return c.CallContext(ctx, nil, "evm_mine")
}

// WaitCKBChallenge sleeps real time — CKB has no evm_increaseTime analog.
func WaitCKBChallenge(challengeSeconds uint64) {
    time.Sleep(time.Duration(challengeSeconds+1) * time.Second)
}

func DeriveKey(index uint32) (*ecdsa.PrivateKey, error) {
    w, err := hdwallet.NewFromMnemonic(hardhatMnemonic)
    if err != nil { return nil, err }
    path := hdwallet.MustParseDerivationPath(fmt.Sprintf("m/44'/60'/0'/0/%d", index))
    acc, err := w.Derive(path, false)
    if err != nil { return nil, err }
    return w.PrivateKey(acc)
}

func LoadChains() (*ETHChainConfig, *CKBChainConfig, error) {
    var addrs struct {
        ETH struct{ Adjudicator, AssetHolder string } `json:"eth"`
    }
    raw, err := os.ReadFile("../hardhat/addresses.json")
    if err != nil { return nil, nil, fmt.Errorf("reading hardhat addresses: %w", err) }
    if err := json.Unmarshal(raw, &addrs); err != nil { return nil, nil, fmt.Errorf("parsing hardhat addresses: %w", err) }

    eth, err := NewETHChainConfig("http://127.0.0.1:8545", 1337, addrs.ETH.Adjudicator, addrs.ETH.AssetHolder)
    if err != nil { return nil, nil, err }

    ckb, err := NewCKBChainConfig("http://127.0.0.1:8114", "../ckb-devnet/deployment.json", "devnet")
    if err != nil { return nil, nil, err }

    return eth, ckb, nil
}
```

### 10.2 Participant setup — wiring ETH + CKB into multi.Adjudicator / multi.Funder

```go
// poc/participant.go
package poc

import (
    "crypto/ecdsa"
    "testing"

    "github.com/decred/dcrd/dcrec/secp256k1/v4"
    "github.com/ethereum/go-ethereum/accounts"
    ethcrypto "github.com/ethereum/go-ethereum/crypto"
    "github.com/libp2p/go-libp2p/core/peer"
    "github.com/stretchr/testify/require"

    ethchannel    "github.com/perun-network/perun-eth-backend/channel"
    ethwallet     "github.com/perun-network/perun-eth-backend/wallet"
    simplewallet  "github.com/perun-network/perun-eth-backend/wallet/simple"

    ckbaddress    "github.com/nervosnetwork/ckb-sdk-go/v2/address"
    ckbbackend    "perun.network/perun-ckb-backend/backend"
    ckbadj        "perun.network/perun-ckb-backend/channel/adjudicator"
    ckbasset      "perun.network/perun-ckb-backend/channel/asset"
    ckbfunder     "perun.network/perun-ckb-backend/channel/funder"
    ckbclient     "perun.network/perun-ckb-backend/client"
    ckbwallet     "perun.network/perun-ckb-backend/wallet"

    "perun.network/go-perun/channel"
    "perun.network/go-perun/channel/multi"
    "perun.network/go-perun/client"
    "perun.network/go-perun/wallet"
    "perun.network/go-perun/watcher/local"
    "perun.network/go-perun/wire"
    libp2pwire    "perun.network/go-perun/wire/net/libp2p"
    wiretest      "perun.network/go-perun/wire/test"
    "polycry.pt/poly-go/test"
)

const (
    ethBackendID      = wallet.BackendID(1) // ethwallet.BackendID
    ckbBackendID      = wallet.BackendID(3) // ckbasset.CKBBackendID
    gasLimit          = uint64(1_000_000)
    challengeDuration = uint64(15)
)

type Participant struct {
    Client     *client.Client
    WireAddr   map[wallet.BackendID]wire.Address
    WalletAddr map[wallet.BackendID]wallet.Address
    WalletAcc  map[wallet.BackendID]wallet.Account

    // Per-backend handles for raw register/withdraw in attack tests.
    EthAdj *ethchannel.Adjudicator
    CkbAdj *ckbadj.Adjudicator

    BalETH BalanceReader
    BalCKB BalanceReader
}

func (p *Participant) HandleAdjudicatorEvent(_ channel.AdjudicatorEvent) {}

type BalanceReader interface{ Balance() *big.Int }

// NewParticipant wires both backends. When coordPeerID is non-empty it installs
// a RelayCoordinatorNotifier so NotifyWatch* fires automatically. Pass "" for
// the no-coordinator attack tests.
func NewParticipant(
    t *testing.T,
    name string,
    key *ecdsa.PrivateKey,
    ckbAddr string, // operator-supplied CKB address (e.g. derived from `ckb-cli account list`)
    bus wire.Bus,
    eth *ETHChainConfig,
    ckb *CKBChainConfig,
    coordPeerID peer.ID,
) *Participant {
    t.Helper()
    rng := test.Prng(t)

    // ---------- ETH side ----------
    ethSWallet := simplewallet.NewWallet(key)
    ethAddrRaw := ethwallet.Address(ethcrypto.PubkeyToAddress(key.PublicKey))
    ethAcc, err := ethSWallet.Unlock(&ethAddrRaw)
    require.NoError(t, err)
    ethAccount := accounts.Account{Address: ethAddrRaw.Address}

    cbETH := ethchannel.NewContractBackend(eth.Client,
        ethchannel.MakeChainID(eth.ChainID),
        simplewallet.NewTransactor(ethSWallet, MakeSigner(eth.ChainID)), 1)
    ethAdj := ethchannel.NewAdjudicator(cbETH, eth.AdjAddr, ethAddrRaw.Address, ethAccount, gasLimit)

    ethAsset := ethchannel.NewAsset(eth.ChainID, eth.AssetAddr)
    ethFunder := ethchannel.NewFunder(cbETH)
    ethFunder.RegisterAsset(*ethAsset, ethchannel.NewETHDepositor(gasLimit), ethAccount)

    // ---------- CKB side ----------
    ckbSignerAddr, err := ckbaddress.Decode(ckbAddr)
    require.NoError(t, err)
    secpKey := secp256k1.PrivKeyFromBytes(key.D.Bytes())

    // Use EVMSigner so the same ECDSA key can sign for ETH + CKB.
    var authContent [20]byte
    copy(authContent[:], ethcrypto.PubkeyToAddress(key.PublicKey).Bytes())
    ckbSigner := ckbbackend.NewEVMSignerInstance(*ckbSignerAddr, *secpKey,
        ckbtypesFromNetwork(ckb.Network), authContent)

    ckbCli, err := ckbclient.NewClient(ckb.Client, ckbSigner, ckb.Deployment)
    require.NoError(t, err)
    ckbAdj := ckbadj.NewAdjudicator(ckbCli)
    ckbFunder := ckbfunder.NewDefaultFunder(ckbCli, ckb.Deployment)

    ckbAcc := ckbwallet.NewAccountFromPrivateKey(secpKey,
        ckb.Deployment.DefaultLockScript.CodeHash, /*defaultScript=*/ false)

    // ---------- multi.Adjudicator + multi.Funder ----------
    mAdj := multi.NewAdjudicator()
    mAdj.RegisterAdjudicator(ethchannel.MakeLedgerBackendID(eth.ChainID), ethAdj)
    mAdj.RegisterAdjudicator(ckbasset.MakeCCID(ckbasset.MakeContractID("03")), ckbAdj)

    mFund := multi.NewFunder()
    mFund.RegisterFunder(ethchannel.MakeLedgerBackendID(eth.ChainID), ethFunder)
    mFund.RegisterFunder(ckbasset.MakeCCID(ckbasset.MakeContractID("03")), ckbFunder)

    watcher, err := local.NewWatcher(mAdj)
    require.NoError(t, err)

    wireAddr := wiretest.NewRandomAddressesMap(rng, 1)
    perunWallets := map[wallet.BackendID]wallet.Wallet{
        ethBackendID: ethSWallet,
        ckbBackendID: ckbwallet.NewWallet(secpKey, ckb.Deployment.DefaultLockScript.CodeHash, false),
    }
    c, err := client.New(wireAddr[0], bus, mFund, mAdj, perunWallets, watcher)
    require.NoError(t, err)

    if coordPeerID != "" {
        libp2pAcc, err := libp2pwire.NewAccount(key)
        require.NoError(t, err)
        c.EnableCoordinationNotifier(libp2pwire.NewRelayCoordinatorNotifier(libp2pAcc, coordPeerID))
    }

    return &Participant{
        Client: c, WireAddr: wireAddr[0],
        WalletAddr: map[wallet.BackendID]wallet.Address{
            ethBackendID: &ethAddrRaw,
            ckbBackendID: ckbAcc.Address(),
        },
        WalletAcc: map[wallet.BackendID]wallet.Account{
            ethBackendID: ethAcc,
            ckbBackendID: ckbAcc,
        },
        EthAdj: ethAdj, CkbAdj: ckbAdj,
        BalETH: newETHBalanceReader(eth.Client, ethAddrRaw.Address),
        BalCKB: newCKBBalanceReader(ckb.Client, *ckbSignerAddr),
    }
}
```

### 10.3 `StartCoordinator(t, eth, ckb)`

```go
// poc/coordinator.go
package poc

import (
    "strings"
    "testing"
    "time"

    "github.com/ethereum/go-ethereum/common"
    ethcrypto "github.com/ethereum/go-ethereum/crypto"
    libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
    "github.com/libp2p/go-libp2p/core/peer"
    "github.com/stretchr/testify/require"

    "cross-chain-coordinator/backends"
    "cross-chain-coordinator/service"
)

func StartCoordinator(t *testing.T, eth *ETHChainConfig, ckb *CKBChainConfig, coordCKBAddr string) (*service.Service, peer.ID, common.Address) {
    t.Helper()

    charlieKey, err := DeriveKey(3) // Hardhat account [3]
    require.NoError(t, err)

    libp2pKey, _, err := libp2pcrypto.GenerateKeyPair(libp2pcrypto.RSA, 2048)
    require.NoError(t, err)

    cfg := []backends.BackendCoordinatorConfig{
        {Type: "eth", ETH: &backends.ETHBackendConfig{
            LedgerID:        eth.ChainID.Uint64(),
            ChainURL:        toWS(eth.RPC),
            AdjudicatorAddr: eth.AdjAddr.Hex(),
        }},
        {Type: "ckb", CKB: &backends.CKBBackendConfig{
            RPCURL:         ckb.RPCURL,
            DeploymentFile: "../ckb-devnet/deployment.json",
            Network:        ckb.Network,
            SignerAddress:  coordCKBAddr,
            UseEVMSigner:   true,
        }},
    }
    svc, err := service.New(cfg, charlieKey, libp2pKey)
    require.NoError(t, err)

    t.Cleanup(func() {
        // Drain in-flight coordinate() calls before tearing down chain backends,
        // otherwise CKB / ETH RPC clients are closed mid-tx and the goroutine
        // observes nil receipts. The 60 s budget covers two challenge windows
        // plus tx confirmation slack.
        _ = svc.Wait(60 * time.Second)
        _ = svc.Close()
    })

    return svc, svc.PeerID(), ethcrypto.PubkeyToAddress(charlieKey.PublicKey)
}

func toWS(rpcURL string) string {
    switch {
    case strings.HasPrefix(rpcURL, "http://"):  return "ws://" + strings.TrimPrefix(rpcURL, "http://")
    case strings.HasPrefix(rpcURL, "https://"): return "wss://" + strings.TrimPrefix(rpcURL, "https://")
    default:                                    return rpcURL
    }
}
```

### 10.4 `buildSecretSignedReq` helper

```go
// poc/attack_sub_test.go
package poc_test

import (
    "fmt"

    "perun.network/go-perun/channel"
    "perun.network/go-perun/wallet"
)

func buildSecretSignedReq(
    base       channel.AdjudicatorReq,
    newBals    channel.Balances,
    accs       []wallet.Account,
    bID        wallet.BackendID,
    idx        channel.Index,
) (channel.AdjudicatorReq, error) {
    s := base.Tx.State.Clone()
    s.Version  = base.Tx.State.Version + 1
    s.Balances = newBals

    sigs := make([]wallet.Sig, len(accs))
    for i, a := range accs {
        sig, err := channel.Sign(a, s, bID)
        if err != nil { return channel.AdjudicatorReq{}, fmt.Errorf("signing party %d: %w", i, err) }
        sigs[i] = sig
    }
    return channel.AdjudicatorReq{
        Params: base.Params,
        Acc:    map[wallet.BackendID]wallet.Account{bID: accs[idx]},
        Tx:     channel.Transaction{State: s, Sigs: sigs},
        Idx:    idx,
    }, nil
}
```

### 10.5 `TestAttackNoCoordinator` (sub-channel, CKB-ETH)

```go
// poc/attack_sub_test.go (continued)
func TestAttackNoCoordinator(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
    defer cancel()
    require := require.New(t)
    assert  := assert.New(t)

    eth, ckb, err := poc.LoadChains(); require.NoError(err)
    bus := wire.NewLocalBus()

    aliceKey, err := poc.DeriveKey(1); require.NoError(err)
    bobKey,   err := poc.DeriveKey(2); require.NoError(err)
    alice := poc.NewParticipant(t, "alice", aliceKey, aliceCKBAddr, bus, eth, ckb, "") // no coordinator
    bob   := poc.NewParticipant(t, "bob",   bobKey,   bobCKBAddr,   bus, eth, ckb, "")

    ethAsset := ethchannel.NewAsset(eth.ChainID, eth.AssetAddr)
    ckbAsset := ckbasset.NewCKBytesNervosAsset()
    parts    := []map[wallet.BackendID]wire.Address{alice.WireAddr, bob.WireAddr}

    initAlloc := channel.NewAllocation(2,
        []wallet.BackendID{ethBackendID, ckbBackendID},
        ethAsset, ckbAsset)
    initAlloc.Balances = initBals

    prop, err := client.NewLedgerChannelProposal(challengeDuration, alice.WalletAddr, initAlloc, parts)
    require.NoError(err)

    // Standard ProposeChannel / OnAccept dance, then bring channel to v1 ...
    chAliceBob, chBobAlice := openAndAdvanceToV1(t, ctx, alice, bob, prop)

    v1ReqAlice := client.NewTestChannel(chAliceBob).AdjudicatorReq()
    v1ReqBob   := client.NewTestChannel(chBobAlice).AdjudicatorReq()

    // STEP 1: Bob registers v1 on CKB and waits out the window.
    accs := []wallet.Account{alice.WalletAcc[ckbBackendID], bob.WalletAcc[ckbBackendID]}
    require.NoError(bob.CkbAdj.Register(ctx, v1ReqBob, nil))
    poc.WaitCKBChallenge(challengeDuration)

    // STEP 2: Bob reveals SECRET v2 on ETH (no prior registration → accepted).
    accsETH := []wallet.Account{alice.WalletAcc[ethBackendID], bob.WalletAcc[ethBackendID]}
    v2ReqBobETH, err := buildSecretSignedReq(v1ReqBob, v2Bals, accsETH, ethBackendID, 1)
    require.NoError(err)
    v2ReqAliceETH, err := buildSecretSignedReq(v1ReqAlice, v2Bals, accsETH, ethBackendID, 0)
    require.NoError(err)
    require.NoError(bob.EthAdj.Register(ctx, v2ReqBobETH, nil))
    require.NoError(poc.AdvanceTime(ctx, eth.RPC, challengeDuration+1))

    // Each chain pays out at its locally registered state.
    require.NoError(bob.EthAdj.Withdraw(ctx,   v2ReqBobETH,   nil))
    require.NoError(alice.EthAdj.Withdraw(ctx, v2ReqAliceETH, nil))
    require.NoError(bob.CkbAdj.Withdraw(ctx,   v1ReqBob,      nil))
    require.NoError(alice.CkbAdj.Withdraw(ctx, v1ReqAlice,    nil))

    diff := channel.Balances{
        {alice.BalETH.Balance(), bob.BalETH.Balance()},
        {alice.BalCKB.Balance(), bob.BalCKB.Balance()},
    }.Sub(initBals)
    expected := channel.Balances{
        v2Bals.Sub(initBals)[0], // ETH at v2
        v1Bals.Sub(initBals)[1], // CKB at v1
    }
    assert.True(ctest.EqualBalancesWithDelta(expected, diff, balanceDelta),
        "divergent attack outcome: want %v ±%v, got %v", expected, balanceDelta, diff)
    t.Logf("Attack SUCCEEDED — Bob ETH=%v CKB=%v, Alice ETH=%v CKB=%v",
        diff[0][1], diff[1][1], diff[0][0], diff[1][0])
}
```

### 10.6 `TestAttackWithCoordinator` (sub-channel, CKB-ETH, defended)

```go
// poc/attack_sub_test.go (continued)
func TestAttackWithCoordinator(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
    defer cancel()
    require := require.New(t)
    assert  := assert.New(t)

    eth, ckb, err := poc.LoadChains(); require.NoError(err)

    // Coordinator runs in-process. coordPeerID is what the notifier dials,
    // coordETHAddr is what WithCoordinator embeds for the ETH backend.
    _, coordPeerID, coordETHAddr := poc.StartCoordinator(t, eth, ckb, charlieCKBAddr)
    coordCKBAddr := ckbAddressFromHardhatIndex(3) // Charlie's CKB-side address (omni-lock derived from ETH addr)

    bus := wire.NewLocalBus()
    aliceKey, err := poc.DeriveKey(1); require.NoError(err)
    bobKey,   err := poc.DeriveKey(2); require.NoError(err)

    alice := poc.NewParticipant(t, "alice", aliceKey, aliceCKBAddr, bus, eth, ckb, coordPeerID)
    bob   := poc.NewParticipant(t, "bob",   bobKey,   bobCKBAddr,   bus, eth, ckb, coordPeerID)

    ethAsset := ethchannel.NewAsset(eth.ChainID, eth.AssetAddr)
    ckbAsset := ckbasset.NewCKBytesNervosAsset()
    parts    := []map[wallet.BackendID]wire.Address{alice.WireAddr, bob.WireAddr}

    initAlloc := channel.NewAllocation(2,
        []wallet.BackendID{ethBackendID, ckbBackendID},
        ethAsset, ckbAsset)
    initAlloc.Balances = initBals

    coordETHWallet := ethwallet.Address(coordETHAddr)
    prop, err := client.NewLedgerChannelProposal(
        challengeDuration, alice.WalletAddr, initAlloc, parts,
        // BOTH backend addresses are baked into the channel's params.coordinator.
        // Both must match what `service.New` derived from `signingKey`.
        client.WithCoordinator(map[wallet.BackendID]wallet.Address{
            ethBackendID: &coordETHWallet,
            ckbBackendID: coordCKBAddr,
        }),
    )
    require.NoError(err)

    chAliceBob, chBobAlice := openAndAdvanceToV1(t, ctx, alice, bob, prop)

    // Start watchers so a dispute on one chain is replicated to the other.
    errs := make(chan error, 4)
    go func() { errs <- chAliceBob.Watch(alice) }()
    go func() { errs <- chBobAlice.Watch(bob) }()
    time.Sleep(100 * time.Millisecond)

    v1ReqBob := client.NewTestChannel(chBobAlice).AdjudicatorReq()
    accsETH  := []wallet.Account{alice.WalletAcc[ethBackendID], bob.WalletAcc[ethBackendID]}
    v2ReqBobETH, err := buildSecretSignedReq(v1ReqBob, v2Bals, accsETH, ethBackendID, 1)
    require.NoError(err)

    // Bob registers v1 on CKB → watcher replicates to ETH.
    require.NoError(bob.CkbAdj.Register(ctx, v1ReqBob, nil))
    poc.WaitCKBChallenge(challengeDuration)

    // Bob registers v2 on ETH BEFORE its window expires.
    require.NoError(bob.EthAdj.Register(ctx, v2ReqBobETH, nil))
    require.NoError(poc.AdvanceTime(ctx, eth.RPC, challengeDuration+1))

    // Coordinator service detects both registrations have finalised, selects v2
    // (highest version), signs it for both backends, and submits coordinate()
    // on both chains. The on-chain CoordinatedEvent propagates back.
    require.Eventually(func() bool { return chAliceBob.Phase() == channel.Coordinated },
        30*time.Second, 200*time.Millisecond, "alice must reach Coordinated phase")
    require.Eventually(func() bool { return chBobAlice.Phase() == channel.Coordinated },
        30*time.Second, 200*time.Millisecond, "bob must reach Coordinated phase")

    require.NoError(chAliceBob.Settle(ctx, false))
    require.NoError(chBobAlice.Settle(ctx, false))
    require.NoError(chAliceBob.Close())
    require.NoError(chBobAlice.Close())

    diff := channel.Balances{
        {alice.BalETH.Balance(), bob.BalETH.Balance()},
        {alice.BalCKB.Balance(), bob.BalCKB.Balance()},
    }.Sub(initBals)
    allV2Diff   := v2Bals.Sub(initBals)
    divergent   := channel.Balances{v2Bals.Sub(initBals)[0], v1Bals.Sub(initBals)[1]}
    assert.True (ctest.EqualBalancesWithDelta(allV2Diff,  diff, balanceDelta), "coordinator must enforce uniform v2")
    assert.False(ctest.EqualBalancesWithDelta(divergent,  diff, balanceDelta), "divergent outcome must not occur")
    t.Logf("Attack PREVENTED — uniform v2: Bob ETH=%v CKB=%v, Alice ETH=%v CKB=%v",
        diff[0][1], diff[1][1], diff[0][0], diff[1][0])
}
```

### Event flow during the defended sub-channel scenario

```
ProposeChannel(WithCoordinator)                    ← BOTH clients notify
    │
    ▼
RelayCoordinatorNotifier.NotifyWatchLedgerChannel  ← /coordinator/notify-watch-ledger/1.0.0
    │
    ▼
host.startWatchingLedger                           ← cross-chain-coordinator side
    │
    ▼
multi.Coordinator.Subscribe (ETH + CKB)            → handleEventsFromChain per ledger
    │
    ▼
RegisteredEvent (CKB v1 + ETH v2) → record per-chain dispute
    │
    ▼
awaitFinalisationAndCoordinate (wall-clock timer)
    │
    ▼
coordinate():
  selectCanonicalSignedState (v2 wins) → buildCoordSigs (1 sig — secp256k1 over keccak256)
  multi.Coordinator.Coordinate fans out to ETH AND CKB (errgroup)
    │
    ▼
on-chain coordinate() / coordinate-cell on both chains → CoordinatedEvent
    │
    ▼
client.awaitCoordinated → Phase = Coordinated → Settle → Withdraw at uniform v2
```

---

## 11. PoC implementation — virtual channel

### 11.1 Topology

```
                            CKB-ETH parent A                CKB-ETH parent B
Alice ←──────────────────► Ingrid ←─────────────────────────► Bob
       (multi-ledger)              (multi-ledger)
                          \                                /
                           \   Alice ↔ Bob virtual         /
                            └────  channel V  ───────────┘
                                  (settles through parents)
```

`Ingrid` proposes the virtual channel V to Bob; V's allocation is
**locked inside both parent channels' `Locked` fields**. While V is open,
all Alice ↔ Bob updates flow through V (off-chain, Ingrid mediates). When V
closes, its terminal state is folded back into the parent allocations and
each parent settles uniformly via the coordinator.

### 11.2 Why a coordinator is needed at the virtual layer

Suppose Bob is malicious and wants to divert the virtual channel's locked
balance on Ingrid's parent B. Without a coordinator:

1. Bob disputes parent B on **CKB only** at the agreed v1.
2. Wait out CKB's challenge window.
3. Then dispute parent B on **ETH** at a fabricated v2 that re-routes the
   virtual channel's locked balance to Bob.
4. ETH's parent B settles at v2 (Bob favoured); CKB's parent B settles at v1
   (Ingrid favoured). Bob profits.

With the coordinator, both parent B chains lock to the same canonical version
before either pays out. V's locked balance is therefore preserved uniformly,
and V can settle into both parents identically.

### 11.3 Participant + Ingrid setup

```go
// poc/attack_virtual_test.go
package poc_test

// Same imports as the sub-channel tests + go-perun's virtual channel proposal.
//   "perun.network/go-perun/client"  // NewVirtualChannelProposal
//   "perun.network/go-perun/channel" // Aux, IDLen

type virtualSetup struct {
    alice, ingrid, bob *poc.Participant
    chAliceIngrid      *client.Channel
    chBobIngrid        *client.Channel
    chAliceBob         *client.Channel
}

// newVirtualSetup brings up three participants and the two parent multi-ledger
// channels (Alice-Ingrid, Bob-Ingrid). It does NOT yet open V.
func newVirtualSetup(t *testing.T, ctx context.Context, withCoord bool) *virtualSetup {
    require := require.New(t)
    eth, ckb, err := poc.LoadChains(); require.NoError(err)
    bus := wire.NewLocalBus()

    var coordPeerID peer.ID
    var coordETHAddr common.Address
    if withCoord {
        _, coordPeerID, coordETHAddr = poc.StartCoordinator(t, eth, ckb, charlieCKBAddr)
    }

    aliceKey,  _ := poc.DeriveKey(1)
    bobKey,    _ := poc.DeriveKey(2)
    ingridKey, _ := poc.DeriveKey(4) // Hardhat account [4]

    alice  := poc.NewParticipant(t, "alice",  aliceKey,  aliceCKBAddr,  bus, eth, ckb, coordPeerID)
    ingrid := poc.NewParticipant(t, "ingrid", ingridKey, ingridCKBAddr, bus, eth, ckb, coordPeerID)
    bob    := poc.NewParticipant(t, "bob",    bobKey,    bobCKBAddr,    bus, eth, ckb, coordPeerID)

    ethAsset := ethchannel.NewAsset(eth.ChainID, eth.AssetAddr)
    ckbAsset := ckbasset.NewCKBytesNervosAsset()

    // Open Alice ↔ Ingrid parent A.
    chAliceIngrid := openMultiLedgerParent(t, ctx, alice, ingrid, ethAsset, ckbAsset, withCoord, coordETHAddr)
    // Open Bob ↔ Ingrid parent B (symmetric).
    chBobIngrid := openMultiLedgerParent(t, ctx, bob, ingrid, ethAsset, ckbAsset, withCoord, coordETHAddr)

    return &virtualSetup{alice: alice, ingrid: ingrid, bob: bob,
        chAliceIngrid: chAliceIngrid, chBobIngrid: chBobIngrid}
}

// openMultiLedgerParent: standard NewLedgerChannelProposal between two
// Participants over the two backends. When withCoord, embeds the coordinator's
// {ETH, CKB} address pair via client.WithCoordinator and starts watchers.
func openMultiLedgerParent(t *testing.T, ctx context.Context, p, q *poc.Participant,
    ethAsset, ckbAsset channel.Asset, withCoord bool, coordETH common.Address) *client.Channel {
    // ... NewLedgerChannelProposal + accept handler + Watch goroutines.
    // Returns p's *client.Channel after Bob/Ingrid accepts.
}
```

### 11.4 Opening the virtual channel

```go
func (v *virtualSetup) openVirtual(t *testing.T, ctx context.Context, initBalsAB channel.Balances) {
    require := require.New(t)
    ethAsset := ethchannel.NewAsset(big.NewInt(1337), common.HexToAddress("..."))
    ckbAsset := ckbasset.NewCKBytesNervosAsset()

    // V's allocation: balances flow between Alice (index 0) and Bob (index 1).
    initAlloc := channel.Allocation{
        Assets:   []channel.Asset{ethAsset, ckbAsset},
        Balances: initBalsAB,
        Backends: []wallet.BackendID{ethBackendID, ckbBackendID},
    }

    // indexMapAlice maps V indices [0=alice, 1=bob] into chAliceIngrid indices [0=alice, 1=ingrid]:
    //   V's index 0 (alice) → 0, V's index 1 (bob) → 1 (the ingrid slot in parent A).
    // indexMapBob maps V indices [0=alice, 1=bob] into chBobIngrid indices [0=bob, 1=ingrid]:
    //   V's index 0 (alice) → 1 (the ingrid slot in parent B), V's index 1 (bob) → 0.
    indexMapAlice := []channel.Index{0, 1}
    indexMapBob   := []channel.Index{1, 0}

    parents := []channel.ID{v.chAliceIngrid.ID(), v.chBobIngrid.ID()}
    peers   := []map[wallet.BackendID]wire.Address{v.alice.WireAddr, v.bob.WireAddr}

    // CKB is UTXO — pack both parent IDs into Aux so PCTS can verify the
    // virtual cell's lineage.
    var aux channel.Aux
    copy(aux[:channel.IDLen],     parents[0][:])
    copy(aux[channel.IDLen:],     parents[1][:])

    vcp, err := client.NewVirtualChannelProposal(
        challengeDuration,
        v.alice.WalletAddr,
        &initAlloc,
        peers,
        parents,
        [][]channel.Index{indexMapAlice, indexMapBob},
        client.WithAux(aux),
    )
    require.NoError(err)

    v.chAliceBob, err = v.alice.Client.ProposeChannel(ctx, vcp)
    require.NoError(err)
}
```

### 11.5 `TestVirtualAttackNoCoordinator`

```go
func TestVirtualAttackNoCoordinator(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
    defer cancel()
    require := require.New(t)
    assert  := assert.New(t)

    v := newVirtualSetup(t, ctx, /*withCoord=*/ false)
    v.openVirtual(t, ctx, initBalsAB) // Alice 5/5 in each asset, say

    // V agrees on v1, then Bob fabricates v2 that drains the virtual cell.
    advanceVirtualToV1(t, ctx, v)
    v1ReqBob, _ := v.bob.AdjudicatorReqForVirtualParent(t, ctx)

    // Bob disputes parent B on CKB at v1.
    require.NoError(v.bob.CkbAdj.Register(ctx, v1ReqBob.parentB, v1ReqBob.subStates))
    poc.WaitCKBChallenge(challengeDuration)

    // Bob disputes parent B on ETH at fabricated v2 that re-routes V's locked
    // balance entirely to Bob's slot.
    v2ReqBobETH := fabricateV2DrainingVirtual(t, v1ReqBob.parentB, v.alice, v.bob)
    require.NoError(v.bob.EthAdj.Register(ctx, v2ReqBobETH, v1ReqBob.subStates))
    require.NoError(poc.AdvanceTime(ctx, ethRPC, challengeDuration+1))

    settleAllWithdrawals(t, ctx, v)

    diff := finalDiff(v)
    // ETH side of parent B paid out v2 (Bob favoured); CKB side paid v1.
    // Net: Bob profits — the virtual channel's safety is broken.
    assert.True(divergentVirtualOutcome(diff), "without coordinator the virtual channel can be drained")
    t.Logf("Virtual attack SUCCEEDED — diff=%v", diff)
}
```

### 11.6 `TestVirtualAttackWithCoordinator`

```go
func TestVirtualAttackWithCoordinator(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
    defer cancel()
    require := require.New(t)
    assert  := assert.New(t)

    v := newVirtualSetup(t, ctx, /*withCoord=*/ true)
    v.openVirtual(t, ctx, initBalsAB)
    advanceVirtualToV1(t, ctx, v)

    // Bob attempts the same divergent dispute on parent B.
    v1ReqBob, _ := v.bob.AdjudicatorReqForVirtualParent(t, ctx)
    require.NoError(v.bob.CkbAdj.Register(ctx, v1ReqBob.parentB, v1ReqBob.subStates))
    poc.WaitCKBChallenge(challengeDuration)

    v2ReqBobETH := fabricateV2DrainingVirtual(t, v1ReqBob.parentB, v.alice, v.bob)
    require.NoError(v.bob.EthAdj.Register(ctx, v2ReqBobETH, v1ReqBob.subStates))
    require.NoError(poc.AdvanceTime(ctx, ethRPC, challengeDuration+1))

    // The coordinator service watches parent B on BOTH chains, sees both
    // RegisteredEvents finalise, selects v2 as canonical, and calls
    // coordinate() on CKB AND ETH so both lock to v2 — INCLUDING the
    // virtual-channel sub-state passed via subChannels.
    require.Eventually(func() bool { return v.chBobIngrid.Phase() == channel.Coordinated },
        45*time.Second, 200*time.Millisecond, "parent B must reach Coordinated phase")

    // Settle parent B → V's terminal state replicated identically to both chains.
    require.NoError(v.chBobIngrid.Settle(ctx, false))
    require.NoError(v.chAliceIngrid.Settle(ctx, false))

    diff := finalDiff(v)
    // Because v2 is the canonical version on BOTH chains, parent B pays the
    // same balance on ETH and CKB. V's preserved balance fills Alice's slot
    // before Bob can extract it.
    assert.True (uniformVirtualOutcome(diff),    "coordinator must enforce uniform parent settlement")
    assert.False(divergentVirtualOutcome(diff),  "no divergent outcome may occur")
    t.Logf("Virtual attack PREVENTED — diff=%v", diff)
}
```

The helpers `fabricateV2DrainingVirtual`, `advanceVirtualToV1`, `finalDiff`,
`uniformVirtualOutcome`, `divergentVirtualOutcome`, `settleAllWithdrawals`,
and `AdjudicatorReqForVirtualParent` are mechanical — they mirror the
sub-channel helpers but operate on the virtual-channel proposer's parent
adjudicator request rather than a single channel's req. Keep them in a
shared `poc/virtual_helpers_test.go` so 11.5 and 11.6 read clean.

---

## 12. Running the PoC

```bash
# Terminal 1 — Ethereum
cd hardhat && npx hardhat node --port 8545

# Terminal 2 — CKB devnet
ckb run -C ckb-devnet/

# Terminal 3 — deploy contracts (once) and run tests
cd hardhat
npx hardhat run scripts/deploy.js --network eth        # writes hardhat/addresses.json

cd ../perun-ckb-backend
./devnet/deploy_contracts.sh ../multiledger-poc/ckb-devnet/migrations ./system_scripts
go run ./cmd/build_deployment_json > ../multiledger-poc/ckb-devnet/deployment.json

cd ../multiledger-poc
go test -v -run TestAttackNoCoordinator        -timeout 180s ./poc/   # sub-channel, vulnerable
go test -v -run TestAttackWithCoordinator      -timeout 240s ./poc/   # sub-channel, defended
go test -v -run TestVirtualAttackNoCoordinator -timeout 240s ./poc/   # virtual, vulnerable
go test -v -run TestVirtualAttackWithCoordinator -timeout 300s ./poc/ # virtual, defended

# Race detector across the whole suite:
go test -count=1 -race -timeout 600s ./poc/
```

The defended tests construct the coordinator in-process via `service.New(...)`;
only the two chain nodes need to be running externally. A reachable
`relay.perun.network:5574` is required because the coordinator dials the Perun
libp2p relay to accept client notifications over circuit streams.

### Expected output

```
=== RUN   TestAttackNoCoordinator
    attack_sub_test.go: Attack SUCCEEDED — Bob ETH=9e18 CKB=7e8, Alice ETH=1e18 CKB=3e8
--- PASS: TestAttackNoCoordinator                  (~45 s)

=== RUN   TestAttackWithCoordinator
    attack_sub_test.go: Attack PREVENTED — uniform v2: Bob ETH=9e18 CKB=5e8, Alice ETH=1e18 CKB=5e8
--- PASS: TestAttackWithCoordinator                (~70 s)

=== RUN   TestVirtualAttackNoCoordinator
    attack_virtual_test.go: Virtual attack SUCCEEDED — diff=...
--- PASS: TestVirtualAttackNoCoordinator           (~90 s)

=== RUN   TestVirtualAttackWithCoordinator
    attack_virtual_test.go: Virtual attack PREVENTED — diff=...
--- PASS: TestVirtualAttackWithCoordinator         (~120 s)
```

---

## 13. Known limitations

- **CKB has no `evm_increaseTime` analog.** Dispute timers progress with real
  wall-clock time as CKB blocks are produced. The defended demos wait the
  full `challengeDuration` seconds on every dispute — keep that constant small
  (≤ 15 s) or the test suite times out. The coordinator's
  `awaitFinalisationAndCoordinate` also uses wall-clock, so this constraint
  binds for both backends.
- **Virtual-channel safety depends on Ingrid.** The coordinator protects each
  *parent* channel's uniform settlement. While V is open and Ingrid is honest,
  V's state evolves correctly off-chain. A colluding Ingrid + Bob can still
  push V's state in Bob's favour off-chain — that's an orthogonal threat
  model (general Perun virtual channel assumption, not specific to this PoC).
  The coordinator cannot detect collusion; it only enforces that whatever
  state ends up locked into each parent settles identically on every chain.
- **`signer_address` is operator-supplied for CKB.** The coordinator service
  does not derive Charlie's CKB address from his ECDSA pubkey automatically
  — too many CKB-side script choices (omni-lock vs. secp256k1_blake160, hash
  type) to pick correctly without operator input. The PoC test computes it
  once via `ckb-cli account import` and pastes the result into the config.
- **CKB devnet rebuild required after restart.** `ckb run` does not preserve
  state across stops by default. Re-deploy the Perun scripts (capsule
  migration) and re-generate `ckb-devnet/deployment.json` on every devnet
  restart, otherwise channel cells reference stale tx hashes.
- **CKB `client/ckbclient.go` upstream TODOs.** Two minor TODOs remain on the
  perun-ckb-backend `coordination` branch: a `Start`-method default-hash
  override and a `createOrGetChannelToken` cell-reuse improvement. Neither
  blocks the coordinate path; both are noted for completeness.

---

## 14. Key invariants

| Layer             | Invariant                                                         | Enforcement                                                 |
| ----------------- | ----------------------------------------------------------------- | ----------------------------------------------------------- |
| ETH contract      | `coordinate()` requires prior `register()`                        | `coordinateSingle`: `"not registered"`                      |
| ETH contract      | `coordinate()` requires `block.timestamp ≥ dispute.timeout`       | `coordinateSingle`: `"refutation timeout not passed"`       |
| ETH contract      | Coordinator ECDSA sig required                                    | `Channel.validateCoordinatorSignature`                      |
| ETH contract      | `register()` rejected in COORDINATED phase                        | `registerSingle`: `"incorrect phase"`                       |
| ETH contract      | Multi-ledger `conclude()` requires COORDINATED                    | `concludeSingle`: `"coordinated settlement required"`       |
| ETH contract      | Coordinator-eligible requires `coordinator != 0 && multiLedger`   | `MultiLedger.sol: isCoordinatedEligible`                    |
| CKB script (PCTS) | `coordinate` cell requires prior `register` cell for that channel | tx rejected (PCTS phase guard)                              |
| CKB script (PCTS) | `coordinate` requires dispute window elapsed                      | tx rejected (PCTS timing guard)                             |
| CKB script (PCTS) | Coordinator signature in `CoordSig` witness must match params     | tx rejected (PCTS sig check)                                |
| CKB script (PCTS) | `register` rejected once channel cell is in COORDINATED state     | tx rejected (PCTS phase guard)                              |
| CKB script (PFLS) | Funds unlock requires parent channel CONCLUDED                    | tx rejected (PFLS phase guard)                              |
| go-perun (multi)  | All chains coordinated concurrently; first error reported         | `multi.Coordinator.dispatch` (errgroup)                     |
| go-perun (client) | `Settle` calls `ensureCoordinated` before `Withdraw`              | `client.Channel.Settle`                                     |
| go-perun (watcher)| Dispute replicated to all chains                                  | `watcher/local` multi-ledger path                           |
| Timing (ETH)      | All timeouts use `block.timestamp` (seconds)                      | `Adjudicator.sol` (use `evm_increaseTime` in tests)         |
| Timing (CKB)      | Dispute timer measured in block height; no fast-forward           | wall-clock waits in tests                                   |

### Timing diagram (defended sub-channel)

```
         t0          t1 (CKB frozen)        t2      t3 (ETH frozen)     t4
CKB:    register(v1)──[window: 15 s]────timeout─────────────────── COORDINATED(v2)──CONCLUDED
                                            │                            ↑
ETH:    register(v1)─── register(v2) ──────│─[window: 15 s]──timeout───┘
        ↑ watcher          ↑ Bob            │                       coordinator service
        replication        (before window    │                       calls coordinate(v2)
                            closes)          │                       on both chains
```

Key sequencing: CKB's window expires first (t1). Only then does Bob register v2
on ETH (still within ETH's open window). ETH's window expires (t3) with v2
frozen. The coordinator then picks v2 as canonical and locks both chains to v2
(t4). After both chains report COORDINATED, `Channel.Settle → ensureCoordinated`
is a no-op and `Withdraw` succeeds at the uniform v2 outcome.

The virtual-channel timing diagram is identical at the parent layer; the
virtual channel itself never registers on-chain unless Ingrid disputes one of
the parents.
