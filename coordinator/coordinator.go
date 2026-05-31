package coordinator

import (
	"context"
	stderrors "errors"
	"strings"

	"github.com/pkg/errors"
	"perun.network/go-perun/channel"
	"perun.network/go-perun/wallet"
)

// coordinate is called once ReadyForCoordination holds.
//  1. Selects the canonical state (highest version across all chains).
//  2. Collects signed sub-states in DFS order — mirrors retrieveLatestSubStates.
//  3. Signs each state (parent + all sub-channels) with coordinator key — coordSigs in DFS order.
//  4. Submits via c.coordinator.Coordinate() which dispatches to all chains.
//
// The call is bounded by c.coordinateDeadline() so a stuck chain cannot pin
// the goroutine forever, and tracked in c.coordWg so CoordinatorHost.Wait can
// drain it cleanly during shutdown.
func (c *CoordinatorHost) coordinate(ctx context.Context, ch *coordCh) error {
	c.coordWg.Add(1)
	defer c.coordWg.Done()

	ctx, cancel := context.WithTimeout(ctx, c.coordinateDeadline())
	defer cancel()

	ch.subChsAccess.Lock()

	// Step 1: Select canonical signed state — highest version across all chains.
	// selectCanonicalSignedState returns the full SignedState (state + participant sigs)
	// from the best chainDispute record. It can return nil when no chain recorded
	// a valid state yet — dereferencing canonical.State without this guard panics.
	canonical := ch.selectCanonicalSignedState()
	if canonical == nil || canonical.State == nil {
		ch.subChsAccess.Unlock()
		return errors.New("coordinate: no canonical state available")
	}
	ch.canonicalState = canonical
	ch.phase = PhaseReadyForCoord

	ch.subChsAccess.Unlock()

	// Step 2: Build AdjudicatorReq for the parent channel.
	// Sigs are the participant signatures already recorded on the canonical state.
	req := channel.AdjudicatorReq{
		Params: ch.params,
		Tx: channel.Transaction{
			State: canonical.State,
			Sigs:  canonical.Sigs,
		},
	}

	// Step 3: Collect signed sub-states in DFS order — mirrors retrieveLatestSubStates.
	signedSubStates, err := c.retrieveSignedSubStates(ch)
	if err != nil {
		return errors.WithMessage(err, "coordinate: collecting sub-states")
	}

	// Step 4: Build coordSigs in DFS order.
	// coordSigs[0]   = coordinator sig over parent canonical state
	// coordSigs[1..] = coordinator sig over each sub-channel state, DFS order
	// Matches contract's coordinateRecursive(coordSigs, startIndex) indexing.
	coordSigs, err := c.buildCoordSigs(ch, canonical.State, signedSubStates)
	if err != nil {
		return errors.WithMessage(err, "coordinate: building coordSigs")
	}

	// Step 5: Dispatch to all chains via multi.Coordinator concurrently.
	if err := c.coordinator.Coordinate(ctx, req, signedSubStates, coordSigs); err != nil {
		if isAlreadyConcluded(err) {
			// Channel already coordinated/concluded on-chain — treat as success.
			// TODO: replace with typed error once go-perun exposes one.
		} else {
			return errors.WithMessage(err, "coordinate: dispatch failed")
		}
	}

	ch.subChsAccess.Lock()
	ch.phase = PhaseCoordinated
	ch.subChsAccess.Unlock()

	return nil
}

// retrieveSignedSubStates mirrors retrieveLatestSubStates from the watcher.
//
// Walks parent.canonicalState.State.Locked in order (already DFS from register).
// For each locked sub-channel:
//   - live in registry  → selectCanonicalSignedState()
//   - de-registered     → archivedSubChStates (set during stopWatching)
func (c *CoordinatorHost) retrieveSignedSubStates(ch *coordCh) ([]channel.SignedState, error) {
	locked := ch.canonicalState.State.Locked
	subStates := make([]channel.SignedState, len(locked))

	for i, subAlloc := range locked {
		subID := subAlloc.ID

		// Mirrors: subCh, ok := r.retrieve(parentTx.Locked[i].ID)
		if subCh, ok := c.registry.retrieve(subID); ok {
			subCh.subChsAccess.Lock()
			ss := subCh.selectCanonicalSignedState()
			subCh.subChsAccess.Unlock()

			if ss.State == nil {
				return nil, errors.Errorf(
					"retrieve sub-states: no canonical state for sub-channel %x", subID)
			}
			subStates[i] = *ss
			continue
		}

		// Mirrors: parent.archivedSubChStates[id] fallback.
		archived, ok := ch.archivedSubChStates[subID]
		if !ok {
			return nil, errors.Errorf(
				"retrieve sub-states: sub-channel %x not found in registry or archive", subID)
		}
		subStates[i] = archived
	}

	return subStates, nil
}

// buildCoordSigs produces coordinator signatures in DFS order.
//
// Contract's coordinateRecursive reads coordSigs[nextIndex] starting at 0
// for the parent, then increments for each sub-channel in DFS traversal.
// For the MVP (flat sub-channel list, no nested subs):
//
//	coordSigs[0] = sig over parent state
//	coordSigs[1] = sig over subStates[0].State
//	coordSigs[2] = sig over subStates[1].State  ...
func (c *CoordinatorHost) buildCoordSigs(
	_ *coordCh,
	canonical *channel.State,
	subStates []channel.SignedState,
) ([]wallet.Sig, error) {
	coordSigs := make([]wallet.Sig, 0, 1+len(subStates))

	// coordSigs[0]: coordinator signs parent canonical state.
	parentSig, err := c.signState(canonical)
	if err != nil {
		return nil, errors.WithMessage(err, "signing parent canonical state")
	}
	coordSigs = append(coordSigs, parentSig)

	// coordSigs[1..n]: coordinator signs each sub-channel state in DFS order.
	for i, ss := range subStates {
		sig, err := c.signState(ss.State)
		if err != nil {
			return nil, errors.WithMessagef(err, "signing sub-channel state [%d]", i)
		}
		coordSigs = append(coordSigs, sig)
	}

	return coordSigs, nil
}

// isAlreadyConcluded checks whether a Coordinate error means the channel is
// already settled on-chain. Prefers the typed sentinel
// channel.ErrChannelAlreadyConcluded (exposed by go-perun for exactly this
// purpose) and falls back to a substring match on the on-chain revert reasons
// for cases where the eth-backend has not yet wrapped the error.
func isAlreadyConcluded(err error) bool {
	if err == nil {
		return false
	}
	if stderrors.Is(err, channel.ErrChannelAlreadyConcluded) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already") || strings.Contains(s, "concluded")
}

// signState signs the encoded state with the coordinator's wallet account.
// Uses channel.Backend.Sign which produces the same encoding as
// Channel.encodeState() in Adjudicator.sol, accepted by
// validateCoordinatorSignature() on-chain.
//
// With multiple backends registered (ETH=1, CKB=3) the map yields several
// accounts. ETH and CKB both sign Keccak256 of the same channel.State
// encoding with the same secp256k1 key, so the first successful Sign produces
// bytes the on-chain coordinate() verifier accepts on every chain.
func (c *CoordinatorHost) signState(state *channel.State) (wallet.Sig, error) {
	for b, acc := range c.acc {
		sig, err := channel.Sign(acc, state, b)
		if err == nil {
			return sig, nil
		}
	}
	return nil, errors.New("signState: no valid signature found for any account")
}
