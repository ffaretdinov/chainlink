package performance

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"

	"github.com/smartcontractkit/chainlink-testing-framework/blockchain"
	ctfClient "github.com/smartcontractkit/chainlink-testing-framework/client"
	"github.com/smartcontractkit/chainlink-testing-framework/k8s/environment"
	"github.com/smartcontractkit/chainlink-testing-framework/k8s/pkg/helm/chainlink"
	"github.com/smartcontractkit/chainlink-testing-framework/k8s/pkg/helm/ethereum"
	"github.com/smartcontractkit/chainlink-testing-framework/k8s/pkg/helm/mockserver"
	mockservercfg "github.com/smartcontractkit/chainlink-testing-framework/k8s/pkg/helm/mockserver-cfg"
	"github.com/smartcontractkit/chainlink-testing-framework/logging"

	"github.com/smartcontractkit/chainlink-testing-framework/networks"

	"github.com/smartcontractkit/chainlink/integration-tests/actions"
	"github.com/smartcontractkit/chainlink/integration-tests/client"
	"github.com/smartcontractkit/chainlink/integration-tests/testreporters"
	"github.com/smartcontractkit/chainlink/integration-tests/testsetups"
)

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func CleanupPerformanceTest(
	t *testing.T,
	testEnvironment *environment.Environment,
	chainlinkNodes []*client.ChainlinkK8sClient,
	testReporter testreporters.ChainlinkProfileTestReporter,
	chainClient blockchain.EVMClient,
) {
	if chainClient != nil {
		chainClient.GasStats().PrintStats()
	}
	err := actions.TeardownSuite(t, testEnvironment, chainlinkNodes, &testReporter, zapcore.PanicLevel, chainClient)
	require.NoError(t, err, "Error tearing down environment")
}

func TestCronPerformance(t *testing.T) {
	testEnvironment := setupCronTest(t)
	if testEnvironment.WillUseRemoteRunner() {
		return
	}

	chainlinkNodes, err := client.ConnectChainlinkNodes(testEnvironment)
	require.NoError(t, err, "Connecting to chainlink nodes shouldn't fail")
	mockServer, err := ctfClient.ConnectMockServer(testEnvironment)
	require.NoError(t, err, "Creating mockserver client shouldn't fail")
	chainlinkNode := chainlinkNodes[0]
	err = mockServer.SetValuePath("/variable", 5)
	require.NoError(t, err, "Setting value path in mockserver shouldn't fail")

	profileFunction := func(chainlinkNode *client.ChainlinkClient) {
		if chainlinkNode != chainlinkNodes[len(chainlinkNodes)-1].ChainlinkClient {
			// Not the last node, hence not all nodes started profiling yet.
			return
		}
		bta := &client.BridgeTypeAttributes{
			Name:        fmt.Sprintf("variable-%s", uuid.New().String()),
			URL:         fmt.Sprintf("%s/variable", mockServer.Config.ClusterURL),
			RequestData: "{}",
		}
		err = chainlinkNode.MustCreateBridge(bta)
		require.NoError(t, err, "Creating bridge in chainlink node shouldn't fail")

		job, err := chainlinkNode.MustCreateJob(&client.CronJobSpec{
			Schedule:          "CRON_TZ=UTC * * * * * *",
			ObservationSource: client.ObservationSourceSpecBridge(bta),
		})
		require.NoError(t, err, "Creating Cron Job in chainlink node shouldn't fail")

		gom := gomega.NewGomegaWithT(t)
		gom.Eventually(func(g gomega.Gomega) {
			jobRuns, err := chainlinkNode.MustReadRunsByJob(job.Data.ID)
			g.Expect(err).ShouldNot(gomega.HaveOccurred(), "Reading Job run data shouldn't fail")

			g.Expect(len(jobRuns.Data)).Should(gomega.BeNumerically(">=", 5), "Expected number of job runs to be greater than 5, but got %d", len(jobRuns.Data))

			for _, jr := range jobRuns.Data {
				g.Expect(jr.Attributes.Errors).Should(gomega.Equal([]interface{}{nil}), "Job run %s shouldn't have errors", jr.ID)
			}
		}, "2m", "1s").Should(gomega.Succeed())
	}

	profileTest := testsetups.NewChainlinkProfileTest(testsetups.ChainlinkProfileTestInputs{
		ProfileFunction: profileFunction,
		ProfileDuration: 30 * time.Second,
		ChainlinkNodes:  []*client.ChainlinkK8sClient{chainlinkNode},
	})
	// Register cleanup for any test
	t.Cleanup(func() {
		CleanupPerformanceTest(t, testEnvironment, chainlinkNodes, profileTest.TestReporter, nil)
	})
	profileTest.Setup(testEnvironment)
	profileTest.Run()
}

func setupCronTest(t *testing.T) (testEnvironment *environment.Environment) {
	logging.Init()
	network := networks.MustGetSelectedNetworksFromEnv()[0]
	evmConfig := ethereum.New(nil)
	if !network.Simulated {
		evmConfig = ethereum.New(&ethereum.Props{
			NetworkName: network.Name,
			Simulated:   network.Simulated,
			WsURLs:      network.URLs,
		})
	}
	baseTOML := `[WebServer]
HTTPWriteTimout = '300s'`
	cd := chainlink.New(0, map[string]interface{}{
		"replicas": 1,
		"toml":     client.AddNetworksConfig(baseTOML, network),
	})

	testEnvironment = environment.New(&environment.Config{
		NamespacePrefix:    fmt.Sprintf("performance-cron-%s", strings.ReplaceAll(strings.ToLower(network.Name), " ", "-")),
		Test:               t,
		PreventPodEviction: true,
	}).
		AddHelm(mockservercfg.New(nil)).
		AddHelm(mockserver.New(nil)).
		AddHelm(evmConfig).
		AddHelm(cd)
	err := testEnvironment.Run()
	require.NoError(t, err, "Error launching test environment")
	return testEnvironment
}
