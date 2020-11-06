package runtime

import (
	"context"
	"fmt"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/oasis-node/cmd/debug/byzantine"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/env"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/log"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/oasis"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/scenario"
)

// ByzantineBeaconHonest is the byzantine beacon honest scenario.
var ByzantineBeaconHonest scenario.Scenario = newByzantineBeaconImpl(
	"beacon-honest",
	nil,
	oasis.ByzantineDefaultIdentitySeed,
	[]string{
		"--" + byzantine.CfgBeaconMode, byzantine.ModeBeaconHonest.String(),
	},
)

type byzantineBeaconImpl struct {
	*byzantineImpl
}

func newByzantineBeaconImpl(
	name string,
	logWatcherHandlerFactories []log.WatcherHandlerFactory,
	identitySeed string,
	extraArgs []string,
) scenario.Scenario {
	inner := newByzantineImpl(
		name,
		"beacon",
		logWatcherHandlerFactories,
		identitySeed,
		extraArgs,
	)
	return &byzantineBeaconImpl{inner.(*byzantineImpl)}
}

func (sc *byzantineBeaconImpl) Fixture() (*oasis.NetworkFixture, error) {
	f, err := sc.byzantineImpl.Fixture()
	if err != nil {
		return nil, err
	}

	// Use really ugly hacks to force the byzantine node to participate.
	if l := len(f.ByzantineNodes); l != 1 {
		return nil, fmt.Errorf("byzantine/beacon: unexpected number of byzantine nodes: %d", l)
	}
	node := f.ByzantineNodes[0]
	pks, err := oasis.GenerateDeterministicNodeKeys(nil, node.IdentitySeed, []signature.SignerRole{signature.SignerNode})
	if err != nil {
		return nil, fmt.Errorf("byzantine/beacon: failed to derive node identity: %w", err)
	}
	f.Network.Beacon.SCRAPEParameters = &beacon.SCRAPEParameters{
		DebugForcedParticipants: []signature.PublicKey{
			pks[0],
		},
	}

	return f, nil
}

func (sc *byzantineBeaconImpl) Run(childEnv *env.Env) error {
	clientErrCh, cmd, err := sc.runtimeImpl.start(childEnv)
	if err != nil {
		return err
	}

	fixture, err := sc.Fixture()
	if err != nil {
		return err
	}

	if err = sc.initialEpochTransitions(fixture); err != nil {
		return err
	}

	// Force some more epoch transitions.
	if err = sc.Net.Controller().SetEpoch(context.Background(), 3); err != nil {
		return err
	}
	if err = sc.Net.Controller().SetEpoch(context.Background(), 4); err != nil {
		return err
	}
	if err = sc.Net.Controller().SetEpoch(context.Background(), 5); err != nil {
		return err
	}

	// TODO: As far as I can tell, there is no way to enforce that the
	// byzantine node did the right thing, because the log watcher crap
	// gets applied to all nodes.

	return sc.wait(childEnv, cmd, clientErrCh)
}
