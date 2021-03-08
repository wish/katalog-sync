package daemon

import (
	"encoding/json"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

func ParseMap(s string) map[string]string {
	pairs := strings.Split(s, ",")
	m := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		split := strings.Split(pair, ":")
		if len(split) == 2 {
			m[strings.TrimSpace(split[0])] = strings.TrimSpace(split[1])
		}
	}
	return m
}

func buildPodConditionPatch(pod *corev1.Pod, condition corev1.PodCondition) ([]byte, error) {
	oldData, err := json.Marshal(corev1.Pod{
		Status: corev1.PodStatus{
			Conditions: nil,
		},
	})
	if err != nil {
		return nil, err
	}
	newData, err := json.Marshal(corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: pod.UID}, // only put the uid in the new object to ensure it appears in the patch as a precondition
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{condition},
		},
	})
	if err != nil {
		return nil, err
	}
	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, corev1.Pod{})
	if err != nil {
		return nil, err
	}
	return patchBytes, nil
}
