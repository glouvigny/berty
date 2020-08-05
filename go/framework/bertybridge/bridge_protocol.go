package bertybridge

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	badger_opts "github.com/dgraph-io/badger/options"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	"github.com/ipfs/go-datastore"
	ds_sync "github.com/ipfs/go-datastore/sync"
	ipfs_badger "github.com/ipfs/go-ds-badger"
	"github.com/ipfs/go-ipfs/core"
	ipfs_repo "github.com/ipfs/go-ipfs/repo"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/routing"
	discovery "github.com/libp2p/go-libp2p-discovery"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/pkg/errors"
	grpc_trace "go.opentelemetry.io/otel/instrumentation/grpctrace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"berty.tech/berty/v2/go/internal/config"
	"berty.tech/berty/v2/go/internal/ipfsutil"
	mc "berty.tech/berty/v2/go/internal/multipeer-connectivity-transport"
	"berty.tech/berty/v2/go/internal/tinder"
	"berty.tech/berty/v2/go/internal/tracer"
	"berty.tech/berty/v2/go/pkg/bertyprotocol"
	"berty.tech/berty/v2/go/pkg/errcode"
)

var (
	defaultProtocolRendezVousPeer = config.BertyMobile.RendezVousPeer
	defaultProtocolBootstrap      = config.BertyMobile.Bootstrap
	defaultTracingHost            = config.BertyMobile.Tracing
	defaultSwarmAddrs             = config.BertyMobile.DefaultSwarmAddrs
	defaultAPIAddrs               = config.BertyMobile.DefaultAPIAddrs
	APIConfig                     = config.BertyMobile.APIConfig
)

type Protocol struct {
	*Bridge

	node    *core.IpfsNode
	service bertyprotocol.Service

	// protocol datastore
	ds             datastore.Batching
	protocolClient bertyprotocol.Client
}

type ProtocolConfig struct {
	*Config

	dLogger  NativeLoggerDriver
	loglevel string
	poiDebug bool

	swarmListeners []string
	rootDirectory  string
	tracing        bool
	tracingPrefix  string
	localDiscovery bool

	// internal
	coreAPI ipfsutil.ExtendedCoreAPI
}

func NewProtocolConfig() *ProtocolConfig {
	return &ProtocolConfig{
		Config: NewConfig(),
	}
}

func (pc *ProtocolConfig) RootDirectory(dir string) {
	pc.rootDirectory = dir
}

func (pc *ProtocolConfig) EnableTracing() {
	pc.tracing = true
}

func (pc *ProtocolConfig) EnablePOIDebug() {
	pc.poiDebug = true
}

func (pc *ProtocolConfig) SetTracingPrefix(prefix string) {
	pc.tracingPrefix = prefix
}

func (pc *ProtocolConfig) LogLevel(level string) {
	pc.loglevel = level
}

func (pc *ProtocolConfig) LoggerDriver(dLogger NativeLoggerDriver) {
	pc.dLogger = dLogger
}

func (pc *ProtocolConfig) AddSwarmListener(laddr string) {
	pc.swarmListeners = append(pc.swarmListeners, laddr)
}

func (pc *ProtocolConfig) DisableLocalDiscovery() {
	pc.localDiscovery = false
}

func NewProtocolBridge(ctx context.Context, config *ProtocolConfig) (*Protocol, error) {
	// setup logger
	var logger *zap.Logger
	{
		var err error

		if config.dLogger != nil {
			logger, err = newNativeLogger(config.loglevel, config.dLogger)
		} else {
			logger, err = newLogger(config.loglevel)
		}

		if err != nil {
			return nil, err
		}
	}

	return newProtocolBridge(ctx, logger, config)
}

func newProtocolBridge(ctx context.Context, logger *zap.Logger, config *ProtocolConfig) (*Protocol, error) {
	// setup coreapi if needed
	var (
		api  ipfsutil.ExtendedCoreAPI
		node *core.IpfsNode
		ps   *pubsub.PubSub
		repo ipfs_repo.Repo
		disc tinder.Driver
	)

	{
		var err error

		if api = config.coreAPI; api == nil {
			// load repo

			if repo, err = getIPFSRepo(config.rootDirectory); err != nil {
				return nil, errors.Wrap(err, "failed to get ipfs repo")
			}

			var rdvpeer *peer.AddrInfo

			if rdvpeer, err = ipfsutil.ParseAndResolveIpfsAddr(ctx, defaultProtocolRendezVousPeer); err != nil {
				return nil, errors.New("failed to parse rdvp multiaddr: " + defaultProtocolRendezVousPeer)
			}

			var bopts = ipfsutil.CoreAPIConfig{
				DisableCorePubSub: true,
				SwarmAddrs:        defaultSwarmAddrs,
				APIAddrs:          defaultAPIAddrs,
				APIConfig:         APIConfig,
				ExtraLibp2pOption: libp2p.ChainOptions(libp2p.Transport(mc.NewTransportConstructorWithLogger(logger))),
				HostConfig: func(h host.Host, _ routing.Routing) error {
					var err error

					h.Peerstore().AddAddrs(rdvpeer.ID, rdvpeer.Addrs, peerstore.PermanentAddrTTL)
					// @FIXME(gfanton): use rand as argument
					rdvClient := tinder.NewRendezvousDiscovery(logger, h, rdvpeer.ID,
						rand.New(rand.NewSource(rand.Int63())))

					minBackoff, maxBackoff := time.Second, time.Minute
					rng := rand.New(rand.NewSource(rand.Int63()))
					disc, err = tinder.NewService(
						logger,
						rdvClient,
						discovery.NewExponentialBackoff(minBackoff, maxBackoff, discovery.FullJitter, time.Second, 5.0, 0, rng),
					)
					if err != nil {
						return err
					}

					ps, err = pubsub.NewGossipSub(ctx, h,
						pubsub.WithMessageSigning(true),
						pubsub.WithFloodPublish(true),
						pubsub.WithDiscovery(disc),
					)

					if err != nil {
						return err
					}

					return nil
				},
			}

			bopts.BootstrapAddrs = defaultProtocolBootstrap

			// should be a valid rendezvous peer
			bopts.BootstrapAddrs = append(bopts.BootstrapAddrs, defaultProtocolRendezVousPeer)
			if len(config.swarmListeners) > 0 {
				bopts.SwarmAddrs = append(bopts.SwarmAddrs, config.swarmListeners...)
			}

			api, node, err = ipfsutil.NewCoreAPIFromRepo(ctx, repo, &bopts)
			if err != nil {
				return nil, errcode.TODO.Wrap(err)
			}

			psapi := ipfsutil.NewPubSubAPI(ctx, logger, disc, ps)
			api = ipfsutil.InjectPubSubCoreAPIExtendedAdaptater(api, psapi)

			// construct http api endpoint
			ipfsutil.ServeHTTPApi(logger, node, config.rootDirectory+"/ipfs")

			// serve the embedded ipfs webui
			ipfsutil.ServeHTTPWebui(logger)

			if config.poiDebug {
				ipfsutil.EnableConnLogger(ctx, logger, node.PeerHost)
			}
		}
	}

	// init tracing
	if config.tracing {
		shortID := fmt.Sprintf("%.6s", node.Identity.String())
		svcName := fmt.Sprintf("<%s>", shortID)
		if prefix := strings.TrimSpace(config.tracingPrefix); prefix != "" {
			svcName = fmt.Sprintf("<%s@%s>", prefix, shortID)
		}
		tracer.InitTracer(defaultTracingHost, svcName)
	}

	// load datastore
	var rootds datastore.Batching
	{
		var err error

		if rootds, err = getRootDatastore(config.rootDirectory); err != nil {
			return nil, errcode.TODO.Wrap(err)
		}
	}

	// setup protocol
	var service bertyprotocol.Service
	{
		odbDir, err := getOrbitDBDirectory(config.rootDirectory)
		if err != nil {
			return nil, errcode.TODO.Wrap(err)
		}

		// initialize new protocol client
		protocolOpts := bertyprotocol.Opts{
			PubSub:         ps,
			Logger:         logger.Named("bertyprotocol"),
			OrbitDirectory: odbDir,
			RootDatastore:  rootds,
			IpfsCoreAPI:    api,
			TinderDriver:   disc,
		}

		if node != nil {
			protocolOpts.Host = node.PeerHost
		}

		service, err = bertyprotocol.New(ctx, protocolOpts)
		if err != nil {
			return nil, errcode.TODO.Wrap(err)
		}
	}

	// register protocol service
	var grpcServer *grpc.Server
	{
		grpcLogger := logger.Named("grpc")
		// Define customfunc to handle panic
		panicHandler := func(p interface{}) (err error) {
			return status.Errorf(codes.Unknown, "panic recover: %v", p)
		}

		// Shared options for the logger, with a custom gRPC code to log level function.
		recoverOpts := []grpc_recovery.Option{
			grpc_recovery.WithRecoveryHandler(panicHandler),
		}

		// setup grpc with zap
		// grpc_zap.ReplaceGrpcLoggerV2(grpcLogger)

		trServer := tracer.New("grpc-server")
		serverOpts := []grpc.ServerOption{
			grpc_middleware.WithUnaryServerChain(
				grpc_ctxtags.UnaryServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),

				grpc_zap.UnaryServerInterceptor(grpcLogger),
				grpc_recovery.UnaryServerInterceptor(recoverOpts...),
				grpc_trace.UnaryServerInterceptor(trServer),
			),
			grpc_middleware.WithStreamServerChain(
				grpc_ctxtags.StreamServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
				grpc_zap.StreamServerInterceptor(grpcLogger),
				grpc_recovery.StreamServerInterceptor(recoverOpts...),
				grpc_trace.StreamServerInterceptor(trServer),
			),
		}

		grpcServer = grpc.NewServer(serverOpts...)
		bertyprotocol.RegisterProtocolServiceServer(grpcServer, service)
	}

	// register messenger service
	var protocolClient bertyprotocol.Client
	{
		var err error
		protocolClient, err = bertyprotocol.NewClient(ctx, service)

		if err != nil {
			return nil, errcode.TODO.Wrap(err)
		}
		// opts := bertymessenger.Opts{
		// 	Logger:          logger.Named("messenger"),
		// 	ProtocolService: service,
		// }
		// messenger := bertymessenger.New(protocolClient, &opts)
		// bertymessenger.RegisterMessengerServiceServer(grpcServer, messenger)
	}

	// setup bridge
	var bridge *Bridge
	{
		var err error

		bridge, err = newBridge(ctx, grpcServer, logger, config.Config)
		if err != nil {
			return nil, err
		}
	}

	return &Protocol{
		Bridge: bridge,

		service: service,
		node:    node,

		protocolClient: protocolClient,

		ds: rootds,
	}, nil
}

func (p *Protocol) Close() (err error) {
	// Close bridge
	p.Bridge.Close()

	p.protocolClient.Close()

	// close service
	err = p.service.Close() // keep service error

	if p.node != nil {
		p.node.Close()
	}

	if p.ds != nil {
		p.ds.Close()
	}

	return
}

func getRootDatastore(path string) (datastore.Batching, error) {
	if path == "" || path == ":memory:" {
		baseds := ds_sync.MutexWrap(datastore.NewMapDatastore())
		return baseds, nil
	}

	basepath := filepath.Join(path, "store")
	_, err := os.Stat(basepath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.Wrap(err, "unable get directory")
		}
		if err := os.MkdirAll(basepath, 0700); err != nil {
			return nil, errors.Wrap(err, "unable to create datastore directory")
		}
	}

	baseds, err := ipfs_badger.NewDatastore(basepath, &ipfs_badger.Options{
		Options: ipfs_badger.DefaultOptions.WithValueLogLoadingMode(badger_opts.FileIO),
	})

	if err != nil {
		return nil, errors.Wrapf(err, "failed to load datastore on: `%s`", basepath)
	}

	return baseds, nil
}

func getOrbitDBDirectory(path string) (string, error) {
	if path == "" || path == ":memory:" {
		return path, nil
	}

	basePath := filepath.Join(path, "orbitdb")
	_, err := os.Stat(basePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", errors.Wrap(err, "unable get orbitdb directory")
		}
		if err := os.MkdirAll(basePath, 0700); err != nil {
			return "", errors.Wrap(err, "unable to create orbitdb directory")
		}
	}

	return basePath, nil
}

func getIPFSRepo(path string) (ipfs_repo.Repo, error) {
	if path == "" || path == ":memory:" {
		repods := ds_sync.MutexWrap(datastore.NewMapDatastore())
		return ipfsutil.CreateMockedRepo(repods)
	}

	basepath := filepath.Join(path, "ipfs")
	_, err := os.Stat(basepath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.Wrap(err, "unable get orbitdb directory")
		}
		if err := os.MkdirAll(basepath, 0700); err != nil {
			return nil, errors.Wrap(err, "unable to create orbitdb directory")
		}
	}

	return ipfsutil.LoadRepoFromPath(basepath)
}
