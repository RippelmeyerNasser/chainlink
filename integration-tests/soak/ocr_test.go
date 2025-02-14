package soak

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink-testing-framework/logging"
	"github.com/smartcontractkit/chainlink-testing-framework/networks"

	"github.com/smartcontractkit/chainlink/integration-tests/actions"
	"github.com/smartcontractkit/chainlink/integration-tests/config"
	"github.com/smartcontractkit/chainlink/integration-tests/testsetups"
)

func TestOCRSoak(t *testing.T) {
	l := logging.GetTestLogger(t)
	// Use this variable to pass in any custom EVM specific TOML values to your Chainlink nodes
	customNetworkTOML := ``
	// Uncomment below for debugging TOML issues on the node
	network := networks.MustGetSelectedNetworksFromEnv()[0]
	fmt.Println("Using Chainlink TOML\n---------------------")
	fmt.Println(networks.AddNetworkDetailedConfig(config.BaseOCR1Config, customNetworkTOML, network))
	fmt.Println("---------------------")

	ocrSoakTest, err := testsetups.NewOCRSoakTest(t, false)
	require.NoError(t, err, "Error creating soak test")
	if !ocrSoakTest.Interrupted() {
		ocrSoakTest.DeployEnvironment(customNetworkTOML)
	}
	if ocrSoakTest.Environment().WillUseRemoteRunner() {
		return
	}
	t.Cleanup(func() {
		if err := actions.TeardownRemoteSuite(ocrSoakTest.TearDownVals(t)); err != nil {
			l.Error().Err(err).Msg("Error tearing down environment")
		}
	})
	if ocrSoakTest.Interrupted() {
		err = ocrSoakTest.LoadState()
		require.NoError(t, err, "Error loading state")
		ocrSoakTest.Resume()
	} else {
		ocrSoakTest.Setup()
		ocrSoakTest.Run()
	}
}
