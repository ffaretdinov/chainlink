package ocr

import (
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"

	"github.com/jmoiron/sqlx"

	relaylogger "github.com/smartcontractkit/chainlink-relay/pkg/logger"

	"github.com/smartcontractkit/libocr/gethwrappers/offchainaggregator"
	ocrnetworking "github.com/smartcontractkit/libocr/networking"
	ocr "github.com/smartcontractkit/libocr/offchainreporting"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting/types"

	txmgrcommon "github.com/smartcontractkit/chainlink/v2/common/txmgr"
	"github.com/smartcontractkit/chainlink/v2/core/chains/evm"
	"github.com/smartcontractkit/chainlink/v2/core/chains/evm/txmgr"
	"github.com/smartcontractkit/chainlink/v2/core/gethwrappers/generated/offchain_aggregator_wrapper"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	"github.com/smartcontractkit/chainlink/v2/core/services/job"
	"github.com/smartcontractkit/chainlink/v2/core/services/keystore"
	"github.com/smartcontractkit/chainlink/v2/core/services/keystore/keys/ethkey"
	"github.com/smartcontractkit/chainlink/v2/core/services/ocrcommon"
	"github.com/smartcontractkit/chainlink/v2/core/services/pg"
	"github.com/smartcontractkit/chainlink/v2/core/services/pipeline"
	"github.com/smartcontractkit/chainlink/v2/core/services/synchronization"
	"github.com/smartcontractkit/chainlink/v2/core/services/telemetry"
	"github.com/smartcontractkit/chainlink/v2/core/utils"
)

type Delegate struct {
	db                    *sqlx.DB
	jobORM                job.ORM
	keyStore              keystore.Master
	pipelineRunner        pipeline.Runner
	peerWrapper           *ocrcommon.SingletonPeerWrapper
	monitoringEndpointGen telemetry.MonitoringEndpointGenerator
	legacyChains          evm.LegacyChainContainer
	lggr                  logger.Logger
	cfg                   Config
	mailMon               *utils.MailboxMonitor
}

var _ job.Delegate = (*Delegate)(nil)

const ConfigOverriderPollInterval = 30 * time.Second

func NewDelegate(
	db *sqlx.DB,
	jobORM job.ORM,
	keyStore keystore.Master,
	pipelineRunner pipeline.Runner,
	peerWrapper *ocrcommon.SingletonPeerWrapper,
	monitoringEndpointGen telemetry.MonitoringEndpointGenerator,
	legacyChains evm.LegacyChainContainer,
	lggr logger.Logger,
	cfg Config,
	mailMon *utils.MailboxMonitor,
) *Delegate {
	return &Delegate{
		db:                    db,
		jobORM:                jobORM,
		keyStore:              keyStore,
		pipelineRunner:        pipelineRunner,
		peerWrapper:           peerWrapper,
		monitoringEndpointGen: monitoringEndpointGen,
		legacyChains:          legacyChains,
		lggr:                  lggr.Named("OCR"),
		cfg:                   cfg,
		mailMon:               mailMon,
	}
}

func (d *Delegate) JobType() job.Type {
	return job.OffchainReporting
}

func (d *Delegate) BeforeJobCreated(spec job.Job)                {}
func (d *Delegate) AfterJobCreated(spec job.Job)                 {}
func (d *Delegate) BeforeJobDeleted(spec job.Job)                {}
func (d *Delegate) OnDeleteJob(spec job.Job, q pg.Queryer) error { return nil }

// ServicesForSpec returns the OCR services that need to run for this job
func (d *Delegate) ServicesForSpec(jb job.Job) (services []job.ServiceCtx, err error) {
	if jb.OCROracleSpec == nil {
		return nil, errors.Errorf("offchainreporting.Delegate expects an *job.OffchainreportingOracleSpec to be present, got %v", jb)
	}
	chain, err := d.legacyChains.Get(jb.OCROracleSpec.EVMChainID.String())
	if err != nil {
		return nil, err
	}
	concreteSpec, err := job.LoadConfigVarsOCR(chain.Config().EVM().OCR(), chain.Config().OCR(), *jb.OCROracleSpec)
	if err != nil {
		return nil, err
	}
	lggr := d.lggr.With(
		"contractAddress", concreteSpec.ContractAddress,
		"jobName", jb.Name.ValueOrZero(),
		"jobID", jb.ID,
		"externalJobID", jb.ExternalJobID)

	contract, err := offchain_aggregator_wrapper.NewOffchainAggregator(concreteSpec.ContractAddress.Address(), chain.Client())
	if err != nil {
		return nil, errors.Wrap(err, "could not instantiate NewOffchainAggregator")
	}

	contractFilterer, err := offchainaggregator.NewOffchainAggregatorFilterer(concreteSpec.ContractAddress.Address(), chain.Client())
	if err != nil {
		return nil, errors.Wrap(err, "could not instantiate NewOffchainAggregatorFilterer")
	}

	contractCaller, err := offchainaggregator.NewOffchainAggregatorCaller(concreteSpec.ContractAddress.Address(), chain.Client())
	if err != nil {
		return nil, errors.Wrap(err, "could not instantiate NewOffchainAggregatorCaller")
	}

	ocrDB := NewDB(d.db, concreteSpec.ID, lggr, d.cfg)

	tracker := NewOCRContractTracker(
		contract,
		contractFilterer,
		contractCaller,
		chain.Client(),
		chain.LogBroadcaster(),
		jb.ID,
		lggr,
		d.db,
		ocrDB,
		chain.Config().EVM(),
		chain.Config().Database(),
		chain.HeadBroadcaster(),
		d.mailMon,
	)
	services = append(services, tracker)

	peerWrapper := d.peerWrapper
	if peerWrapper == nil {
		return nil, errors.New("cannot setup OCR job service, libp2p peer was missing")
	} else if !peerWrapper.IsStarted() {
		return nil, errors.New("peerWrapper is not started. OCR jobs require a started and running p2p peer")
	}

	var v1BootstrapPeers []string
	if concreteSpec.P2PBootstrapPeers != nil {
		v1BootstrapPeers = concreteSpec.P2PBootstrapPeers
	} else {
		v1BootstrapPeers, err = chain.Config().P2P().V1().DefaultBootstrapPeers()
		if err != nil {
			return nil, err
		}
	}

	v2Bootstrappers, err := ocrcommon.ParseBootstrapPeers(concreteSpec.P2PV2Bootstrappers)
	if err != nil {
		return nil, err
	} else if len(v2Bootstrappers) == 0 {
		// ParseBootstrapPeers() does not distinguish between no p2pv2Bootstrappers field
		//  present in job spec, and p2pv2Bootstrappers = [].  So even if an empty list is
		//  passed explicitly, this will still fall back to using the V2 bootstappers defined
		//  in P2P.V2.DefaultBootstrappers config var.  Only a non-empty list will override the default list.
		v2Bootstrappers = peerWrapper.P2PConfig().V2().DefaultBootstrappers()
	}

	ocrLogger := relaylogger.NewOCRWrapper(lggr, chain.Config().OCR().TraceLogging(), func(msg string) {
		d.jobORM.TryRecordError(jb.ID, msg)
	})

	lc := toLocalConfig(chain.Config().EVM(), chain.Config().EVM().OCR(), chain.Config().Insecure(), *concreteSpec, chain.Config().OCR())
	if err = ocr.SanityCheckLocalConfig(lc); err != nil {
		return nil, err
	}
	lggr.Info(fmt.Sprintf("OCR job using local config %+v", lc))

	if concreteSpec.IsBootstrapPeer {
		var bootstrapper *ocr.BootstrapNode
		bootstrapper, err = ocr.NewBootstrapNode(ocr.BootstrapNodeArgs{
			BootstrapperFactory:   peerWrapper.Peer1,
			V1Bootstrappers:       v1BootstrapPeers,
			V2Bootstrappers:       v2Bootstrappers,
			ContractConfigTracker: tracker,
			Database:              ocrDB,
			LocalConfig:           lc,
			Logger:                ocrLogger,
		})
		if err != nil {
			return nil, errors.Wrap(err, "error calling NewBootstrapNode")
		}
		bootstrapperCtx := job.NewServiceAdapter(bootstrapper)
		services = append(services, bootstrapperCtx)
	} else {
		// In V1 or V1V2 mode, p2pv1BootstrapPeers must be defined either in
		//   node config or in job spec
		if peerWrapper.P2PConfig().NetworkStack() != ocrnetworking.NetworkingStackV2 {
			if len(v1BootstrapPeers) < 1 {
				return nil, errors.New("Need at least one v1 bootstrap peer defined")
			}
		}

		// In V1V2 or V2 mode, p2pv2Bootstrappers must be defined either in
		//   node config or in job spec
		if peerWrapper.P2PConfig().NetworkStack() != ocrnetworking.NetworkingStackV1 {
			if len(v2Bootstrappers) < 1 {
				return nil, errors.New("Need at least one v2 bootstrap peer defined")
			}
		}

		ocrkey, err := d.keyStore.OCR().Get(concreteSpec.EncryptedOCRKeyBundleID.String())
		if err != nil {
			return nil, err
		}
		contractABI, err := abi.JSON(strings.NewReader(offchainaggregator.OffchainAggregatorABI))
		if err != nil {
			return nil, errors.Wrap(err, "could not get contract ABI JSON")
		}

		cfg := chain.Config()
		strategy := txmgrcommon.NewQueueingTxStrategy(jb.ExternalJobID, cfg.OCR().DefaultTransactionQueueDepth(), cfg.Database().DefaultQueryTimeout())

		var checker txmgr.TransmitCheckerSpec
		if chain.Config().OCR().SimulateTransactions() {
			checker.CheckerType = txmgr.TransmitCheckerTypeSimulate
		}

		if concreteSpec.TransmitterAddress == nil {
			return nil, errors.New("TransmitterAddress is missing")
		}

		var jsGasLimit *uint32
		if jb.GasLimit.Valid {
			jsGasLimit = &jb.GasLimit.Uint32
		}
		gasLimit := pipeline.SelectGasLimit(chain.Config().EVM().GasEstimator(), jb.Type.String(), jsGasLimit)

		// effectiveTransmitterAddress is the transmitter address registered on the ocr contract. This is by default the EOA account on the node.
		// In the case of forwarding, the transmitter address is the forwarder contract deployed onchain between EOA and OCR contract.
		effectiveTransmitterAddress := concreteSpec.TransmitterAddress.Address()
		if jb.ForwardingAllowed {
			fwdrAddress, fwderr := chain.TxManager().GetForwarderForEOA(effectiveTransmitterAddress)
			if fwderr == nil {
				effectiveTransmitterAddress = fwdrAddress
			} else {
				lggr.Warnw("Skipping forwarding for job, will fallback to default behavior", "job", jb.Name, "err", fwderr)
			}
		}

		transmitter, err := ocrcommon.NewTransmitter(
			chain.TxManager(),
			[]common.Address{concreteSpec.TransmitterAddress.Address()},
			gasLimit,
			effectiveTransmitterAddress,
			strategy,
			checker,
			chain.ID(),
			d.keyStore.Eth(),
		)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create transmitter")
		}

		contractTransmitter := NewOCRContractTransmitter(
			concreteSpec.ContractAddress.Address(),
			contractCaller,
			contractABI,
			transmitter,
			chain.LogBroadcaster(),
			tracker,
			chain.ID(),
			effectiveTransmitterAddress,
		)

		runResults := make(chan *pipeline.Run, chain.Config().JobPipeline().ResultWriteQueueDepth())

		var configOverrider ocrtypes.ConfigOverrider
		configOverriderService, err := d.maybeCreateConfigOverrider(lggr, chain, concreteSpec.ContractAddress)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to create ConfigOverrider")
		}

		// NOTE: conditional assigning to `configOverrider` is necessary due to the unfortunate fact that assigning `nil` to an
		// interface variable causes `x == nil` checks to always return false, so methods on the interface cannot be safely called then.
		//
		// the problematic case would be:
		//    configOverriderService, err := d.maybeCreateConfigOverrider(...)
		//	  if err != nil { return ... }
		//	  configOverrider = configOverriderService // contract might be `nil`
		//    assert.False(configOverrider != nil) // even if 'contract' was nil, this check will return true, unexpectedly
		if configOverriderService != nil {
			services = append(services, configOverriderService)
			configOverrider = configOverriderService
		}

		jb.OCROracleSpec.CaptureEATelemetry = chain.Config().OCR().CaptureEATelemetry()
		enhancedTelemChan := make(chan ocrcommon.EnhancedTelemetryData, 100)
		if ocrcommon.ShouldCollectEnhancedTelemetry(&jb) {
			enhancedTelemService := ocrcommon.NewEnhancedTelemetryService(&jb, enhancedTelemChan, make(chan struct{}), d.monitoringEndpointGen.GenMonitoringEndpoint("EVM", chain.ID().String(), concreteSpec.ContractAddress.String(), synchronization.EnhancedEA), lggr.Named("EnhancedTelemetry"))
			services = append(services, enhancedTelemService)
		} else {
			lggr.Infow("Enhanced telemetry is disabled for job", "job", jb.Name)
		}

		oracle, err := ocr.NewOracle(ocr.OracleArgs{
			Database: ocrDB,
			Datasource: ocrcommon.NewDataSourceV1(
				d.pipelineRunner,
				jb,
				*jb.PipelineSpec,
				lggr,
				runResults,
				enhancedTelemChan,
			),
			LocalConfig:                  lc,
			ContractTransmitter:          contractTransmitter,
			ContractConfigTracker:        tracker,
			PrivateKeys:                  ocrkey,
			BinaryNetworkEndpointFactory: peerWrapper.Peer1,
			Logger:                       ocrLogger,
			V1Bootstrappers:              v1BootstrapPeers,
			V2Bootstrappers:              v2Bootstrappers,
			MonitoringEndpoint:           d.monitoringEndpointGen.GenMonitoringEndpoint("EVM", chain.ID().String(), concreteSpec.ContractAddress.String(), synchronization.OCR),
			ConfigOverrider:              configOverrider,
		})
		if err != nil {
			return nil, errors.Wrap(err, "error calling NewOracle")
		}
		oracleCtx := job.NewServiceAdapter(oracle)
		services = append(services, oracleCtx)

		// RunResultSaver needs to be started first so its available
		// to read db writes. It is stopped last after the Oracle is shut down
		// so no further runs are enqueued and we can drain the queue.
		services = append([]job.ServiceCtx{ocrcommon.NewResultRunSaver(
			runResults,
			d.pipelineRunner,
			make(chan struct{}),
			lggr,
			cfg.JobPipeline().MaxSuccessfulRuns(),
		)}, services...)
	}

	return services, nil
}

func (d *Delegate) maybeCreateConfigOverrider(logger logger.Logger, chain evm.Chain, contractAddress ethkey.EIP55Address) (*ConfigOverriderImpl, error) {
	flagsContractAddress := chain.Config().EVM().FlagsContractAddress()
	if flagsContractAddress != "" {
		flags, err := NewFlags(flagsContractAddress, chain.Client())
		if err != nil {
			return nil, errors.Wrapf(err,
				"OCR: unable to create Flags contract instance, check address: %s or remove EVM.FlagsContractAddress configuration variable",
				flagsContractAddress,
			)
		}

		ticker := utils.NewPausableTicker(ConfigOverriderPollInterval)
		return NewConfigOverriderImpl(logger, contractAddress, flags, &ticker)
	}
	return nil, nil
}
