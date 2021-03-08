package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	consulApi "github.com/hashicorp/consul/api"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ReadinessGate
const ReadinessGateType = "katalog-sync.wish.com/synced" // name of readiness gate

var (
	// Annotation names
	ConsulServiceNames        = "katalog-sync.wish.com/service-names"     // comma-separated list of service names
	ConsulServicePort         = "katalog-sync.wish.com/service-port"      // port to use for consul entry
	ConsulServicePortOverride = "katalog-sync.wish.com/service-port-"     // port override to use for a specific service name
	ConsulServiceTags         = "katalog-sync.wish.com/service-tags"      // tags for the service
	ConsulServiceTagsOverride = "katalog-sync.wish.com/service-tags-"     // tags override to use for a specific service name
	ConsulServiceMeta         = "katalog-sync.wish.com/service-meta"      // meta for the service
	ConsulServiceMetaOverride = "katalog-sync.wish.com/service-meta-"     // meta override to use for a specific service name
	SidecarName               = "katalog-sync.wish.com/sidecar"           // Name of sidecar container, only to be set if it exists
	SyncInterval              = "katalog-sync.wish.com/sync-interval"     // How frequently we want to sync this service
	ConsulServiceCheckTTL     = "katalog-sync.wish.com/service-check-ttl" // TTL for the service checks we put in consul
	ContainerExclusion        = "katalog-sync.wish.com/container-exclude" // comma-separated list of containers to exclude from ready check
)

// NewPod returns a daemon pod based on a config and a k8s pod
func NewPod(pod corev1.Pod, dc *DaemonConfig) (*Pod, error) {
	var sidecarState *SidecarState
	// If we have an annotation saying we have a sidecar, lets load it
	if sidecarContainerName, ok := pod.ObjectMeta.Annotations[SidecarName]; ok {
		// we want to mark the initial state based on what the sidecar container state
		// is, this way if the daemon gets reloaded we don't require a re-negotiation
		sidecarReady := false
		found := false
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name == sidecarContainerName {
				sidecarReady = containerStatus.Ready
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("Unable to find sidecar container %s", sidecarContainerName)
		}

		sidecarState = &SidecarState{
			SidecarName: sidecarContainerName,
			Ready:       sidecarReady,
		}
	}

	// Check if we have a readiness gate defined
	var ourReadinessGate corev1.PodReadinessGate
	for _, gate := range pod.Spec.ReadinessGates {
		if gate.ConditionType == ReadinessGateType {
			ourReadinessGate = gate
		}
	}

	// Calculate SyncInterval
	syncInterval := dc.DefaultSyncInterval
	if interval, ok := pod.ObjectMeta.Annotations[SyncInterval]; ok {
		duration, err := time.ParseDuration(interval)
		if err != nil {
			return nil, err
		}
		syncInterval = duration
	}

	// Calculate CheckTTL
	checkTTL := dc.DefaultCheckTTL
	if interval, ok := pod.ObjectMeta.Annotations[ConsulServiceCheckTTL]; ok {
		duration, err := time.ParseDuration(interval)
		if err != nil {
			return nil, err
		}
		checkTTL = duration
	}

	// Ensure that the checkTTL is at least SyncTTLBuffer greater than syncTTL
	if minCheckTTL := syncInterval + dc.SyncTTLBuffer; checkTTL < minCheckTTL {
		checkTTL = minCheckTTL
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Pod{
		Pod:                      pod,
		SidecarState:             sidecarState,
		SyncStatuses:             make(map[string]*SyncStatus),
		OutstandingReadinessGate: ourReadinessGate.ConditionType == ReadinessGateType,

		CheckTTL:     checkTTL,
		SyncInterval: syncInterval,
		Ctx:          ctx,
		Cancel:       cancel,
	}, nil

}

// Pod is our representation of a pod in k8s
type Pod struct {
	corev1.Pod
	*SidecarState
	// map servicename -> sync status
	SyncStatuses
	OutstandingReadinessGate bool // Do we have a ReadinessGate to set
	InitialSyncDone          bool // Ready and in consul

	CheckTTL     time.Duration
	SyncInterval time.Duration
	Ctx          context.Context
	Cancel       context.CancelFunc

	l sync.Mutex
}

// HasChange will return whether a change has been made that needs a full resync
// if not then a simple TTL update will suffice
func (p *Pod) HasChange(service *consulApi.AgentService) bool {
	if service.Port != p.GetPort(service.Service) {
		return true
	}

	if service.Address != p.Status.PodIP {
		return true
	}

	return false
}

// GetServiceID returns an identifier that addresses this pod.
func (p *Pod) GetServiceID(serviceName string) string {
	// ServiceID is katalog-sync_service_namespace_pod
	return strings.Join([]string{
		"katalog-sync",
		serviceName,
		p.Pod.ObjectMeta.Namespace,
		p.Pod.ObjectMeta.Name,
	}, "_")
}

// UpdatePod updates the k8s pod
func (p *Pod) UpdatePod(k8sPod corev1.Pod) {
	p.Pod = k8sPod
}

// GetServiceNames returns the list of service names defined in the k8s annotations
func (p *Pod) GetServiceNames() []string {
	return strings.Split(p.Pod.ObjectMeta.Annotations[ConsulServiceNames], ",")
}

// HasServiceName returns whether a given name is one of the annotated service names for this pod
func (p *Pod) HasServiceName(n string) bool {
	for _, name := range p.GetServiceNames() {
		if name == n {
			return true
		}
	}
	return false
}

// GetTags returns the tags for a given service for this pod
// This first checks the service-specific tags, and falls back to the service-level tags
func (p *Pod) GetTags(n string) []string {
	if tagStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServiceTagsOverride+n]; ok {
		return strings.Split(tagStr, ",")
	}

	if tagStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServiceTags]; ok {
		return strings.Split(tagStr, ",")
	}

	return nil
}

// GetServiceMeta returns a map of metadata to be added to the ServiceMetadata
func (p *Pod) GetServiceMeta(n string) map[string]string {
	if metaStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServiceMetaOverride+n]; ok {
		return ParseMap(metaStr)
	}

	if metaStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServiceMeta]; ok {
		return ParseMap(metaStr)
	}

	return nil
}

// GetPort returns the port for a given service for this pod
// This first checks the service-specific port, and falls back to the service-level port
func (p *Pod) GetPort(n string) int {
	if portStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServicePortOverride+n]; ok {
		port, err := strconv.Atoi(portStr)
		if err == nil {
			return port
		}
		logrus.Errorf("Unable to parse port from annotation %s: %v", portStr, err)
	}

	// First we look for a port in an annotation
	if portStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServicePort]; ok {
		port, err := strconv.Atoi(portStr)
		if err == nil {
			return port
		}
		logrus.Errorf("Unable to parse port from annotation %s: %v", portStr, err)
	}

	// If no port was defined, we find the first port we can in the spec and use that
	for _, container := range p.Pod.Spec.Containers {
		for _, port := range container.Ports {
			return int(port.ContainerPort)
		}
	}

	// TODO: error?
	return -1
}

// Ready checks the readiness of the containers in the pod
func (p *Pod) Ready() (bool, map[string]bool) {
	if p.SidecarState != nil {
		if !p.SidecarState.Ready {
			// TODO: change return to be a string that describes? here seems odd to not say anything
			return false, nil
		}
	}

	// If pod is terminating we want to mark it as not-ready (for sync status);
	// This way we mimic the shutdown behavior of normal services (e.g. pods
	// in terminating status are removed as endpoints for servivces)
	// Determining Terminating state as kubectl does (https://github.com/kubernetes/kubernetes/blob/v1.2.0/pkg/kubectl/resource_printer.go#L588)
	if p.DeletionTimestamp != nil {
		return false, nil
	}

	podReady := true
	containerReadiness := make(map[string]bool)
	excludeContainers := p.ContainerExclusion()
	for _, containerStatus := range p.Pod.Status.ContainerStatuses {
		if _, ok := excludeContainers[containerStatus.Name]; ok {
			delete(excludeContainers, containerStatus.Name)
			continue
		}
		// If we have a sidecar defined, we skip the container for it -- as the request showed up
		if p.SidecarState != nil {
			if containerStatus.Name == p.SidecarState.SidecarName {
				continue
			}
		}
		podReady = podReady && containerStatus.Ready
		containerReadiness[containerStatus.Name] = containerStatus.Ready
	}
	if len(excludeContainers) > 0 {
		logrus.Warnf("Some exclude containers for %s not found in pod: %v", p.ObjectMeta.SelfLink, excludeContainers)
	}
	return podReady, containerReadiness
}

// ContainerExclusion returns the containers that should be excluded from a readiness check
func (p *Pod) ContainerExclusion() map[string]struct{} {
	str, ok := p.Pod.ObjectMeta.Annotations[ContainerExclusion]
	if !ok {
		return nil
	}
	excludeContainers := strings.Split(str, ",")

	m := make(map[string]struct{}, len(excludeContainers))
	for _, c := range excludeContainers {
		m[c] = struct{}{}
	}

	return m
}

func (p *Pod) HandleReadinessGate() error {
	p.l.Lock()
	defer p.l.Unlock()
	logrus.Debugf("HandleReadinessGate: %v", p.GetServiceNames())
	// Fast path for things without a readiness gate or with a completed readiness gate
	if !p.OutstandingReadinessGate {
		return nil
	}

	var ourCondition corev1.PodCondition
	for _, condition := range p.Pod.Status.Conditions {
		if condition.Type == ReadinessGateType {
			ourCondition = condition
		}
	}

	logrus.Tracef("condition: %v", ourCondition)

	// If the pod is already marked ready; we are done
	if ourCondition.Status == corev1.ConditionTrue {
		p.OutstandingReadinessGate = false
		return nil
	}

	// We didn't find it, set it!
	if ourCondition.Type != ReadinessGateType {
		ourCondition.Type = ReadinessGateType
	}

	ready, reasonMap := p.Ready()
	if ready {
		// Assuming the pod is ready; we need to check sync status
		var notSyncedServices []string
		for serviceName, status := range p.SyncStatuses {
			if status.LastError != nil {
				notSyncedServices = append(notSyncedServices, serviceName)
			}
		}
		if len(notSyncedServices) != 0 {
			ourCondition.Status = corev1.ConditionFalse
			ourCondition.Reason = "Not all services synced to consul"
			ourCondition.Message = fmt.Sprintf("The following services haven't been synced to consul yet: %s", notSyncedServices)
		} else {
			// check that this ended up in consul as well
			if p.InitialSyncDone {
				ourCondition.Status = corev1.ConditionTrue
				ourCondition.Reason = "Done"
				ourCondition.Message = "Done"
			} else {
				ourCondition.Status = corev1.ConditionFalse
				ourCondition.Reason = "Not synced to remote consul"
				ourCondition.Message = "State synced to local consul, waiting on sync to remote consul"
			}
		}
	} else {
		ourCondition.Status = corev1.ConditionFalse
		ourCondition.Reason = "Not all containers are ready"
		notesB, err := json.MarshalIndent(reasonMap, "", "  ")
		if err != nil {
			panic(err)
		}
		ourCondition.Message = string(notesB)
	}

	logrus.Infof("condition to set: %v", ourCondition)

	patch, err := buildPodConditionPatch(&p.Pod, ourCondition)
	if err != nil {
		return err
	}

	// TODO: pass in
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	podsClient := clientset.CoreV1().Pods(p.Pod.ObjectMeta.Namespace)

	_, err = podsClient.Patch(context.TODO(), p.Pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "status")
	if err != nil {
		return err
	}

	return nil
}

// State from our sidecar service
type SidecarState struct {
	SidecarName string // name of the sidecar container
	Ready       bool
}

// SyncStatuses is a map of SyncStatus for each service defined in a pod (serviceName -> *SyncStatus)
type SyncStatuses map[string]*SyncStatus

// GetStatus returns the SyncStatus for the given serviceName
func (s SyncStatuses) GetStatus(n string) *SyncStatus {
	status, ok := s[n]
	if !ok {
		status = &SyncStatus{}
		s[n] = status
	}
	return status
}

// GetError returns the first error found in the set of SyncStatuses
func (s SyncStatuses) GetError() error {
	for _, status := range s {
		if status.LastError != nil {
			return status.LastError
		}
	}

	return nil
}

// SyncStatus encapsulates the result of the last sync attempt
type SyncStatus struct {
	LastUpdated time.Time
	LastError   error
}

// SetError sets the error and LastUpdated time for the status
func (s *SyncStatus) SetError(e error) {
	s.LastError = e
	s.LastUpdated = time.Now()
}
