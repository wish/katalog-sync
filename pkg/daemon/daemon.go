package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	consulApi "github.com/hashicorp/consul/api"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	katalogsync "github.com/wish/katalog-sync/proto"
)

var (
	ConsulSyncSourceName  = "external-sync-source"
	ConsulSyncSourceValue = "katalog-sync"
	ConsulK8sLinkName     = "external-k8s-link"
	ConsulK8sNamespace    = "external-k8s-namespace"
	ConsulK8sPod          = "external-k8s-pod"
)

// Metrics
var (
	k8sSyncCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_sync_kubelet_sync_count_total",
		Help: "How many syncs completed from kubelet API, partitioned by success",
	}, []string{"status"})
	k8sSyncSummary = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "katalog_sync_kubelet_sync_duration_seconds",
		Help: "Latency of sync process from kubelet",
	}, []string{"status"})
	consulSyncCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "katalog_sync_consul_sync_count_total",
		Help: "How many syncs completed to consul API, partitioned by success",
	}, []string{"status"})
	consulSyncSummary = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "katalog_sync_consul_sync_duration_seconds",
		Help: "Latency of sync process from kubelet",
	}, []string{"status"})
)

func init() {
	prometheus.MustRegister(
		k8sSyncCount,
		k8sSyncSummary,
		consulSyncCount,
		consulSyncSummary,
	)
}

// DaemonConfig contains the configuration options for a katalog-sync-daemon
type DaemonConfig struct {
	MinSyncInterval     time.Duration `long:"min-sync-interval" env:"MIN_SYNC_INTERVAL" description:"minimum duration allowed for sync" default:"500ms"`
	MaxSyncInterval     time.Duration `long:"max-sync-interval" env:"MAX_SYNC_INTERVAL" description:"maximum duration allowed for sync" default:"5s"`
	DefaultSyncInterval time.Duration `long:"default-sync-interval" env:"DEFAULT_SYNC_INTERVAL" default:"1s"`
	DefaultCheckTTL     time.Duration `long:"default-check-ttl" env:"DEFAULT_CHECK_TTL" default:"10s"`
	SyncTTLBuffer       time.Duration `long:"sync-ttl-buffer-duration" env:"SYNC_TTL_BUFFER_DURATION" description:"how much time to ensure is between sync time and ttl" default:"10s"`
}

// NewDaemon is a helper function to return a new *Daemon
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

// Register handles a sidecar request for registration. This will block until
// (1) the pod excluding the sidecar container is ready
// (2) the service has been pushed to the agent services API
// (3) the entry shows up in the catalog API (meaning it synced to the cluster)
func (d *Daemon) Register(ctx context.Context, in *katalogsync.RegisterQuery) (*katalogsync.RegisterResult, error) {
	if err := d.doSync(ctx); err != nil {
		return nil, err
	}

	k := podCacheKey(in.Namespace, in.PodName)
	pod, ok := d.localK8sState[k]
	if !ok {
		return nil, fmt.Errorf("Unable to find pod with katalog-sync annotation (%s): %s", ConsulServiceNames, k)
	}

	if pod.SidecarState == nil {
		return nil, fmt.Errorf("Pod is missing annotation %s for sidecar", SidecarName)
	}

	pod.SidecarState.SidecarName = in.ContainerName
	pod.SidecarState.Ready = true

	if err := d.doSync(ctx); err != nil {
		return nil, err
	}

	if err := pod.SyncStatuses.GetError(); err != nil {
		return nil, errors.Wrap(err, "Unable to sync status")
	}

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
	}
	return nil, fmt.Errorf("not ready!: %v", pod.SyncStatuses.GetError())
}

// Deregister handles a sidecar request for deregistration. This will block until
// (2) the service has been removed from the agent services API
// (3) the entry has been removed from the catalog API (meaning it synced to the cluster)
func (d *Daemon) Deregister(ctx context.Context, in *katalogsync.DeregisterQuery) (*katalogsync.DeregisterResult, error) {
	if err := d.doSync(ctx); err != nil {
		return nil, err
	}

	k := podCacheKey(in.Namespace, in.PodName)
	pod, ok := d.localK8sState[k]
	if !ok {
		return nil, fmt.Errorf("Unable to find pod with katalog-sync annotation (%s): %s", ConsulServiceNames, k)
	}

	if pod.SidecarState == nil {
		return nil, fmt.Errorf("Pod is missing annotation %s for sidecar", SidecarName)
	}

	pod.SidecarState.Ready = false

	if err := d.doSync(ctx); err != nil {
		return nil, err
	}

	if err := pod.SyncStatuses.GetError(); err != nil {
		return nil, errors.Wrap(err, "Unable to sync status")
	}

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
				status, _, err := d.consulClient.Agent().AgentHealthServiceByID(pod.GetServiceID(serviceName))
				if err == nil {
					// if health status is not fixed and is passing; not done
					if pod.GetServiceHealth(serviceName, "") == "" && status == consulApi.HealthPassing {
						synced = false
					}
				} else {
					// if we got an error; assume it isn't synced
					synced = false
				}
			}
		}
		return synced
	}); err != nil {
		return nil, err
	}

	if ready, _ := pod.Ready(); !ready {
		return nil, nil
	}
	return nil, fmt.Errorf("ready!: %v", pod.SyncStatuses.GetError())
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
		start := time.Now()
		if err := d.fetchK8s(); err != nil {
			k8sSyncCount.WithLabelValues("error").Inc()
			k8sSyncSummary.WithLabelValues("error").Observe(time.Now().Sub(start).Seconds())
			logrus.Errorf("Error fetching state from k8s: %v", err)
		} else {
			k8sSyncCount.WithLabelValues("success").Inc()
			k8sSyncSummary.WithLabelValues("success").Observe(time.Now().Sub(start).Seconds())
		}

		// Do initial sync
		start = time.Now()
		err := d.syncConsul()
		if err != nil {
			consulSyncCount.WithLabelValues("error").Inc()
			consulSyncSummary.WithLabelValues("error").Observe(time.Now().Sub(start).Seconds())
		} else {
			consulSyncCount.WithLabelValues("success").Inc()
			consulSyncSummary.WithLabelValues("success").Observe(time.Now().Sub(start).Seconds())
		}
		return err
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

		key := podCacheKey(pod.Namespace, pod.Name)
		newKeys[key] = struct{}{}
		if existingPod, ok := d.localK8sState[key]; ok {
			existingPod.UpdatePod(pod)
			existingPod.HandleReadinessGate()
		} else {
			p, err := NewPod(pod, &d.c)
			if err != nil {
				logrus.Errorf("error creating local state for pod: %v", err)
			} else {
				d.localK8sState[key] = p
				// If there is an outstanding readinessGate we need to register a wait for remote syncing
				if p.OutstandingReadinessGate {
					go d.waitPod(p)
				}
				// Create readiness gate
				p.HandleReadinessGate()
			}
		}
	}

	// remove any local ones that don't exist anymore
	for k, pod := range d.localK8sState {
		if _, ok := newKeys[k]; !ok {
			pod.Cancel()
			delete(d.localK8sState, k)
		}
	}

	return nil
}

// Background goroutine to wait for a pod to be ready in consul; once done set "InitialSyncDone"
func (d *Daemon) waitPod(pod *Pod) {
	syncedRemotely := false
	for {
		select {
		case <-pod.Ctx.Done():
			return
		default:
		}
		// If we haven't ensured the service is synced remotely; wait on that
		if !syncedRemotely {
			// The goal here is to ensure that the registration has propogated to the rest of the cluster
			nodeName, err := d.consulClient.Agent().NodeName()
			if err != nil {
				time.Sleep(time.Second) // TODO; exponential backoff
				continue                // retry
			}

			opts := &consulApi.QueryOptions{AllowStale: true, UseCache: true}
			if err := d.ConsulNodeDoUntil(pod.Ctx, nodeName, opts, func(node *consulApi.CatalogNode) bool {
				synced := true
				for _, serviceName := range pod.GetServiceNames() {
					// If the service exists, then we just need to update
					if _, ok := node.Services[pod.GetServiceID(serviceName)]; !ok {
						synced = false
					}
				}
				return synced
			}); err != nil {
				time.Sleep(time.Second) // TODO; exponential backoff
				continue                // retry
			}
			syncedRemotely = true
		}
		if ready, _ := pod.Ready(); ready {
			pod.InitialSyncDone = true
			// trigger a handle of readiness gate to avoid the poll delay.
			pod.HandleReadinessGate()
			return
		}
	}
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

		status := consulApi.HealthCritical
		if ready {
			status = consulApi.HealthPassing
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
					pod.SyncStatuses.GetStatus(serviceName).SetError(
						d.consulClient.Agent().UpdateTTL(
							pod.GetServiceID(serviceName), string(notesB), pod.GetServiceHealth(serviceName, status)))
				}
			} else {
				// Define the base metadata that katalog-sync requires
				meta := map[string]string{
					"external-source":    "kubernetes",                                               // Define the source of this service; see https://github.com/hashicorp/consul/blob/fc1d9e5d78749edc55249e5e7c1a8f7a24add99d/website/source/docs/platform/k8s/service-sync.html.md#service-meta
					ConsulSyncSourceName: ConsulSyncSourceValue,                                      // Mark this as katalog-sync so we know we generated this
					ConsulK8sLinkName:    podCacheKey(pod.ObjectMeta.Namespace, pod.ObjectMeta.Name), // which includes full path to this (ns, pod name, etc.)
					ConsulK8sNamespace:   pod.ObjectMeta.Namespace,
					ConsulK8sPod:         pod.ObjectMeta.Name,
				}
				// Add in any metadata that the pod annotations define
				for k, v := range pod.GetServiceMeta(serviceName) {
					if _, ok := meta[k]; !ok {
						meta[k] = v
					}
				}
				// Next we actually register the service with consul
				pod.SyncStatuses.GetStatus(serviceName).SetError(d.consulClient.Agent().ServiceRegister(&consulApi.AgentServiceRegistration{
					ID:      pod.GetServiceID(serviceName),
					Name:    serviceName,
					Port:    pod.GetPort(serviceName),
					Address: pod.Status.PodIP,
					Meta:    meta,
					Tags:    pod.GetTags(serviceName),

					Check: &consulApi.AgentServiceCheck{
						CheckID: pod.GetServiceID(serviceName), // TODO: better name? -- the name cannot have `/` in it -- its used in the API query path
						TTL:     pod.CheckTTL.String(),
						Status:  pod.GetServiceHealth(serviceName, status), // Current status of check
						Notes:   string(notesB),                            // Map of container->ready
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

// ConsulNodeDoUntil is a helper to wait until a change has propogated into the CatalogAPI
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

func podCacheKey(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return fmt.Sprintf("%s/%s", namespace, name)
}
