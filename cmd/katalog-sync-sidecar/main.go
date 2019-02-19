package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/sirupsen/logrus"
	grpc "google.golang.org/grpc"

	katalogsync "github.com/wish/katalog-sync/proto"
)

var opts struct {
	LogLevel              string        `long:"log-level" env:"LOG_LEVEL" description:"Log level" default:"info"`
	KatalogSyncEndpoint   string        `long:"katalog-sync-daemon" env:"KATALOG_SYNC_DAEMON" description:"katalog-sync-daemon API endpoint"`
	KatalogSyncMaxBackoff time.Duration `long:"katalog-sync-daemon-max-backoff" env:"KATALOG_SYNC_DAEMON_MAX_BACKOFF" description:"katalog-sync-daemon API max backoff" default:"1s"`
	BindAddr              string        `long:"bind-address" env:"BIND_ADDRESS" description:"address for binding checks to"`

	Namespace     string `long:"namespace" env:"NAMESPACE" description:"k8s namespace this is running in"`
	PodName       string `long:"pod-name" env:"POD_NAME" description:"k8s pod this is running in"`
	ContainerName string `long:"container-name" env:"CONTAINER_NAME" description:"k8s container this is running in"`
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

	conn, err := grpc.Dial(opts.KatalogSyncEndpoint, grpc.WithInsecure(), grpc.WithBackoffMaxDelay(opts.KatalogSyncMaxBackoff))
	if err != nil {
		logrus.Fatalf("Unable to connect to katalog-sync-daemon: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // TODO: do we even need this?
	sigs := make(chan os.Signal, 1)
	defer close(sigs)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	client := katalogsync.NewKatalogSyncClient(conn)

	// Connect to sidecar and send register request
	// We want to retry until we are successful
	for {
		// If we get a signal to stop; lets gracefully exit
		select {
		case sig := <-sigs:
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				logrus.Infof("Got signal to stop while registering, exiting")
				return
			}
		default:
		}

		if _, err := client.Register(ctx, &katalogsync.RegisterQuery{Namespace: opts.Namespace, PodName: opts.PodName, ContainerName: opts.ContainerName}); err != nil {
			logrus.Errorf("error registering with katalog-sync-daemon: %v %v", grpc.Code(err), err)
		} else {
			break
		}

		// TODO: better sleep + backoff based on GRPC error codes
		time.Sleep(time.Second)
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

	go func() {
		<-sigs
		cancel()
	}()

	// Send deregister request
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		logrus.Infof("deregister attempt")
		_, err := client.Deregister(ctx, &katalogsync.DeregisterQuery{Namespace: opts.Namespace, PodName: opts.PodName, ContainerName: opts.ContainerName})
		if err == nil {
			return
		}

		// TODO: better sleep + backoff based on GRPC error codes
		time.Sleep(time.Second)
	}
}
