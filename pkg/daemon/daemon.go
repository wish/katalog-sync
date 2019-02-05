package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	consulApi "github.com/hashicorp/consul/api"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	katalogsync "github.com/wish/katalog-sync/proto"
)

var (
	ConsulSyncSourceName  = "external-sync-source"
	ConsulSyncSourceValue = "katalog-sync"
	ConsulK8sLinkName     = "external-k8s-link"
)

type DaemonConfig struct {
	MinSyncInterval     time.Duration `long:"min-sync-interval" description:"minimum duration allowed for sync" default:"500ms"`
	MaxSyncInterval     time.Duration `long:"max-sync-interval" description:"maximum duration allowed for sync" default:"5s"`
	DefaultSyncInterval time.Duration `long:"default-sync-interval" default:"1s"`
	DefaultCheckTTL     time.Duration `long:"default-check-ttl" default:"10s"`
	SyncTTLBuffer       time.Duration `long:"sync-ttl-buffer-duration" description:"how much time to ensure is between sync time and ttl" default:"10s"`
}

func NewDaemon(c DaemonConfig, k8sClient Kubelet, consulClient *consulApi.Client) *Daemon {
	return &Daemon{
		c:            c,
		k8sClient:    k8sClient,
		consulClient: consulClient,

		localK8sState: make(map[string]*Pod),
		syncCh:        make(chan chan error),
	}
}

// Daemon is responsible for syncing state from k8s -> consul
type Daemon struct {
	c DaemonConfig

	k8sClient    Kubelet
	consulClient *consulApi.Client

	// TODO: locks around this? or move everything through a channel
	// Our local representation of what pods are running
	localK8sState map[string]*Pod

	syncCh chan chan error
}

func (d *Daemon) doSync(ctx context.Context) error {
	ch := make(chan error, 1)

	// Trigger a sync
	d.syncCh <- ch
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-ch:
		return err
	}
}

func (d *Daemon) Register(ctx context.Context, in *katalogsync.RegisterQuery) (*katalogsync.RegisterResult, error) {
	if err := d.doSync(ctx); err != nil {
		return nil, err
	}

	k := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", in.Namespace, in.PodName)
	pod, ok := d.localK8sState[k]
	if !ok {
		return nil, fmt.Errorf("Unable to find pod: %s", k)
	}

	pod.SidecarState.SidecarName = in.ContainerName
	pod.SidecarState.Ready = true

	if err := d.doSync(ctx); err != nil {
		return nil, err
	}

	if err := pod.SyncStatuses.GetError(); err != nil {
		return nil, errors.Wrap(err, "Unable to sync status")
	}

	// TODO: optional through a flag in RegisterQuery
	// The goal here is to ensure that the registration has propogated to the rest of the cluster
	nodeName, err := d.consulClient.Agent().NodeName()
	if err != nil {
		return nil, err
	}
	opts := &consulApi.QueryOptions{AllowStale: true, UseCache: true}
	if err := d.ConsulNodeDoUntil(ctx, nodeName, opts, func(node *consulApi.CatalogNode) bool {
		synced := true
		for _, serviceName := range pod.GetServiceNames() {
			// If the service exists, then we just need to update
			if _, ok := node.Services[pod.GetServiceID(serviceName)]; !ok {
				synced = false
			}
		}
		return synced
	}); err != nil {
		return nil, err
	}

	if ready, _ := pod.Ready(); ready {
		return nil, nil
	} else {
		return nil, fmt.Errorf("not ready!: %v", pod.SyncStatuses.GetError())
	}
}

func (d *Daemon) Deregister(ctx context.Context, in *katalogsync.DeregisterQuery) (*katalogsync.DeregisterResult, error) {
	if err := d.doSync(ctx); err != nil {
		return nil, err
	}

	k := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", in.Namespace, in.PodName)
	pod, ok := d.localK8sState[k]
	if !ok {
		return nil, fmt.Errorf("Unable to find pod: %s", k)
	}

	pod.SidecarState.Ready = false

	if err := d.doSync(ctx); err != nil {
		return nil, err
	}

	if err := pod.SyncStatuses.GetError(); err != nil {
		return nil, errors.Wrap(err, "Unable to sync status")
	}

	// TODO: optional through a flag in DeregisterQuery
	// The goal here is to ensure that the deregistration has propogated to the rest of the cluster
	nodeName, err := d.consulClient.Agent().NodeName()
	if err != nil {
		return nil, err
	}
	opts := &consulApi.QueryOptions{AllowStale: true, UseCache: true}
	if err := d.ConsulNodeDoUntil(ctx, nodeName, opts, func(node *consulApi.CatalogNode) bool {
		synced := true
		for _, serviceName := range pod.GetServiceNames() {
			// If the service exists, then we just need to update
			if _, ok := node.Services[pod.GetServiceID(serviceName)]; ok {
				synced = false
			}
		}
		return synced
	}); err != nil {
		return nil, err
	}

	if ready, _ := pod.Ready(); !ready {
		return nil, nil
	} else {
		return nil, fmt.Errorf("ready!: %v", pod.SyncStatuses.GetError())
	}
}

func (d *Daemon) calculateSleepTime() time.Duration {
	sleepDuration := d.c.MaxSyncInterval
	for _, pod := range d.localK8sState {
		if pod.SyncInterval < sleepDuration && pod.SyncInterval > d.c.MinSyncInterval {
			sleepDuration = pod.SyncInterval
		}
	}
	return sleepDuration
}

// TODO: refactor into a start/stop/run job (so initial sync is done on start, and the rest in background goroutine)
func (d *Daemon) Run() error {
	timer := time.NewTimer(0)
	var lastRun time.Time

	retChans := make([]chan error, 0)

	doSync := func() error {
		defer func() {
			sleepTime := d.calculateSleepTime()
			logrus.Infof("sleeping for %s", sleepTime)
			timer = time.NewTimer(sleepTime)
			lastRun = time.Now()
		}()
		// Load initial state from k8s
		if err := d.fetchK8s(); err != nil {
			return err
		}

		// Do initial sync
		if err := d.syncConsul(); err != nil {
			return err
		}

		return nil
	}

	// Loop forever running the update job
	for {
		select {
		// If the timer went off, then we need to do a sync
		case <-timer.C:
			start := time.Now()
			err := doSync()
			logrus.Infof("Sync completed in %s: %v", time.Now().Sub(start), err)
			for _, ch := range retChans {
				select {
				case ch <- err:
				default:
				}
			}
			retChans = retChans[:]

		// If we got a channel on the syncCh then we need to add it to our list
		case ch := <-d.syncCh:
			retChans = append(retChans, ch)
			if time.Now().Sub(lastRun) > d.c.MinSyncInterval {
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(0)
			}
		}
	}

	return nil
}

// fetchK8s is responsible for updating the local k8sState with what we pull
// from our k8sClient
func (d *Daemon) fetchK8s() error {
	podList, err := d.k8sClient.GetPodList()
	if err != nil {
		return err
	}

	// Add/Update the ones we have
	newKeys := make(map[string]struct{})
	for _, pod := range podList.Items {
		// If the pod doesn't have a service-name defined, we don't touch it
		if _, ok := pod.ObjectMeta.Annotations[ConsulServiceNames]; !ok {
			continue
		}

		// If the pod isn't in the "Running" phase, we skip
		if pod.Status.Phase != "Running" {
			continue
		}

		key := pod.ObjectMeta.SelfLink
		newKeys[key] = struct{}{}
		if existingPod, ok := d.localK8sState[key]; ok {
			existingPod.UpdatePod(pod)
		} else {
			p, err := NewPod(pod, &d.c)
			if err != nil {
				logrus.Errorf("error creating local state for pod: %v", err)
			} else {
				d.localK8sState[key] = p
			}
		}
	}

	// remove any local ones that don't exist anymore
	for k := range d.localK8sState {
		if _, ok := newKeys[k]; !ok {
			delete(d.localK8sState, k)
		}
	}

	return nil
}

// syncConsul is responsible for syncing local state to consul
func (d *Daemon) syncConsul() error {
	// Get services from consul
	consulServices, err := d.consulClient.Agent().Services()
	if err != nil {
		return err
	}

	// TODO: split out update, for now we'll just re-register it all
	// Push/Update from local state
	for _, pod := range d.localK8sState {
		ready, containerReadiness := pod.Ready()

		status := "critical"
		if ready {
			status = "passing"
		}

		notesB, err := json.MarshalIndent(containerReadiness, "", "  ")
		if err != nil {
			panic(err)
		}

		for _, serviceName := range pod.GetServiceNames() {
			// If the service exists, then we just need to update
			if consulService, ok := consulServices[pod.GetServiceID(serviceName)]; ok && !pod.HasChange(consulService) {
				// only call update if we are past halflife of last update
				if pod.SyncStatuses.GetStatus(serviceName).LastUpdated.IsZero() || time.Now().Sub(pod.SyncStatuses.GetStatus(serviceName).LastUpdated) >= (pod.CheckTTL/2) {
					// If the service already exists, just update the check
					pod.SyncStatuses.GetStatus(serviceName).SetError(d.consulClient.Agent().UpdateTTL(pod.GetServiceID(serviceName), string(notesB), consulApi.HealthPassing))
				}
			} else {
				pod.SyncStatuses.GetStatus(serviceName).SetError(d.consulClient.Agent().ServiceRegister(&consulApi.AgentServiceRegistration{
					ID:      pod.GetServiceID(serviceName),
					Name:    serviceName,
					Port:    pod.GetPort(serviceName), // TODO: error if missing? Or default to first found?
					Address: pod.Status.PodIP,
					Meta: map[string]string{ // TODO: have a tag here say katalog-sync?
						"external-source":      "kubernetes",             // TODO: make this configurable?
						"external-sync-source": "katalog-sync",           // TODO:: configurable?
						"external-k8s-ns":      pod.ObjectMeta.Namespace, /// Lets put in what NS this came from
						ConsulK8sLinkName:      pod.ObjectMeta.SelfLink,  // which includes full path to this (ns, pod name, etc.)
						// TODO: other annotations that get mapped here
					},
					Tags: pod.GetTags(serviceName),

					// TODO: proxy through to us (or sidecar?) for local pod state
					Check: &consulApi.AgentServiceCheck{
						CheckID: pod.GetServiceID(serviceName), // TODO: better name? -- the name cannot have `/` in it -- its used in the API query path
						TTL:     pod.CheckTTL.String(),

						Status: status,         // Current status of check
						Notes:  string(notesB), // Map of container->ready
					},
				}))
			}
		}
	}

	// Delete old ones
	for _, consulService := range consulServices {
		// We skip all services we aren't syncing (in case others are also registering agent services)
		if v, ok := consulService.Meta[ConsulSyncSourceName]; !ok || v != ConsulSyncSourceValue {
			continue
		}

		// If the service exists, skip
		if pod, ok := d.localK8sState[consulService.Meta[ConsulK8sLinkName]]; ok && pod.HasServiceName(consulService.Service) {
			continue
		}

		if err := d.consulClient.Agent().ServiceDeregister(consulService.ID); err != nil {
			return err
		}
	}

	return nil
}

type consulNodeFunc func(*consulApi.CatalogNode) bool

func (d *Daemon) ConsulNodeDoUntil(ctx context.Context, nodeName string, opts *consulApi.QueryOptions, f consulNodeFunc) error {
	for {
		// If the client is no longer waiting, lets stop checking
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		node, m, err := d.consulClient.Catalog().Node(nodeName, opts)
		if err != nil {
			return err
		}
		opts.WaitIndex = m.LastIndex
		if f(node) {
			return nil
		}
	}
}
