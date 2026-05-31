# cross-chain-coordinator

A Trusted Third Party (TTP) **coordinator service** for the [Perun](https://perun.network)
state-channel protocol. It watches multi-ledger channels across multiple EVM chains
and, once every chain's dispute window has closed, issues a coordinator certificate
(`coordSigs`) and submits an on-chain `coordinate()` transaction on every chain so
both sides settle at the same canonical version.

Without a coordinator, a malicious participant can register different versions of a
multi-ledger channel state on different chains and withdraw divergently. See
[`MULTILEDGER_ATTACK_POC.md`](./MULTILEDGER_ATTACK_POC.md) for the full attack model
and protection guarantee.

The service is a standalone process. Clients talk to it over a libp2p circuit relay
using three JSON-over-stream protocols (`/coordinator/notify-watch-ledger/1.0.0`,
`notify-watch-sub`, `notify-stop-watch`).

---

## Quick start

```bash
# 1) Generate the libp2p identity (one-time; writes an RSA-2048 libp2p key).
go run . -mode keygen -keyfile sign_private.key

# 2) Generate the ECDSA signing key (one-time; hex, no 0x prefix, one line).
openssl rand -hex 32 > coord_ecdsa.key

# 3) Edit devnet_config.yaml to point at your chains and ECDSA key (see below).

# 4) Start the relay coordinator.
go run . -mode relay -keyfile sign_private.key -config devnet_config.yaml
# Logs: "Relay server started with ID: 12D3KooW…"   ← that's the coordinator's peer.ID
```

The service dials the hardcoded Perun relay (`relay.perun.network:5574`, peer ID
`QmcxeYpYpYPX4J3478YZUaxFytYfUDbNe1jUWVYeZjL3gY`), reserves a slot (renewed every
4 min), and accepts inbound streams through the relay circuit. It never listens
directly.

---

## Configuration (`devnet_config.yaml`)

Each entry in `coordinators` is a tagged union: `type: eth | ckb` selects the
backend, and the corresponding `eth:` or `ckb:` block carries the
backend-specific fields. Mix freely — one ETH chain and one CKB chain is the
end-goal CKB-ETH multi-ledger topology.

```yaml
private_key_path: ./coord_ecdsa.key     # 64-char hex ECDSA key (one line, no 0x prefix)
coordinators:
  - type: eth
    eth:
      ledger_id: 1337                     # chain ID
      chain_url: "ws://127.0.0.1:8545"    # MUST be ws:// or wss://
      adjudicator_addr: "0xDEADBEEF..."   # deployed Adjudicator on this chain
  - type: ckb
    ckb:
      rpc_url: "http://127.0.0.1:8114"    # MUST be http:// or https://
      deployment_file: "./ckb_deployment.json"  # JSON-serialised backend.Deployment
      network: "devnet"                   # mainnet | testnet | devnet
      signer_address: "ckt1qyqd0rfh3..."  # operator's CKB address
      use_evm_signer: true                # true = omni-lock EVM auth; false = secp256k1_blake160
```

`backends.Config.Validate` enforces:
- non-empty `private_key_path`
- at least one coordinator entry
- `type` is `eth` or `ckb`, and exactly the matching `eth:` / `ckb:` block is set
- **ETH**: unique per `ledger_id`; `chain_url` starts with `ws://` or `wss://`
  (HTTP transports silently break `SubscribeNewHead`, required for
  `BlockTimeout.Wait` and `confirmNTimes` in the ETH backend); non-empty
  `adjudicator_addr`
- **CKB**: at most one CKB entry (CKB uses a fixed `LedgerBackendID` of
  `asset.CCID{backendID=3, ledgerID="03"}` regardless of mainnet/testnet/devnet);
  `rpc_url` is `http://` or `https://` (CKB JSON-RPC is HTTP-only — the polling
  subscription handles event streaming); non-empty `deployment_file` (read at
  service start, parsed as JSON into `perun-ckb-backend.Deployment`); non-empty
  `signer_address`; `network` is `mainnet`, `testnet`, or `devnet`

The ECDSA key referenced by `private_key_path` is the coordinator's signing key,
shared between backends. **Its derived ETH address is what clients embed via**
`client.WithCoordinator(map[BackendID]wallet.Address{1: ethAddr, 3: ckbAddr})`
**at channel-open time** — channel IDs are derived from `Params` including this
map, so a mismatch makes `CalcID` differ on the client side and the channel is
unrecognisable. ETH and CKB both sign Keccak256 of the same `channel.State`
encoding with the same key, so a single `*ecdsa.PrivateKey` produces valid
coordinator signatures for both on-chain verifiers.

The libp2p `-keyfile` is independent of the ECDSA key and determines the
service's `peer.ID`. Keep it stable across restarts — clients hardcode this
peer ID.

---

## Architecture

```
main.go                       CLI: -mode keygen | -mode relay
service/service.go            wires backends.SetupMultiCoordinator +
                              coordinator.SetupRelayCoordinator
backends/
  config.go                   YAML loader + tagged-union validator
  multicoordinator.go         builds *multi.Coordinator + wallet.Account map,
                              dispatches on Type (eth | ckb)
  ethcoordinator.go           one *ethchannel.Coordinator per chain
  ckbcoordinator.go           one *ckbcoordinator.Coordinator per CKB network
                              (uses fixed LedgerBackendID asset.CCID{3, "03"})
coordinator/
  host.go                     libp2p host, relay reservation, stream handler
                              registration, Wait()/coordWg
  handlers.go                 JSON-decode + dispatch for the 3 protocol streams
  protocol.go                 protocol IDs, request/response envelopes
  registry.go                 in-memory coordCh + registry, per-chain dispute records
  watcher.go                  startWatching*, handleEventsFromChain,
                              awaitFinalisationAndCoordinate (wall-clock dispute timer)
  coordinator.go              coordinate(): selectCanonicalSignedState, buildCoordSigs,
                              multi.Coordinator.Coordinate dispatch
```

### Coordination flow

```
NotifyWatchLedgerChannel(signedState)
    │
    ▼
host.startWatchingLedger
    │
    ├─ validateCoordinatorDesignation  (Params.Coordinator must contain our address)
    ├─ multi.Coordinator.Subscribe(ctx, id)  (one ethchannel sub per chain)
    └─ spawn handleEventsFromChain goroutine
            │
            ▼
        for ev := range *multi.AdjudicatorSubscription:
            *RegisteredEvent  → record chainDisputes[ledger]
                                spawn awaitFinalisationAndCoordinate on first registration
            *ProgressedEvent  → reset that chain's dispute timeout
            *CoordinatedEvent → registry.remove(id) + return
            *ConcludedEvent   → registry.remove(id) + return

awaitFinalisationAndCoordinate (per channel)
    │
    ▼
loop until every chainDispute is finalised:
    wait time.Until(d.timeout) using time.After   ← WALL-CLOCK, not block.timestamp
    │
    ▼
coordinate(ctx, ch)
    │
    ├─ selectCanonicalSignedState     (highest .State.Version across chainDisputes)
    ├─ retrieveSignedSubStates        (DFS walk over canonical.State.Locked)
    ├─ buildCoordSigs                 (coordSigs[0]=parent, then DFS sub-states)
    └─ multi.Coordinator.Coordinate   (fan-out one goroutine per asset's chain)
```

The wall-clock dispute timer is intentional — it decouples the coordinator from each
chain's block-production behaviour. Trade-off: integration tests that fast-forward
the chain clock still sleep the full `ChallengeDuration` seconds in real time, so use
a short `ChallengeDuration` (1–15 s) in tests rather than minutes.

`coordinate()` is bounded by `CoordinatorHost.coordinateTimeout`
(`defaultCoordinateTimeout = 5 min`) and tracked in `coordWg`; call
`CoordinatorHost.Wait(timeout)` to drain all in-flight `coordinate()` goroutines
before tearing down chain backends.

---

## Client-side wiring

Clients use `RelayCoordinatorNotifier` from the go-perun fork
(`wire/net/libp2p/coordinator.go`) to notify the coordinator on channel events.
The notifier takes the coordinator's `peer.ID` at construction — that ID is
the one printed by `runRelay` at startup (`Relay server started with ID: 12D3KooW…`).

```go
import (
    "github.com/libp2p/go-libp2p/core/peer"
    libp2pwire "perun.network/go-perun/wire/net/libp2p"
    "perun.network/go-perun/client"
)

// One per participant — must be installed BEFORE the first ProposeChannel.
account, _ := libp2pwire.NewAccount(privKey /* + dialer/listener */)

coordPeerID, _ := peer.Decode("12D3KooW…") // from coordinator startup log
notifier := libp2pwire.NewRelayCoordinatorNotifier(account, coordPeerID)
client.EnableCoordinationNotifier(notifier)

// Channel proposals must include the coordinator's wallet address.
prop, _ := client.NewLedgerChannelProposal(
    challengeDuration, walletAddr, alloc, parts,
    client.WithCoordinator(map[wallet.BackendID]wallet.Address{1: coordEthAddr}),
)
```

---

## Testing

```bash
# Unit + always-on mock-integration tests (~0.3 s).
go test ./...

# Adds the ETH simulated-backend integration test (deploys Adjudicator, runs the
# full Register → wait → coordinate → CoordinatedEvent pipeline; ~2 s per run).
go test -tags integration ./coordinator/... -run TestEthIntegration -v -timeout 60s
```

The integration test mines blocks via `sb.StartMining(50ms)`, so chain time
advances ~20 s per second of real time. The full test (`ChallengeDuration = 2 s`)
completes in ~2 s end-to-end.

Test files:
- `backends/config_test.go` — config loader + validator
- `coordinator/coordinator_test.go` — `isAlreadyConcluded` heuristic
- `coordinator/watcher_test.go` — `validateCoordinatorDesignation`, registry cascade
- `coordinator/mock_integration_test.go` — full event→coordinate pipeline with an
  injectable mock `CoordinatorSubscriber` (single-ledger, sub-channel, duplicate-event)
- `coordinator/eth_integration_test.go` — full pipeline on simulated geth with a
  deployed Adjudicator (build tag `integration`)

---

## Dependencies (forks pinned via `replace`)

```
replace perun.network/go-perun                     => github.com/NhoxxKienn/go-perun                v0.0.0-20260526062537-a05990e2cb40
replace github.com/perun-network/perun-eth-backend => github.com/NhoxxKienn/perun-eth-backend      v0.6.1-0.20260525091241-e1f6c19121e0
replace perun.network/perun-ckb-backend            => github.com/NhoxxKienn/perun-ckb-backend      v0.0.0-20260531113006-5aa8111e501a
replace github.com/nervosnetwork/ckb-sdk-go/v2     => github.com/perun-network/ckb-sdk-go/v2       v2.2.1-0.20260530044933-548463b5d86f
```

- **go-perun fork** adds `multi.Coordinator`, `CoordinatedEvent`, and the
  client-side `RelayCoordinatorNotifier` wiring this service consumes.
- **perun-eth-backend fork** exposes `ethchannel.Coordinator` (the
  `CoordinatorSubscriber` implementation for EVM chains).
- **perun-ckb-backend `coordination` branch** exposes
  `coordinator.NewCoordinator(client.CKBClient)` and the matching polling
  subscription that emits `CoordinatedEvent` after a CKB `coordinate` cell
  lands. Its own `replace` on `github.com/nervosnetwork/ckb-sdk-go/v2` is
  reproduced here so the transitive dependency graph resolves cleanly.

---

## Known limitations

- **No persistence** of watched channels across restarts. A coordinator restart in
  the middle of a dispute window currently loses the in-memory `registry` state;
  clients would need to re-send `NotifyWatch*`.
- **Partial-failure recovery is best-effort.** `multi.Coordinator.dispatch` now waits
  for every per-chain coordinate goroutine (via `errgroup`) and reports the first
  error, but there is no retry loop — a transient failure on one chain currently
  bubbles up to `awaitFinalisationAndCoordinate`'s error log without an automated
  re-try. Manual recovery: the on-chain `coordinate()` is idempotent, so a fresh
  `NotifyWatch*` from any participant will re-drive coordination.
- **CKB time advancement.** CKB has no `evm_increaseTime` analog — its dispute
  timers progress with real wall-clock time as blocks are produced. For CKB-ETH
  multi-ledger demos this means a short `challengeDuration` (≤ 15 s) keeps tests
  fast; longer windows wait in real time even on the ETH side because the
  coordinator's `awaitFinalisationAndCoordinate` uses wall-clock, not block
  height.
- **At most one CKB entry per coordinator.** CKB always reports its
  `LedgerBackendID` as `asset.CCID{backendID=3, ledgerID="03"}` — there is no
  per-chain LedgerID variability. A second CKB entry is rejected by the
  validator. (ETH chains are unique per chain ID; many ETH entries are
  permitted.)

Previously documented gaps that are now fixed:

- `coordID` placeholder in the go-perun fork — replaced by
  `NewRelayCoordinatorNotifier(acc, coordPeerID)` taking the peer ID at runtime.
- `isAlreadyConcluded` substring matching — now prefers
  `errors.Is(err, channel.ErrChannelAlreadyConcluded)` (typed sentinel exported by
  go-perun), with substring matching kept only as a fallback for un-wrapped
  on-chain revert reasons.
- `multi.Coordinator.dispatch` partial-failure abandonment — upstream switched to
  `errgroup.WithContext`, so dispatch now waits for all per-chain goroutines and
  reports the first error rather than abandoning in-flight calls.

See [`MULTILEDGER_ATTACK_POC.md`](./MULTILEDGER_ATTACK_POC.md) §13.7 for the full
gaps table including detail on each fix.

---

## See also

- [`CLAUDE.md`](./CLAUDE.md) — project context for AI assistants
- [`MULTILEDGER_ATTACK_POC.md`](./MULTILEDGER_ATTACK_POC.md) — attack model,
  protection guarantee, coordinator implementation reference, and a self-contained
  Hardhat PoC
- [go-perun coordination branch](https://github.com/NhoxxKienn/go-perun/tree/coordination)
  — fork with `multi.Coordinator`, `CoordinatedEvent`, `RelayCoordinatorNotifier`
