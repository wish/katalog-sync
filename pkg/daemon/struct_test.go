package daemon

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/sergi/go-diff/diffmatchpatch"
	k8sApi "k8s.io/api/core/v1"
)

var podTestDir = "testfiles"

type podTestResult struct {
	Err                      bool                         `json:"error"`
	ServiceNames             []string                     `json:"service_names"`
	ServiceIDs               map[string]string            `json:"service_ids"`
	Tags                     map[string][]string          `json:"tags"`
	Ports                    map[string]int               `json:"ports"`
	Ready                    map[string]map[string]bool   `json:"ready"`
	ServiceMeta              map[string]map[string]string `json:"service_meta"`
	OutstandingReadinessGate bool                         `json:"outstandingReadinessGate,omitempty"`
}

func TestPod(t *testing.T) {
	// Find all tests
	files, err := ioutil.ReadDir(podTestDir)
	if err != nil {
		t.Fatalf("error loading tests: %v", err)
	}

	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		// TODO: subtest stuff
		t.Run(file.Name(), func(t *testing.T) {
			runPodIntegrationTest(t, file.Name())
		})
	}
}

func runPodIntegrationTest(t *testing.T, testDir string) {
	filepath.Walk(path.Join(podTestDir, testDir), func(fpath string, info os.FileInfo, err error) error {
		// If its not a directory, skip it
		if !info.IsDir() {
			return nil
		}

		k8sPod := k8sApi.Pod{}
		b, err := ioutil.ReadFile(path.Join(fpath, "input.json"))
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("Unable to read input: %v", err)
		}

		if err := json.Unmarshal(b, &k8sPod); err != nil {
			t.Fatalf("unable to unmarshal input to pod: %v", err)
		}

		relFilePath, err := filepath.Rel(podTestDir, fpath)
		if err != nil {
			t.Fatalf("Error getting relative path? Shouldn't be possible: %v", err)
		}

		t.Run(relFilePath, func(t *testing.T) {
			result := &podTestResult{
				ServiceIDs:  make(map[string]string),
				Tags:        make(map[string][]string),
				Ports:       make(map[string]int),
				Ready:       make(map[string]map[string]bool),
				ServiceMeta: make(map[string]map[string]string),
			}

			pod, err := NewPod(k8sPod, &DaemonConfig{})
			result.Err = err != nil
			if err == nil {
				result.ServiceNames = pod.GetServiceNames()

				for _, name := range result.ServiceNames {
					result.ServiceIDs[name] = pod.GetServiceID(name)
					result.Tags[name] = pod.GetTags(name)
					result.Ports[name] = pod.GetPort(name)
					_, result.Ready[name] = pod.Ready()
					result.ServiceMeta[name] = pod.GetServiceMeta(name)
				}
			}

			// handle
			if pod != nil {
				pod.HandleReadinessGate()
				result.OutstandingReadinessGate = pod.OutstandingReadinessGate
			}

			b, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				panic(err)
			}
			ioutil.WriteFile(path.Join(fpath, "result.json"), b, 0644)

			baselineResultBytes, err := ioutil.ReadFile(path.Join(fpath, "baseline.json"))
			if err != nil {
				t.Skip("No baseline.json found, skipping comparison")
			} else {
				baselineResultBytes = bytes.TrimSpace(baselineResultBytes)
				resultBytes := bytes.TrimSpace(b)
				if !bytes.Equal(baselineResultBytes, resultBytes) {
					dmp := diffmatchpatch.New()
					diffs := dmp.DiffMain(string(baselineResultBytes), string(resultBytes), false)
					t.Fatalf("Mismatch of results and baseline!\n%s", dmp.DiffPrettyText(diffs))
				}
			}
		})
		return nil
	})
}
