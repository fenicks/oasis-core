package runtime

import (
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/env"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/scenario"
)

// DumpRestore is the dump and restore scenario.
var DumpRestore scenario.Scenario = newDumpRestoreImpl()

type dumpRestoreImpl struct {
	runtimeImpl
}

func newDumpRestoreImpl() scenario.Scenario {
	sc := &dumpRestoreImpl{
		runtimeImpl: *newRuntimeImpl(
			"dump-restore",
			"test-long-term-client",
			// Use -nomsg variant as this test also compares with the database dump which cannot
			// reconstruct the emitted messages as those are not available in the state dump alone.
			[]string{"--mode", "part1-nomsg"},
		),
	}
	return sc
}

func (sc *dumpRestoreImpl) Clone() scenario.Scenario {
	return &dumpRestoreImpl{
		runtimeImpl: *sc.runtimeImpl.Clone().(*runtimeImpl),
	}
}

func (sc *dumpRestoreImpl) Run(childEnv *env.Env) error {
	clientErrCh, cmd, err := sc.start(childEnv)
	if err != nil {
		return err
	}

	// Wait for the client to exit.
	select {
	case err = <-sc.Net.Errors():
		_ = cmd.Process.Kill()
	case err = <-clientErrCh:
	}
	if err != nil {
		return err
	}

	// Dump restore network.
	fixture, err := sc.Fixture()
	if err != nil {
		return err
	}
	if err = sc.DumpRestoreNetwork(childEnv, fixture, true); err != nil {
		return err
	}

	// Check that everything works with restored state.
	sc.runtimeImpl.clientArgs = []string{"--mode", "part2"}
	return sc.runtimeImpl.Run(childEnv)
}
