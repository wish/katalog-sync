package daemon

import (
	"encoding/json"
	"io/ioutil"
	"net/http"

	k8sApi "k8s.io/api/core/v1"
)

func NewKubeletClient(e string) *KubeletClient {
	return &KubeletClient{e}
}

type KubeletClient struct {
	endpoint string
}

func (k *KubeletClient) GetPodList() (*k8sApi.PodList, error) {
	// k8s testing
	resp, err := http.Get(k.endpoint)
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
