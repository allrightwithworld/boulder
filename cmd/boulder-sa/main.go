package main

import (
	"flag"
	"os"

	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/features"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	"github.com/letsencrypt/boulder/sa"
	sapb "github.com/letsencrypt/boulder/sa/proto"
	"github.com/prometheus/client_golang/prometheus"
)

type config struct {
	SA struct {
		cmd.ServiceConfig
		cmd.DBConfig

		Features map[string]bool

		// Max simultaneous SQL queries caused by a single RPC.
		ParallelismPerRPC int
	}

	Syslog cmd.SyslogConfig
}

func main() {
	grpcAddr := flag.String("grpc-addr", "", "gRPC listen address override")
	debugAddr := flag.String("debug-addr", "", "Debug server address override")
	configFile := flag.String("config", "", "File path to the configuration file for this service")
	flag.Parse()
	if *configFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	var c config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")

	err = features.Set(c.SA.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	if *grpcAddr != "" {
		c.SA.GRPC.Address = *grpcAddr
	}
	if *debugAddr != "" {
		c.SA.DebugAddr = *debugAddr
	}

	scope, logger := cmd.StatsAndLogging(c.Syslog, c.SA.DebugAddr)
	defer logger.AuditPanic()
	logger.Info(cmd.VersionString())

	saConf := c.SA

	dbURL, err := saConf.DBConfig.URL()
	cmd.FailOnError(err, "Couldn't load DB URL")

	dbMap, err := sa.NewDbMap(dbURL, saConf.DBConfig.MaxDBConns)
	cmd.FailOnError(err, "Couldn't connect to SA database")

	// Export the MaxDBConns
	dbConnStat := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "max_db_connections",
		Help: "Maximum number of DB connections allowed.",
	})
	scope.MustRegister(dbConnStat)
	dbConnStat.Set(float64(saConf.DBConfig.MaxDBConns))

	if saConf.DBConfig.MaxIdleDBConns != 0 {
		dbMap.Db.SetMaxIdleConns(saConf.DBConfig.MaxIdleDBConns)
	}
	go sa.ReportDbConnCount(dbMap, scope)

	parallel := saConf.ParallelismPerRPC
	if parallel < 1 {
		parallel = 1
	}
	sai, err := sa.NewSQLStorageAuthority(dbMap, cmd.Clock(), logger, scope, parallel)
	cmd.FailOnError(err, "Failed to create SA impl")

	tls, err := c.SA.TLS.Load()
	cmd.FailOnError(err, "TLS config")
	serverMetrics := bgrpc.NewServerMetrics(scope)
	grpcSrv, listener, err := bgrpc.NewServer(c.SA.GRPC, tls, serverMetrics)
	cmd.FailOnError(err, "Unable to setup SA gRPC server")
	gw := bgrpc.NewStorageAuthorityServer(sai)
	sapb.RegisterStorageAuthorityServer(grpcSrv, gw)

	go cmd.CatchSignals(logger, grpcSrv.GracefulStop)

	err = cmd.FilterShutdownErrors(grpcSrv.Serve(listener))
	cmd.FailOnError(err, "SA gRPC service failed")
}
