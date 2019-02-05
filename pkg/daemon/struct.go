package daemon

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	consulApi "github.com/hashicorp/consul/api"
	"github.com/sirupsen/logrus"
	k8sApi "k8s.io/api/core/v1"
)

var (
	// Annotation names
	ConsulServiceNames        = "katalog-sync.wish.com/service-names"     // comma-separated list of service names
	ConsulServicePort         = "katalog-sync.wish.com/service-port"      // port to use for consul entry
	ConsulServicePortOverride = "katalog-sync.wish.com/service-port-"     // port override to use for a specific service name
	ConsulServiceTags         = "katalog-sync.wish.com/service-tags"      // tags for the service
	ConsulServiceTagsOverride = "katalog-sync.wish.com/service-tags-"     // tags override to use for a specific service name
	SidecarName               = "katalog-sync.wish.com/sidecar"           // Name of sidecar container, only to be set if it exists
	SyncInterval              = "katalog-sync.wish.com/sync-interval"     // How frequently we want to sync this service
	ConsulServiceCheckTTL     = "katalog-sync.wish.com/service-check-ttl" // TTL for the service checks we put in consul
)

func NewPod(pod k8sApi.Pod, dc *DaemonConfig) (*Pod, error) {
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

	return &Pod{
		Pod:          pod,
		SidecarState: sidecarState,
		SyncStatuses: make(map[string]*SyncStatus),

		CheckTTL:     checkTTL,
		SyncInterval: syncInterval,
	}, nil

}

// Our representation of a pod in k8s
type Pod struct {
	k8sApi.Pod
	*SidecarState
	// map servicename -> sync status
	SyncStatuses

	CheckTTL     time.Duration
	SyncInterval time.Duration
}

// TODO: check if there needs to be a change to the given service based on this pod
func (p *Pod) HasChange(service *consulApi.AgentService) bool {
	return false
}

// GetID returns an identifier that addresses this pod.
func (p *Pod) GetServiceID(serviceName string) string {
	// ServiceID is katalog-sync_service_namespace_pod
	return strings.Join([]string{
		"katalog-sync",
		serviceName,
		p.Pod.ObjectMeta.Namespace,
		p.Pod.ObjectMeta.Name,
	}, "_")
}

func (p *Pod) UpdatePod(k8sPod k8sApi.Pod) {
	p.Pod = k8sPod
}

func (p *Pod) GetServiceNames() []string {
	return strings.Split(p.Pod.ObjectMeta.Annotations[ConsulServiceNames], ",")
}

func (p *Pod) HasServiceName(n string) bool {
	for _, name := range p.GetServiceNames() {
		if name == n {
			return true
		}
	}
	return false
}

func (p *Pod) GetTags(n string) []string {
	if tagStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServiceTagsOverride+n]; ok {
		return strings.Split(tagStr, ",")
	}

	return strings.Split(p.Pod.ObjectMeta.Annotations[ConsulServiceTags], ",")
}

func (p *Pod) GetPort(n string) int {
	if portStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServicePortOverride+n]; ok {
		port, err := strconv.Atoi(portStr)
		if err == nil {
			return port
		} else {
			logrus.Errorf("Unable to parse port from annotation %s: %v", portStr, err)
		}
	}

	// First we look for a port in an annotation
	if portStr, ok := p.Pod.ObjectMeta.Annotations[ConsulServicePort]; ok {
		port, err := strconv.Atoi(portStr)
		if err == nil {
			return port
		} else {
			logrus.Errorf("Unable to parse port from annotation %s: %v", portStr, err)
		}
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
	podReady := true
	containerReadiness := make(map[string]bool)
	for _, containerStatus := range p.Pod.Status.ContainerStatuses {
		// If we have a sidecar defined, we skip the container for it -- as the request showed up
		if p.SidecarState != nil {
			if containerStatus.Name == p.SidecarState.SidecarName {
				continue
			}
		}
		podReady = podReady && containerStatus.Ready
		containerReadiness[containerStatus.Name] = containerStatus.Ready
	}
	return podReady, containerReadiness
}

// State from our sidecar service
type SidecarState struct {
	SidecarName string // name of the sidecar container
	Ready       bool
}

type SyncStatuses map[string]*SyncStatus

func (s SyncStatuses) GetStatus(n string) *SyncStatus {
	status, ok := s[n]
	if !ok {
		status = &SyncStatus{}
		s[n] = status
	}
	return status
}

func (s SyncStatuses) GetError() error {
	for _, status := range s {
		if status.LastError != nil {
			return status.LastError
		}
	}

	return nil
}

type SyncStatus struct {
	LastUpdated time.Time
	LastError   error
}

func (s *SyncStatus) SetError(e error) {
	s.LastError = e
	s.LastUpdated = time.Now()
}
