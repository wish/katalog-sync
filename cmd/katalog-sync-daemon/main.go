package main

import (
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"

	consulApi "github.com/hashicorp/consul/api"
	flags "github.com/jessevdk/go-flags"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	grpc "google.golang.org/grpc"

	"github.com/wish/katalog-sync/pkg/daemon"
	katalogsync "github.com/wish/katalog-sync/proto"
)

// TODO: consul flags
var opts struct {
	LogLevel        string `long:"log-level" env:"LOG_LEVEL" description:"Log level" default:"info"`
	BindAddr        string `long:"bind-address" env:"BIND_ADDRESS" description:"address for binding RPC interface for sidecar"`
	MetricsBindAddr string `long:"metrics-bind-address" env:"METRICS_BIND_ADDRESS" description:"address for binding metrics interface"`
	PProfBindAddr   string `long:"pprof-bind-address" env:"PPROF_BIND_ADDRESS" description:"address for binding pprof"`
	daemon.DaemonConfig
	daemon.KubeletClientConfig
}

func main() {
	parser := flags.NewParser(&opts, flags.Default)
	if _, err := parser.Parse(); err != nil {
		// If the error was from the parser, then we can simply return
		// as Parse() prints the error already
		if _, ok := err.(*flags.Error); ok {
			os.Exit(1)
		}
		logrus.Fatalf("Error parsing flags: %v", err)
	}

	if opts.PProfBindAddr != "" {
		l, err := net.Listen("tcp", opts.PProfBindAddr)
		if err != nil {
			logrus.Fatalf("Error binding: %v", err)
		}

		go func() {
			http.Serve(l, http.DefaultServeMux)
		}()
	}

	if opts.MetricsBindAddr != "" {
		l, err := net.Listen("tcp", opts.MetricsBindAddr)
		if err != nil {
			logrus.Fatalf("Error binding: %v", err)
		}

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		go func() {
			http.Serve(l, mux)
		}()
	}

	// Use log level
	level, err := logrus.ParseLevel(opts.LogLevel)
	if err != nil {
		logrus.Fatalf("Unknown log level %s: %v", opts.LogLevel, err)
	}
	logrus.SetLevel(level)

	// Set the log format to have a reasonable timestamp
	formatter := &logrus.TextFormatter{
		FullTimestamp: true,
	}
	logrus.SetFormatter(formatter)

	kubeletClient, err := daemon.NewKubeletClient(opts.KubeletClientConfig)
	if err != nil {
		logrus.Fatalf("Unable to create kubelet client: %v", err)
	}

	// Consul testing
	consulCfg := consulApi.DefaultConfig()
	client, err := consulApi.NewClient(consulCfg)
	if err != nil {
		panic(err)
	}

	d := daemon.NewDaemon(opts.DaemonConfig, kubeletClient, client)

	if opts.BindAddr != "" {
		s := grpc.NewServer()
		katalogsync.RegisterKatalogSyncServer(s, d)
		l, err := net.Listen("tcp", opts.BindAddr)
		if err != nil {
			logrus.Fatalf("failed to listen: %v", err)
		}
		go func() {
			logrus.Errorf("error serving: %v", s.Serve(l))
		}()
	}

	// TODO: change to background, and wait on signals to die
	d.Run()
}
