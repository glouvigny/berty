package bertybridge

import (
	"context"
	"os"
	"path/filepath"

	"berty.tech/berty/v2/go/internal/config"
	"berty.tech/berty/v2/go/internal/ipfsutil"
	"berty.tech/berty/v2/go/pkg/bertychat"
	"berty.tech/berty/v2/go/pkg/bertyprotocol"
	"berty.tech/berty/v2/go/pkg/errcode"
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
	"github.com/libp2p/go-libp2p-core/peer"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var defaultProtocolRendezVousPeer = config.BertyMobile.RendezVousPeer
var defaultProtocolBootstrap = config.BertyMobile.Bootstrap

type Protocol struct {
	*Bridge

	node    *core.IpfsNode
	dht     *dht.IpfsDHT
	service bertyprotocol.Service

	// protocol datastore
	ds datastore.Batching

	// ipfs repo
	repo ipfs_repo.Repo
}

type ProtocolConfig struct {
	*Config

	dLogger  NativeLoggerDriver
	loglevel string

	swarmListeners []string
	rootDirectory  string

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

func (pc *ProtocolConfig) LogLevel(level string) {
	pc.loglevel = level
}

func (pc *ProtocolConfig) LoggerDriver(dLogger NativeLoggerDriver) {
	pc.dLogger = dLogger
}

func (pc *ProtocolConfig) AddSwarmListener(laddr string) {
	pc.swarmListeners = append(pc.swarmListeners, laddr)
}

func NewProtocolBridge(config *ProtocolConfig) (*Protocol, error) {
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

	return newProtocolBridge(logger, config)
}

func newProtocolBridge(logger *zap.Logger, config *ProtocolConfig) (*Protocol, error) {
	ctx := context.Background()

	// setup coreapi if needed
	var api ipfsutil.ExtendedCoreAPI
	var node *core.IpfsNode
	var dht *dht.IpfsDHT
	var repo ipfs_repo.Repo
	{
		var err error

		if api = config.coreAPI; api == nil {
			// load repo

			if repo, err = getIPFSRepo(config.rootDirectory); err != nil {
				return nil, errors.Wrap(err, "failed to get ipfs repo")
			}

			var bopts = ipfsutil.CoreAPIConfig{}
			bopts.BootstrapAddrs = defaultProtocolBootstrap

			var rdvpeer *peer.AddrInfo
			var crouting <-chan *ipfsutil.RoutingOut

			if rdvpeer, err = ipfsutil.ParseAndResolveIpfsAddr(ctx, defaultProtocolRendezVousPeer); err != nil {
				return nil, errors.New("failed to parse rdvp multiaddr: " + defaultProtocolRendezVousPeer)
			} else { // should be a valid rendezvous peer
				bopts.BootstrapAddrs = append(bopts.BootstrapAddrs, defaultProtocolRendezVousPeer)
				bopts.Routing, crouting = ipfsutil.NewTinderRouting(logger, rdvpeer, false)
			}

			if len(config.swarmListeners) > 0 {
				bopts.SwarmAddrs = config.swarmListeners
			}

			api, node, err = ipfsutil.NewCoreAPIFromRepo(ctx, repo, &bopts)
			if err != nil {
				return nil, errcode.TODO.Wrap(err)
			}

			out := <-crouting
			dht = out.IpfsDHT
		}
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
		odb_dir, err := getOrbitDBDirectory(config.rootDirectory)
		if err != nil {
			return nil, errcode.TODO.Wrap(err)
		}

		// initialize new protocol client
		protocolOpts := bertyprotocol.Opts{
			Logger:         logger.Named("bertyprotocol"),
			OrbitDirectory: odb_dir,
			RootDatastore:  rootds,
			IpfsCoreAPI:    api,
		}

		service, err = bertyprotocol.New(protocolOpts)
		if err != nil {
			return nil, errcode.TODO.Wrap(err)
		}
	}

	// register protocol service
	var grpcServer *grpc.Server
	{
		grpcLogger := logger.Named("grpc.protocol")
		// Define customfunc to handle panic
		panicHandler := func(p interface{}) (err error) {
			return status.Errorf(codes.Unknown, "panic recover: %v", p)
		}

		// Shared options for the logger, with a custom gRPC code to log level function.
		recoverOpts := []grpc_recovery.Option{
			grpc_recovery.WithRecoveryHandler(panicHandler),
		}

		zapOpts := []grpc_zap.Option{
			grpc_zap.WithLevels(grpcCodeToLevel),
		}

		// setup grpc with zap
		grpc_zap.ReplaceGrpcLoggerV2(grpcLogger)
		grpcServer = grpc.NewServer(
			grpc_middleware.WithUnaryServerChain(
				grpc_ctxtags.UnaryServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),

				grpc_zap.UnaryServerInterceptor(grpcLogger, zapOpts...),
				grpc_recovery.UnaryServerInterceptor(recoverOpts...),
			),
			grpc_middleware.WithStreamServerChain(
				grpc_ctxtags.StreamServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
				grpc_zap.StreamServerInterceptor(grpcLogger, zapOpts...),
				grpc_recovery.StreamServerInterceptor(recoverOpts...),
			),
		)

		bertyprotocol.RegisterProtocolServiceServer(grpcServer, service)
	}

	// register chat service
	{
		protocolClient, err := bertyprotocol.NewClient(service)
		if err != nil {
			return nil, errcode.TODO.Wrap(err)
		}
		chat := bertychat.New(protocolClient, &bertychat.Opts{Logger: logger.Named("chat")})
		bertychat.RegisterChatServiceServer(grpcServer, chat)
	}

	// setup bridge
	var bridge *Bridge
	{
		var err error

		bridge, err = newBridge(grpcServer, logger, config.Config)
		if err != nil {
			return nil, err
		}
	}

	return &Protocol{
		Bridge: bridge,

		service: service,
		node:    node,
		dht:     dht,

		ds: rootds,
	}, nil
}

func (p *Protocol) newServiceClient() (bertyprotocol.ProtocolServiceClient, error) {
	cl, err := p.Bridge.NewGRPCClient()
	if err != nil {
		return nil, err
	}

	return bertyprotocol.NewProtocolServiceClient(cl.grpcClient), nil
}

func (p *Protocol) Close() (err error) {
	// Close bridge
	p.Bridge.Close()

	// close service
	err = p.service.Close() // keep service error

	/// close other services
	if p.dht != nil {
		p.dht.Close()
	}

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
