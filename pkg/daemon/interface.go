package daemon

import (
	consulApi "github.com/hashicorp/consul/api"
	k8sApi "k8s.io/api/core/v1"
)

type Kubelet interface {
	GetPodList() (*k8sApi.PodList, error)
}

type ConsulCatalog interface {
	Services() (map[string]*consulApi.AgentService, error)
}

type ConsulAgent interface {
	UpdateTTL(checkID, output, status string) error
	Services() (map[string]*consulApi.AgentService, error)
	ServiceDeregister(serviceID string) error
	ServiceRegister(service *consulApi.AgentServiceRegistration) error
}
