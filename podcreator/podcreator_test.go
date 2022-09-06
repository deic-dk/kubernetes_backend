package podcreator

import (
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testUser   = "registeredtest7"
	remoteIP   = "10.0.0.20"
	homeServer = "10.2.0.20"
	testSshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFFaL0dy3Dq4DA5GCqFBKVWZntBSF0RIeVd9/qdhIj2n joshua@myhost"
)

func newUser(uid string) managed.User {
	config := util.MustLoadGlobalConfig()
	client := k8sclient.NewK8sClient(config)
	return managed.NewUser(uid, client, config)
}

func TestPodCreator(t *testing.T) {
	tests := []struct {
		yamlURL          string
		containerEnvVars map[string]map[string]string
	}{
		{
			"https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
			map[string]map[string]string{"jupyter": {"FILE": "", "WORKING_DIRECTORY": "jupyter"}},
		},
		{
			"https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/ubuntu_sciencedata.yaml",
			map[string]map[string]string{"ubuntu-jammy": {"SSH_PUBLIC_KEY": testSshKey}},
		},
	}
	u := newUser(testUser)
	for _, test := range tests {
		pc, err := NewPodCreator(test.yamlURL, testUser, remoteIP, test.containerEnvVars, u.Client, u.GlobalConfig)
		if err != nil {
			t.Fatalf("Could't initialize podcreator for %s", err.Error())
		}
		if pc.targetPod == nil {
			t.Fatal("Didn't initialize targetPod")
		}

		// Check containerEnvVars are all set
		for containerName, envVars := range test.containerEnvVars {
			var targetPodContainer v1.Container
			hasContainer := false
			// Find the container with the matching name
			for _, container := range pc.targetPod.Spec.Containers {
				if container.Name == containerName {
					targetPodContainer = container
					hasContainer = true
					break
				}
			}
			if !hasContainer {
				t.Logf("Pod %s doesn't have container %s, should be faulty input in the request containerEnvVars", pc.targetPod.Name, containerName)
				continue
			}
			for key, value := range envVars {
				hasKey := false
				for _, env := range targetPodContainer.Env {
					if env.Name == key {
						hasKey = true
						if value != env.Value {
							t.Fatalf("targetPod %s container has key %s value %s, should be %s", pc.targetPod.Name, key, env.Value, value)
						}
					}
				}
				if !hasKey {
					t.Fatalf("targetPod %s container doesn't have key %s", pc.targetPod.Name, key)
				}
			}
		}

		// Check mandatoryEnvVars are all set
		mandatoryVars := map[string]string{"HOME_SERVER": homeServer, "SD_UID": testUser}
		for _, targetContainer := range pc.targetPod.Spec.Containers {
			for key, value := range mandatoryVars {
				has := false
				for _, env := range targetContainer.Env {
					if env.Name == key {
						has = true
						if value != env.Value {
							t.Fatalf("targetPod %s container %s had incorrectly set key %s = %s, should be %s", pc.targetPod.Name, targetContainer.Name, key, env.Value, value)
						}
					}
				}
				if !has {
					t.Fatalf("targetPod %s container %s doesn't have key %s", pc.targetPod.Name, targetContainer.Name, key)
				}
			}
		}

		// check targetPod name
		podNameRegex := regexp.MustCompile(fmt.Sprintf("[a-z]+-%s(-\\d)?", u.GetUserString()))
		if !podNameRegex.MatchString(pc.targetPod.Name) {
			t.Fatalf("targetPod name %s doesn't match regex", pc.targetPod.Name)
		}
		listOpt := metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", pc.targetPod.Name)}
		podList, err := u.Client.ListPods(listOpt)
		if err != nil {
			t.Fatal(err.Error())
		}
		if len(podList.Items) != 0 {
			t.Fatalf("targetPod name %s is already taken in the namespace", pc.targetPod.Name)
		}

		// Attempt to create
		ready := util.NewReadyChannel(90 * time.Second)
		response, err := pc.CreatePod(ready)
		if err != nil {
			t.Fatal(err.Error())
		}
		if !ready.Receive() {
			t.Fatalf("Pod %s didn't reach ready", pc.targetPod.Name)
		}

		// Check that pod exists
		podList, err = u.Client.ListPods(listOpt)
		if err != nil {
			t.Fatal(err.Error())
		}
		if len(podList.Items) != 1 {
			t.Fatalf("Pod %s doesn't exist after creation", pc.targetPod.Name)
		}

		// Check that storage exists if required
		storageRequired := false
		for _, volume := range podList.Items[0].Spec.Volumes {
			if volume.Name == "sciencedata" {
				storageRequired = true
				break
			}
		}
		if storageRequired != pc.requiresUserStorage() {
			t.Fatalf("requiresUserStorage returns %t when storageRequired is %t", pc.requiresUserStorage(), storageRequired)
		}
		if storageRequired {
			pvList, err := u.Client.ListPV(u.GetStorageListOptions())
			if err != nil {
				t.Fatal(err.Error())
			}
			if len(pvList.Items) == 0 {
				t.Fatalf("storage required by %s but PV not present", pc.targetPod.Name)
			}
			pvcList, err := u.Client.ListPVC(u.GetStorageListOptions())
			if err != nil {
				t.Fatal(err.Error())
			}
			if len(pvcList.Items) == 0 {
				t.Fatalf("storage required by %s but PVC not present", pc.targetPod.Name)
			}
		}

		// Check start jobs

		// Check for ssh if needed
		for _, container := range response.Object.Spec.Containers {
			for _, port := range container.Ports {
				if port.ContainerPort == 22 {
					// If the pod requires a service to forward port 22
					serviceList, err := response.ListServices()
					if err != nil {
						t.Fatal(err.Error())
					}
					has := false
					for _, svc := range serviceList.Items {
						if svc.Name == fmt.Sprintf("%s-ssh", pc.targetPod.Name) {
							has = true
							break
						}
					}
					if !has {
						t.Fatalf("pod %s requires ssh service and service doesn't exist", pc.targetPod.Name)
					}
					break
				}
			}
		}

		// Check for recently written cache file
		file, err := os.Stat(fmt.Sprintf("/tmp/tokens/%s", pc.targetPod.Name))
		if err != nil {
			t.Fatalf("podcache file error %s", err.Error())
		}
		writtenSecAgo := time.Now().Sub(file.ModTime()).Seconds()
		if writtenSecAgo > 10 {
			t.Fatalf("podcache for %s was written more than 10s ago, probably from an older pod by the same name", pc.targetPod.Name)
		}
	}
}
