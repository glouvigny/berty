package core

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	account "berty.tech/core/manager/account"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var (
	accountName = ""
	appConfig   *account.StateDB
)

func logger() *zap.Logger {
	return zap.L().Named("client.rn.gomobile")
}

func panicHandler() {
	if r := recover(); r != nil {
		panicErr := errors.New(fmt.Sprintf("panic from global export: %+v", r))
		logger().Error(panicErr.Error())
		if accountName == "" {
			return
		}
		a, err := account.Get(accountName)
		if err != nil {
			return
		}
		a.ErrChan() <- panicErr
	}
}

func getRandomPort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func Panic() {
	panic(nil)
}

func GetPort() (int, error) {

	defer panicHandler()

	a, err := account.Get(accountName)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.Split(a.GQLBind, ":")[1])
}

func Initialize(loggerNative Logger) error {

	defer panicHandler()

	if err := setupLogger("debug", loggerNative); err != nil {
		return err
	}

	initBleFunc()

	return nil
}

func ListAccounts(datastorePath string) (string, error) {

	defer panicHandler()

	accounts, err := account.List(datastorePath)
	if err != nil {
		return "", err
	}
	logger().Debug("ListAccounts", zap.Strings("acccounts", accounts))
	return strings.Join(accounts, ":"), nil
}

func initOrRestoreAppState(datastorePath string) error {
	initialJSONNetConf, err := json.Marshal(initialNetConf)
	if err != nil {
		return err
	}

	// Needed by OpenStateDB to init DB if no previous config is found (first launch)
	initialState := account.StateDB{
		JSONNetConf: string(initialJSONNetConf),
		BotMode:     initialBotMode,
		LocalGRPC:   initiallocalGRPC,
	}

	appState, err := account.OpenStateDB(datastorePath+"berty.state.db", initialState)
	if err != nil {
		return errors.Wrap(err, "state DB init failed")
	}
	appConfig = appState
	logger().Debug("App state:", zap.Int("StartCounter", appConfig.StartCounter))
	logger().Debug("App state:", zap.String("JSONNetConf", appConfig.JSONNetConf))
	logger().Debug("App state:", zap.Bool("BotMode", appConfig.BotMode))
	logger().Debug("App state:", zap.Bool("LocalGRPC", appConfig.LocalGRPC))

	return nil
}

func Start(nickname, datastorePath string, loggerNative Logger) error {

	defer panicHandler()

	accountName = nickname

	a, _ := account.Get(nickname)
	if a != nil {
		// daemon already started, no errors to return
		return nil
	}

	if err := initOrRestoreAppState(datastorePath); err != nil {
		return errors.Wrap(err, "app init/restore state failed")
	}

	run(nickname, datastorePath, loggerNative)
	waitDaemon(nickname)
	return nil
}

func Restart() error {

	defer panicHandler()

	currentAccount, _ := account.Get(accountName)
	if currentAccount != nil {
		currentAccount.ErrChan() <- nil
	}

	waitDaemon(accountName)
	return nil
}

func DropDatabase(datastorePath string) error {

	defer panicHandler()

	currentAccount, err := account.Get(accountName)
	if err != nil {
		return err
	}
	err = currentAccount.DropDatabase()
	if err != nil {
		return err
	}
	return Restart()
}

func run(nickname, datastorePath string, loggerNative Logger) {
	go func() {
		for {
			err := daemon(nickname, datastorePath, loggerNative)
			if err != nil {
				logger().Error("handle error, try to restart", zap.Error(err))
				time.Sleep(time.Second)
			} else {
				logger().Info("restarting daemon")
			}
		}
	}()
}

func waitDaemon(nickname string) {
	currentAccount, _ := account.Get(nickname)
	if currentAccount == nil || currentAccount.GQLBind == "" {
		logger().Debug("waiting for daemon to start")
		time.Sleep(time.Second)
		waitDaemon(nickname)
	}
}

func daemon(nickname, datastorePath string, loggerNative Logger) error {
	defer panicHandler()

	grpcPort, err := getRandomPort()
	if err != nil {
		return err
	}
	gqlPort, err := getRandomPort()
	if err != nil {
		return err
	}

	var a *account.Account

	netConf, err := createNetworkConfig()
	if err != nil {
		return err
	}

	accountOptions := account.Options{
		account.WithRing(ring),
		account.WithName(nickname),
		account.WithPassphrase("secure"),
		account.WithDatabase(&account.DatabaseOptions{
			Path: datastorePath,
			Drop: false,
		}),
		account.WithP2PNetwork(netConf),
		account.WithGrpcServer(&account.GrpcServerOptions{
			Bind:         fmt.Sprintf(":%d", grpcPort),
			Interceptors: false,
		}),
		account.WithGQL(&account.GQLOptions{
			Bind:         fmt.Sprintf(":%d", gqlPort),
			Interceptors: false,
		}),
	}

	if appConfig.BotMode {
		accountOptions = append(accountOptions, account.WithBot())
	}

	a, err = account.New(accountOptions...)
	if err != nil {
		return err
	}
	defer account.Delete(a)

	err = a.Open()
	if err != nil {
		return err
	}
	defer a.Close()

	if appConfig.LocalGRPC {
		err := StartLocalGRPC()
		if err != nil {
			logger().Error(err.Error())
			appConfig.LocalGRPC = false
		}
		// Continue if local gRPC fails (e.g wifi not connected)
		// Still re-enableable via toggle in devtools
	}
	logger().Debug("daemon started")
	return <-a.ErrChan()
}