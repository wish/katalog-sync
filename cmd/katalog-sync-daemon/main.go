package main

import (
	"net"
	"os"

	consulApi "github.com/hashicorp/consul/api"
	flags "github.com/jessevdk/go-flags"
	"github.com/sirupsen/logrus"
	grpc "google.golang.org/grpc"

	"github.com/wish/katalog-sync/pkg/daemon"
	katalogsync "github.com/wish/katalog-sync/proto"
)

// TODO: consul flags
var opts struct {
	LogLevel string `long:"log-level" description:"Log level" default:"info"`
	BindAddr string `long:"bind-address" description:"address for binding RPC interface for sidecar"`
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
