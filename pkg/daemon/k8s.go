package daemon

import (
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"net/http"

	k8sApi "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

type KubeletClientConfig struct {
	APIEndpoint        string `long:"kubelet-api" description:"kubelet API endpoint" default:"http://localhost:10255/pods"`
	InsecureSkipVerify bool   `long:"kubelet-api-insecure-skip-verify" description:"skip verification of TLS certificate from kubelet API"`
}

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

type KubeletClient struct {
	c      KubeletClientConfig
	client *http.Client
}

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
