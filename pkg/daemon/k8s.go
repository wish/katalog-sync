package daemon

import (
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"net/http"

	k8sApi "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

// KubeletClientConfig holds the config options for connecting to the kubelet API
type KubeletClientConfig struct {
	APIEndpoint        string `long:"kubelet-api" env:"KUBELET_API" description:"kubelet API endpoint" default:"http://localhost:10255/pods"`
	InsecureSkipVerify bool   `long:"kubelet-api-insecure-skip-verify" env:"KUBELET_API_INSECURE_SKIP_VERIFY" description:"skip verification of TLS certificate from kubelet API"`
}

// NewKubeletClient returns a new KubeletClient based on the given config
func NewKubeletClient(c KubeletClientConfig) (*KubeletClient, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		if err == rest.ErrNotInCluster {
			if c.InsecureSkipVerify {
				tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
				return &KubeletClient{c: c, client: &http.Client{Transport: tr}}, nil
			}

			return &KubeletClient{c: c, client: http.DefaultClient}, nil
		}
		return nil, err
	}
	if c.InsecureSkipVerify {
		config.TLSClientConfig.Insecure = true
		config.TLSClientConfig.CAData = nil
		config.TLSClientConfig.CAFile = ""
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		return nil, err
	}

	return &KubeletClient{c: c, client: &http.Client{Transport: transport}}, nil
}

// KubeletClient is an HTTP client for kubelet that implements the Kubelet interface
type KubeletClient struct {
	c      KubeletClientConfig
	client *http.Client
}

// GetPodList returns the list of pods the kubelet is managing
func (k *KubeletClient) GetPodList() (*k8sApi.PodList, error) {
	// k8s testing
	req, err := http.NewRequest("GET", k.c.APIEndpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var podList k8sApi.PodList
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &podList); err != nil {
		return nil, err
	}
	return &podList, nil
}
