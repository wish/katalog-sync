package daemon

import (
	consulApi "github.com/hashicorp/consul/api"
	k8sApi "k8s.io/api/core/v1"
)

// Kubelet encapsulates the interface for kubelet interaction
type Kubelet interface {
	GetPodList() (*k8sApi.PodList, error)
}

// ConsulCatalog encapsulates the interface for interacting with the Catalog API
type ConsulCatalog interface {
	Services() (map[string]*consulApi.AgentService, error)
}

// ConsulAgent encapsulates the interface for interacting with the local agent
// and service API
type ConsulAgent interface {
	UpdateTTL(checkID, output, status string) error
	Services() (map[string]*consulApi.AgentService, error)
	ServiceDeregister(serviceID string) error
	ServiceRegister(service *consulApi.AgentServiceRegistration) error
}
