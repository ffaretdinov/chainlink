package ocr2_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/abi/bind/backends"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/hashicorp/consul/sdk/freeport"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/libocr/commontypes"
	"github.com/smartcontractkit/libocr/gethwrappers2/ocr2aggregator"

	testoffchainaggregator2 "github.com/smartcontractkit/libocr/gethwrappers2/testocr2aggregator"
	confighelper2 "github.com/smartcontractkit/libocr/offchainreporting2plus/confighelper"
	ocrtypes2 "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink/v2/core/assets"
	"github.com/smartcontractkit/chainlink/v2/core/bridges"
	"github.com/smartcontractkit/chainlink/v2/core/chains/evm/forwarders"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/generated/authorized_forwarder"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/generated/link_token_interface"
	"github.com/smartcontractkit/chainlink/v2/core/internal/cltest"
	"github.com/smartcontractkit/chainlink/v2/core/internal/cltest/heavyweight"
	"github.com/smartcontractkit/chainlink/v2/core/internal/testutils"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	"github.com/smartcontractkit/chainlink/v2/core/services/chainlink"
	"github.com/smartcontractkit/chainlink/v2/core/services/keystore/keys/keystest"
	"github.com/smartcontractkit/chainlink/v2/core/services/keystore/keys/ocr2key"
	"github.com/smartcontractkit/chainlink/v2/core/services/ocr2/testhelpers"
	"github.com/smartcontractkit/chainlink/v2/core/services/ocr2/validate"
	"github.com/smartcontractkit/chainlink/v2/core/services/ocrbootstrap"
	"github.com/smartcontractkit/chainlink/v2/core/services/relay/evm"
	"github.com/smartcontractkit/chainlink/v2/core/store/models"
	"github.com/smartcontractkit/chainlink/v2/core/utils"
)

type ocr2Node struct {
	app                  *cltest.TestApplication
	peerID               string
	transmitter          common.Address
	effectiveTransmitter common.Address
	keybundle            ocr2key.KeyBundle
}

func setupOCR2Contracts(t *testing.T) (*bind.TransactOpts, *backends.SimulatedBackend, common.Address, *ocr2aggregator.OCR2Aggregator) {
	owner := testutils.MustNewSimTransactor(t)
	sb := new(big.Int)
	sb, _ = sb.SetString("100000000000000000000", 10) // 1 eth
	genesisData := core.GenesisAlloc{owner.From: {Balance: sb}}
	gasLimit := ethconfig.Defaults.Miner.GasCeil * 2
	b := backends.NewSimulatedBackend(genesisData, gasLimit)
	linkTokenAddress, _, linkContract, err := link_token_interface.DeployLinkToken(owner, b)
	require.NoError(t, err)
	accessAddress, _, _, err := testoffchainaggregator2.DeploySimpleWriteAccessController(owner, b)
	require.NoError(t, err, "failed to deploy test access controller contract")
	b.Commit()

	minAnswer, maxAnswer := new(big.Int), new(big.Int)
	minAnswer.Exp(big.NewInt(-2), big.NewInt(191), nil)
	maxAnswer.Exp(big.NewInt(2), big.NewInt(191), nil)
	maxAnswer.Sub(maxAnswer, big.NewInt(1))
	ocrContractAddress, _, ocrContract, err := ocr2aggregator.DeployOCR2Aggregator(
		owner,
		b,
		linkTokenAddress, //_link common.Address,
		minAnswer,        // -2**191
		maxAnswer,        // 2**191 - 1
		accessAddress,
		accessAddress,
		9,
		"TEST",
	)
	// Ensure we have finality depth worth of blocks to start.
	for i := 0; i < 20; i++ {
		b.Commit()
	}
	require.NoError(t, err)
	_, err = linkContract.Transfer(owner, ocrContractAddress, big.NewInt(1000))
	require.NoError(t, err)
	b.Commit()
	return owner, b, ocrContractAddress, ocrContract
}

func setupNodeOCR2(
	t *testing.T,
	owner *bind.TransactOpts,
	port int,
	useForwarder bool,
	b *backends.SimulatedBackend,
	p2pV2Bootstrappers []commontypes.BootstrapperLocator,
) *ocr2Node {
	p2pKey := keystest.NewP2PKeyV2(t)
	config, _ := heavyweight.FullTestDBV2(t, func(c *chainlink.Config, s *chainlink.Secrets) {
		c.Insecure.OCRDevelopmentMode = ptr(true) // Disables ocr spec validation so we can have fast polling for the test.

		c.Feature.LogPoller = ptr(true)

		c.OCR.Enabled = ptr(false)
		c.OCR2.Enabled = ptr(true)

		c.P2P.PeerID = ptr(p2pKey.PeerID())
		c.P2P.V1.Enabled = ptr(false)
		c.P2P.V2.Enabled = ptr(true)
		c.P2P.V2.DeltaDial = models.MustNewDuration(500 * time.Millisecond)
		c.P2P.V2.DeltaReconcile = models.MustNewDuration(5 * time.Second)
		c.P2P.V2.ListenAddresses = &[]string{fmt.Sprintf("127.0.0.1:%d", port)}
		if len(p2pV2Bootstrappers) > 0 {
			c.P2P.V2.DefaultBootstrappers = &p2pV2Bootstrappers
		}

		c.EVM[0].LogPollInterval = models.MustNewDuration(5 * time.Second)
		c.EVM[0].Transactions.ForwardersEnabled = &useForwarder
	})

	app := cltest.NewApplicationWithConfigV2AndKeyOnSimulatedBlockchain(t, config, b, p2pKey)

	sendingKeys, err := app.KeyStore.Eth().EnabledKeysForChain(testutils.SimulatedChainID)
	require.NoError(t, err)
	require.Len(t, sendingKeys, 1)
	transmitter := sendingKeys[0].Address
	effectiveTransmitter := sendingKeys[0].Address

	// Fund the transmitter address with some ETH
	n, err := b.NonceAt(testutils.Context(t), owner.From, nil)
	require.NoError(t, err)

	tx := types.NewTransaction(
		n, transmitter,
		assets.Ether(1).ToInt(),
		21000,
		assets.GWei(1).ToInt(),
		nil)
	signedTx, err := owner.Signer(owner.From, tx)
	require.NoError(t, err)
	err = b.SendTransaction(testutils.Context(t), signedTx)
	require.NoError(t, err)
	b.Commit()

	kb, err := app.GetKeyStore().OCR2().Create("evm")
	require.NoError(t, err)

	if useForwarder {
		// deploy a forwarder
		faddr, _, authorizedForwarder, err2 := authorized_forwarder.DeployAuthorizedForwarder(owner, b, common.HexToAddress("0x326C977E6efc84E512bB9C30f76E30c160eD06FB"), owner.From, common.Address{}, []byte{})
		require.NoError(t, err2)

		// set EOA as an authorized sender for the forwarder
		_, err2 = authorizedForwarder.SetAuthorizedSenders(owner, []common.Address{transmitter})
		require.NoError(t, err2)
		b.Commit()

		// add forwarder address to be tracked in db
		forwarderORM := forwarders.NewORM(app.GetSqlxDB(), logger.TestLogger(t), config.Database())
		chainID := utils.Big(*b.Blockchain().Config().ChainID)
		_, err2 = forwarderORM.CreateForwarder(faddr, chainID)
		require.NoError(t, err2)

		effectiveTransmitter = faddr
	}
	return &ocr2Node{
		app:                  app,
		peerID:               p2pKey.PeerID().Raw(),
		transmitter:          transmitter,
		effectiveTransmitter: effectiveTransmitter,
		keybundle:            kb,
	}
}

func TestIntegration_OCR2(t *testing.T) {
	t.Parallel()
	owner, b, ocrContractAddress, ocrContract := setupOCR2Contracts(t)

	lggr := logger.TestLogger(t)
	bootstrapNodePort := freeport.GetOne(t)
	bootstrapNode := setupNodeOCR2(t, owner, bootstrapNodePort, false /* useForwarders */, b, nil)

	var (
		oracles      []confighelper2.OracleIdentityExtra
		transmitters []common.Address
		kbs          []ocr2key.KeyBundle
		apps         []*cltest.TestApplication
	)
	ports := freeport.GetN(t, 4)
	for i := 0; i < 4; i++ {
		node := setupNodeOCR2(t, owner, ports[i], false /* useForwarders */, b, []commontypes.BootstrapperLocator{
			// Supply the bootstrap IP and port as a V2 peer address
			{PeerID: bootstrapNode.peerID, Addrs: []string{fmt.Sprintf("127.0.0.1:%d", bootstrapNodePort)}},
		})

		kbs = append(kbs, node.keybundle)
		apps = append(apps, node.app)
		transmitters = append(transmitters, node.transmitter)

		oracles = append(oracles, confighelper2.OracleIdentityExtra{
			OracleIdentity: confighelper2.OracleIdentity{
				OnchainPublicKey:  node.keybundle.PublicKey(),
				TransmitAccount:   ocrtypes2.Account(node.transmitter.String()),
				OffchainPublicKey: node.keybundle.OffchainPublicKey(),
				PeerID:            node.peerID,
			},
			ConfigEncryptionPublicKey: node.keybundle.ConfigEncryptionPublicKey(),
		})
	}

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	go func() {
		for range tick.C {
			b.Commit()
		}
	}()

	blockBeforeConfig := initOCR2(t, lggr, b, ocrContract, owner, bootstrapNode, oracles, transmitters, transmitters, func(blockNum int64) string {
		return fmt.Sprintf(`
type				= "bootstrap"
name				= "bootstrap"
relay				= "evm"
schemaVersion		= 1
contractID			= "%s"
[relayConfig]
chainID 			= 1337
fromBlock = %d
`, ocrContractAddress, blockNum)
	})

	var jids []int32
	var servers, slowServers = make([]*httptest.Server, 4), make([]*httptest.Server, 4)
	// We expect metadata of:
	//  latestAnswer:nil // First call
	//  latestAnswer:0
	//  latestAnswer:10
	//  latestAnswer:20
	//  latestAnswer:30
	var metaLock sync.Mutex
	expectedMeta := map[string]struct{}{
		"0": {}, "10": {}, "20": {}, "30": {},
	}
	for i := 0; i < 4; i++ {
		s := i
		require.NoError(t, apps[i].Start(testutils.Context(t)))

		// API speed is > observation timeout set in ContractSetConfigArgsForIntegrationTest
		slowServers[i] = httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
			time.Sleep(5 * time.Second)
			res.WriteHeader(http.StatusOK)
			_, err := res.Write([]byte(`{"data":10}`))
			require.NoError(t, err)
		}))
		t.Cleanup(slowServers[s].Close)
		servers[i] = httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
			b, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			var m bridges.BridgeMetaDataJSON
			require.NoError(t, json.Unmarshal(b, &m))
			if m.Meta.LatestAnswer != nil && m.Meta.UpdatedAt != nil {
				metaLock.Lock()
				delete(expectedMeta, m.Meta.LatestAnswer.String())
				metaLock.Unlock()
			}
			res.WriteHeader(http.StatusOK)
			_, err = res.Write([]byte(`{"data":10}`))
			require.NoError(t, err)
		}))
		t.Cleanup(servers[s].Close)
		u, _ := url.Parse(servers[i].URL)
		require.NoError(t, apps[i].BridgeORM().CreateBridgeType(&bridges.BridgeType{
			Name: bridges.BridgeName(fmt.Sprintf("bridge%d", i)),
			URL:  models.WebURL(*u),
		}))

		ocrJob, err := validate.ValidatedOracleSpecToml(apps[i].Config.OCR2(), apps[i].Config.Insecure(), fmt.Sprintf(`
type               = "offchainreporting2"
relay              = "evm"
schemaVersion      = 1
pluginType         = "median"
name               = "web oracle spec"
contractID         = "%s"
ocrKeyBundleID     = "%s"
transmitterID      = "%s"
contractConfigConfirmations = 1
contractConfigTrackerPollInterval = "1s"
observationSource  = """
    // data source 1
    ds1          [type=bridge name="%s"];
    ds1_parse    [type=jsonparse path="data"];
    ds1_multiply [type=multiply times=%d];

    // data source 2
    ds2          [type=http method=GET url="%s"];
    ds2_parse    [type=jsonparse path="data"];
    ds2_multiply [type=multiply times=%d];

    ds1 -> ds1_parse -> ds1_multiply -> answer1;
    ds2 -> ds2_parse -> ds2_multiply -> answer1;

	answer1 [type=median index=0];
"""
[relayConfig]
chainID = 1337
fromBlock = %d
[pluginConfig]
juelsPerFeeCoinSource = """
		// data source 1
		ds1          [type=bridge name="%s"];
		ds1_parse    [type=jsonparse path="data"];
		ds1_multiply [type=multiply times=%d];

		// data source 2
		ds2          [type=http method=GET url="%s"];
		ds2_parse    [type=jsonparse path="data"];
		ds2_multiply [type=multiply times=%d];

		ds1 -> ds1_parse -> ds1_multiply -> answer1;
		ds2 -> ds2_parse -> ds2_multiply -> answer1;

	answer1 [type=median index=0];
"""
`, ocrContractAddress, kbs[i].ID(), transmitters[i], fmt.Sprintf("bridge%d", i), i, slowServers[i].URL, i, blockBeforeConfig.Number().Int64(), fmt.Sprintf("bridge%d", i), i, slowServers[i].URL, i))
		require.NoError(t, err)
		err = apps[i].AddJobV2(testutils.Context(t), &ocrJob)
		require.NoError(t, err)
		jids = append(jids, ocrJob.ID)
	}

	// Assert that all the OCR jobs get a run with valid values eventually.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		ic := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Want at least 2 runs so we see all the metadata.
			pr := cltest.WaitForPipelineComplete(t, ic, jids[ic], 2, 7, apps[ic].JobORM(), 2*time.Minute, 5*time.Second)
			jb, err := pr[0].Outputs.MarshalJSON()
			require.NoError(t, err)
			assert.Equal(t, []byte(fmt.Sprintf("[\"%d\"]", 10*ic)), jb, "pr[0] %+v pr[1] %+v", pr[0], pr[1])
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	// 4 oracles reporting 0, 10, 20, 30. Answer should be 20 (results[4/2]).
	gomega.NewGomegaWithT(t).Eventually(func() string {
		answer, err := ocrContract.LatestAnswer(nil)
		require.NoError(t, err)
		return answer.String()
	}, 1*time.Minute, 200*time.Millisecond).Should(gomega.Equal("20"))

	for _, app := range apps {
		jobs, _, err := app.JobORM().FindJobs(0, 1000)
		require.NoError(t, err)
		// No spec errors
		for _, j := range jobs {
			ignore := 0
			for i := range j.JobSpecErrors {
				// Non-fatal timing related error, ignore for testing.
				if strings.Contains(j.JobSpecErrors[i].Description, "leader's phase conflicts tGrace timeout") {
					ignore++
				}
			}
			require.Len(t, j.JobSpecErrors, ignore)
		}
	}
	em := map[string]struct{}{}
	metaLock.Lock()
	maps.Copy(em, expectedMeta)
	metaLock.Unlock()
	assert.Len(t, em, 0, "expected metadata %v", em)

	// Assert we can read the latest config digest and epoch after a report has been submitted.
	contractABI, err := abi.JSON(strings.NewReader(ocr2aggregator.OCR2AggregatorABI))
	require.NoError(t, err)
	apps[0].GetRelayers().LegacyEVMChains().Slice()
	ct, err := evm.NewOCRContractTransmitter(ocrContractAddress, apps[0].GetRelayers().LegacyEVMChains().Slice()[0].Client(), contractABI, nil, apps[0].GetRelayers().LegacyEVMChains().Slice()[0].LogPoller(), lggr, nil)
	require.NoError(t, err)
	configDigest, epoch, err := ct.LatestConfigDigestAndEpoch(testutils.Context(t))
	require.NoError(t, err)
	details, err := ocrContract.LatestConfigDetails(nil)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(configDigest[:], details.ConfigDigest[:]))
	digestAndEpoch, err := ocrContract.LatestConfigDigestAndEpoch(nil)
	require.NoError(t, err)
	assert.Equal(t, digestAndEpoch.Epoch, epoch)
}

func initOCR2(t *testing.T, lggr logger.Logger, b *backends.SimulatedBackend,
	ocrContract *ocr2aggregator.OCR2Aggregator,
	owner *bind.TransactOpts,
	bootstrapNode *ocr2Node,
	oracles []confighelper2.OracleIdentityExtra,
	transmitters []common.Address,
	payees []common.Address,
	specFn func(int64) string,
) (
	blockBeforeConfig *types.Block,
) {
	lggr.Debugw("Setting Payees on OraclePlugin Contract", "transmitters", payees)
	_, err := ocrContract.SetPayees(
		owner,
		transmitters,
		payees,
	)
	require.NoError(t, err)
	blockBeforeConfig, err = b.BlockByNumber(testutils.Context(t), nil)
	require.NoError(t, err)
	signers, effectiveTransmitters, threshold, _, encodedConfigVersion, encodedConfig, err := confighelper2.ContractSetConfigArgsForEthereumIntegrationTest(
		oracles,
		1,
		1000000000/100, // threshold PPB
	)
	require.NoError(t, err)

	minAnswer, maxAnswer := new(big.Int), new(big.Int)
	minAnswer.Exp(big.NewInt(-2), big.NewInt(191), nil)
	maxAnswer.Exp(big.NewInt(2), big.NewInt(191), nil)
	maxAnswer.Sub(maxAnswer, big.NewInt(1))

	onchainConfig, err := testhelpers.GenerateDefaultOCR2OnchainConfig(minAnswer, maxAnswer)
	require.NoError(t, err)

	lggr.Debugw("Setting Config on Oracle Contract",
		"signers", signers,
		"transmitters", transmitters,
		"effectiveTransmitters", effectiveTransmitters,
		"threshold", threshold,
		"onchainConfig", onchainConfig,
		"encodedConfigVersion", encodedConfigVersion,
	)
	_, err = ocrContract.SetConfig(
		owner,
		signers,
		effectiveTransmitters,
		threshold,
		onchainConfig,
		encodedConfigVersion,
		encodedConfig,
	)
	require.NoError(t, err)
	b.Commit()

	err = bootstrapNode.app.Start(testutils.Context(t))
	require.NoError(t, err)

	chainSet := bootstrapNode.app.GetRelayers().LegacyEVMChains()
	require.NotNil(t, chainSet)
	ocrJob, err := ocrbootstrap.ValidatedBootstrapSpecToml(specFn(blockBeforeConfig.Number().Int64()))
	require.NoError(t, err)
	err = bootstrapNode.app.AddJobV2(testutils.Context(t), &ocrJob)
	require.NoError(t, err)
	return
}

func TestIntegration_OCR2_ForwarderFlow(t *testing.T) {
	t.Parallel()
	owner, b, ocrContractAddress, ocrContract := setupOCR2Contracts(t)

	lggr := logger.TestLogger(t)
	bootstrapNodePort := freeport.GetOne(t)
	bootstrapNode := setupNodeOCR2(t, owner, bootstrapNodePort, true /* useForwarders */, b, nil)

	var (
		oracles            []confighelper2.OracleIdentityExtra
		transmitters       []common.Address
		forwarderContracts []common.Address
		kbs                []ocr2key.KeyBundle
		apps               []*cltest.TestApplication
	)
	ports := freeport.GetN(t, 4)
	for i := uint16(0); i < 4; i++ {
		node := setupNodeOCR2(t, owner, ports[i], true /* useForwarders */, b, []commontypes.BootstrapperLocator{
			// Supply the bootstrap IP and port as a V2 peer address
			{PeerID: bootstrapNode.peerID, Addrs: []string{fmt.Sprintf("127.0.0.1:%d", bootstrapNodePort)}},
		})

		// Effective transmitter should be a forwarder not an EOA.
		require.NotEqual(t, node.effectiveTransmitter, node.transmitter)

		kbs = append(kbs, node.keybundle)
		apps = append(apps, node.app)
		forwarderContracts = append(forwarderContracts, node.effectiveTransmitter)
		transmitters = append(transmitters, node.transmitter)

		oracles = append(oracles, confighelper2.OracleIdentityExtra{
			OracleIdentity: confighelper2.OracleIdentity{
				OnchainPublicKey:  node.keybundle.PublicKey(),
				TransmitAccount:   ocrtypes2.Account(node.effectiveTransmitter.String()),
				OffchainPublicKey: node.keybundle.OffchainPublicKey(),
				PeerID:            node.peerID,
			},
			ConfigEncryptionPublicKey: node.keybundle.ConfigEncryptionPublicKey(),
		})
	}

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	go func() {
		for range tick.C {
			b.Commit()
		}
	}()

	blockBeforeConfig := initOCR2(t, lggr, b, ocrContract, owner, bootstrapNode, oracles, forwarderContracts, transmitters, func(int64) string {
		return fmt.Sprintf(`
type				= "bootstrap"
name				= "bootstrap"
relay				= "evm"
schemaVersion		= 1
forwardingAllowed   = true
contractID			= "%s"
[relayConfig]
chainID 			= 1337
`, ocrContractAddress)
	})

	var jids []int32
	var servers, slowServers = make([]*httptest.Server, 4), make([]*httptest.Server, 4)
	// We expect metadata of:
	//  latestAnswer:nil // First call
	//  latestAnswer:0
	//  latestAnswer:10
	//  latestAnswer:20
	//  latestAnswer:30
	var metaLock sync.Mutex
	expectedMeta := map[string]struct{}{
		"0": {}, "10": {}, "20": {}, "30": {},
	}
	for i := 0; i < 4; i++ {
		s := i
		require.NoError(t, apps[i].Start(testutils.Context(t)))

		// API speed is > observation timeout set in ContractSetConfigArgsForIntegrationTest
		slowServers[i] = httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
			time.Sleep(5 * time.Second)
			res.WriteHeader(http.StatusOK)
			_, err := res.Write([]byte(`{"data":10}`))
			require.NoError(t, err)
		}))
		t.Cleanup(func() {
			slowServers[s].Close()
		})
		servers[i] = httptest.NewServer(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
			b, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			var m bridges.BridgeMetaDataJSON
			require.NoError(t, json.Unmarshal(b, &m))
			if m.Meta.LatestAnswer != nil && m.Meta.UpdatedAt != nil {
				metaLock.Lock()
				delete(expectedMeta, m.Meta.LatestAnswer.String())
				metaLock.Unlock()
			}
			res.WriteHeader(http.StatusOK)
			_, err = res.Write([]byte(`{"data":10}`))
			require.NoError(t, err)
		}))
		t.Cleanup(func() {
			servers[s].Close()
		})
		u, _ := url.Parse(servers[i].URL)
		require.NoError(t, apps[i].BridgeORM().CreateBridgeType(&bridges.BridgeType{
			Name: bridges.BridgeName(fmt.Sprintf("bridge%d", i)),
			URL:  models.WebURL(*u),
		}))

		ocrJob, err := validate.ValidatedOracleSpecToml(apps[i].Config.OCR2(), apps[i].Config.Insecure(), fmt.Sprintf(`
type               = "offchainreporting2"
relay              = "evm"
schemaVersion      = 1
pluginType         = "median"
name               = "web oracle spec"
forwardingAllowed  = true
contractID         = "%s"
ocrKeyBundleID     = "%s"
transmitterID      = "%s"
contractConfigConfirmations = 1
contractConfigTrackerPollInterval = "1s"
observationSource  = """
    // data source 1
    ds1          [type=bridge name="%s"];
    ds1_parse    [type=jsonparse path="data"];
    ds1_multiply [type=multiply times=%d];

    // data source 2
    ds2          [type=http method=GET url="%s"];
    ds2_parse    [type=jsonparse path="data"];
    ds2_multiply [type=multiply times=%d];

    ds1 -> ds1_parse -> ds1_multiply -> answer1;
    ds2 -> ds2_parse -> ds2_multiply -> answer1;

	answer1 [type=median index=0];
"""
[relayConfig]
chainID = 1337
[pluginConfig]
juelsPerFeeCoinSource = """
		// data source 1
		ds1          [type=bridge name="%s"];
		ds1_parse    [type=jsonparse path="data"];
		ds1_multiply [type=multiply times=%d];

		// data source 2
		ds2          [type=http method=GET url="%s"];
		ds2_parse    [type=jsonparse path="data"];
		ds2_multiply [type=multiply times=%d];

		ds1 -> ds1_parse -> ds1_multiply -> answer1;
		ds2 -> ds2_parse -> ds2_multiply -> answer1;

	answer1 [type=median index=0];
"""
`, ocrContractAddress, kbs[i].ID(), transmitters[i], fmt.Sprintf("bridge%d", i), i, slowServers[i].URL, i, fmt.Sprintf("bridge%d", i), i, slowServers[i].URL, i))
		require.NoError(t, err)
		err = apps[i].AddJobV2(testutils.Context(t), &ocrJob)
		require.NoError(t, err)
		jids = append(jids, ocrJob.ID)
	}

	// Once all the jobs are added, replay to ensure we have the configSet logs.
	for _, app := range apps {
		require.NoError(t, app.GetRelayers().LegacyEVMChains().Slice()[0].LogPoller().Replay(testutils.Context(t), blockBeforeConfig.Number().Int64()))
	}
	require.NoError(t, bootstrapNode.app.GetRelayers().LegacyEVMChains().Slice()[0].LogPoller().Replay(testutils.Context(t), blockBeforeConfig.Number().Int64()))

	// Assert that all the OCR jobs get a run with valid values eventually.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		ic := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Want at least 2 runs so we see all the metadata.
			pr := cltest.WaitForPipelineComplete(t, ic, jids[ic], 2, 7, apps[ic].JobORM(), 2*time.Minute, 5*time.Second)
			jb, err := pr[0].Outputs.MarshalJSON()
			require.NoError(t, err)
			assert.Equal(t, []byte(fmt.Sprintf("[\"%d\"]", 10*ic)), jb, "pr[0] %+v pr[1] %+v", pr[0], pr[1])
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	// 4 oracles reporting 0, 10, 20, 30. Answer should be 20 (results[4/2]).
	gomega.NewGomegaWithT(t).Eventually(func() string {
		answer, err := ocrContract.LatestAnswer(nil)
		require.NoError(t, err)
		return answer.String()
	}, 1*time.Minute, 200*time.Millisecond).Should(gomega.Equal("20"))

	for _, app := range apps {
		jobs, _, err := app.JobORM().FindJobs(0, 1000)
		require.NoError(t, err)
		// No spec errors
		for _, j := range jobs {
			ignore := 0
			for i := range j.JobSpecErrors {
				// Non-fatal timing related error, ignore for testing.
				if strings.Contains(j.JobSpecErrors[i].Description, "leader's phase conflicts tGrace timeout") {
					ignore++
				}
			}
			require.Len(t, j.JobSpecErrors, ignore)
		}
	}
	em := map[string]struct{}{}
	metaLock.Lock()
	maps.Copy(em, expectedMeta)
	metaLock.Unlock()
	assert.Len(t, em, 0, "expected metadata %v", em)

	// Assert we can read the latest config digest and epoch after a report has been submitted.
	contractABI, err := abi.JSON(strings.NewReader(ocr2aggregator.OCR2AggregatorABI))
	require.NoError(t, err)
	ct, err := evm.NewOCRContractTransmitter(ocrContractAddress, apps[0].GetRelayers().LegacyEVMChains().Slice()[0].Client(), contractABI, nil, apps[0].GetRelayers().LegacyEVMChains().Slice()[0].LogPoller(), lggr, nil)
	require.NoError(t, err)
	configDigest, epoch, err := ct.LatestConfigDigestAndEpoch(testutils.Context(t))
	require.NoError(t, err)
	details, err := ocrContract.LatestConfigDetails(nil)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(configDigest[:], details.ConfigDigest[:]))
	digestAndEpoch, err := ocrContract.LatestConfigDigestAndEpoch(nil)
	require.NoError(t, err)
	assert.Equal(t, digestAndEpoch.Epoch, epoch)
}

func ptr[T any](v T) *T { return &v }
