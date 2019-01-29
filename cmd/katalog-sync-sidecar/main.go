package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	flags "github.com/jessevdk/go-flags"
	"github.com/sirupsen/logrus"
	grpc "google.golang.org/grpc"

	katalogsync "github.com/wish/katalog-sync/proto"
)

// TODO: consul flags
var opts struct {
	KatalogSyncEndpoint string `long:"katalog-sync-daemon" description:"katalog-sync-daemon API endpoint"`
	BindAddr            string `long:"bind-address" description:"address for binding checks to"`

	Namespace     string `long:"namespace"`
	PodName       string `long:"pod-name"`
	ContainerName string `long:"container-name"`
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

	var ready bool

	l, err := net.Listen("tcp", opts.BindAddr)
	if err != nil {
		logrus.Fatalf("Error binding: %v", err)
	}

	go func() {
		http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
			logrus.Infof("ready? %v", ready)
			if !ready {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		})
		// TODO: log error?
		http.Serve(l, http.DefaultServeMux)
	}()

	conn, err := grpc.Dial(opts.KatalogSyncEndpoint, grpc.WithInsecure())
	if err != nil {
		logrus.Fatalf("Unable to connect to katalog-sync-daemon: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // TODO: do we even need this?
	sigs := make(chan os.Signal)
	defer close(sigs)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	client := katalogsync.NewKatalogSyncClient(conn)

	logrus.Infof("Starting register")
	// Connect to sidecar and send register request
	if _, err := client.Register(ctx, &katalogsync.RegisterQuery{Namespace: opts.Namespace, PodName: opts.PodName, ContainerName: opts.ContainerName}); err != nil {
		panic(err)
	}
	ready = true
	logrus.Infof("register complete, waiting for signals")

	// TODO: add option that will do the TTL updates on our own?

	// Wait for kill signal
WAITLOOP:
	for {
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				logrus.Infof("Got signal to stop, starting deregister")
				break WAITLOOP
			}
		}
	}

	// Send deregister request
	for {
		logrus.Infof("deregister attempt")
		_, err := client.Deregister(ctx, &katalogsync.DeregisterQuery{Namespace: opts.Namespace, PodName: opts.PodName, ContainerName: opts.ContainerName})
		if err == nil {
			return
		}
	}
}
