package podcreator

import (
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/testingutil"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	remoteIP   = "10.0.0.20"
	homeServer = "10.2.0.20"
)

func newUser(uid string) managed.User {
	config := util.MustLoadGlobalConfig()
	client := k8sclient.NewK8sClient(config)
	return managed.NewUser(uid, client, config)
}

func TestPodCreation(t *testing.T) {
	// First delete all of the testUser's pods
	err := testingutil.DeleteAllUserPods(testingutil.TestUser)
	if err != nil {
		t.Fatal(err.Error())
	}

	// Then attempt to create two of each of the standard pod types with default parameters
	defaultRequests := testingutil.GetStandardPodRequests()
	u := newUser(testingutil.TestUser)
	mandatoryEnvVars := map[string]string{"HOME_SERVER": homeServer, "SD_UID": u.UserID}
	for _, request := range defaultRequests {
		for i := 0; i < 2; i++ {
			pc, err := NewPodCreator(request.YamlURL, u.UserID, remoteIP, request.Settings, u.Client, u.GlobalConfig)
			if err != nil {
				t.Fatalf("Could't initialize podcreator for %s", err.Error())
			}
			if pc.targetPod == nil {
				t.Fatal("Didn't initialize targetPod")
			}

			// Check environment variables are all set in the targetPod
			for containerName, envVars := range request.Settings {
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

			// Check that mandatory environment variables are all set
			for _, targetContainer := range pc.targetPod.Spec.Containers {
				for key, value := range mandatoryEnvVars {
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
			_, err = pc.CreatePod(ready)
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

			// Check that pc.recquiresUserStorage behaves correctly for this pod
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
		}
	}
}

func TestPostCreationState(t *testing.T) {
	u := newUser(testingutil.TestUser)
	podList, err := u.ListPods()
	if err != nil {
		t.Fatal(err.Error())
	}
	// Check that user storage exists
	pvList, err := u.Client.ListPV(u.GetStorageListOptions())
	if err != nil {
		t.Fatal(err.Error())
	}
	if len(pvList.Items) == 0 {
		t.Fatal("User storage PV not present")
	}
	pvcList, err := u.Client.ListPVC(u.GetStorageListOptions())
	if err != nil {
		t.Fatal(err.Error())
	}
	if len(pvcList.Items) == 0 {
		t.Fatal("User storage PVC not present")
	}

	// Check the success of the start jobs for all of the user's pods
	for _, pod := range podList {
		// Check for ssh if needed
		var sshPort int32 = 0
		needsSsh := false
		for _, container := range pod.Object.Spec.Containers {
			for _, port := range container.Ports {
				if port.ContainerPort == 22 {
					needsSsh = true
					// If the pod requires a service to forward port 22
					serviceList, err := pod.ListServices()
					if err != nil {
						t.Fatal(err.Error())
					}
					has := false
					for _, svc := range serviceList.Items {
						if svc.Name == fmt.Sprintf("%s-ssh", pod.Object.Name) {
							has = true
							// get the sshPort
							for _, portEntry := range svc.Spec.Ports {
								if portEntry.TargetPort == intstr.FromInt(22) {
									sshPort = portEntry.NodePort
								}
							}
							break
						}
					}
					if !has {
						t.Fatalf("pod %s requires ssh service and service doesn't exist", pod.Object.Name)
					}
					break
				}
			}
		}

		// Check that the data saved in the podCache matches the state of the pod
		podInfo := pod.GetPodInfo()
		// First the sshPort in OtherResourceInfo
		if needsSsh {
			if sshPort == 0 {
				t.Fatalf("Pod %s needs ssh port, but its service didn't have the listed targetPort", pod.Object.Name)
			}
			sshPortAsString := fmt.Sprintf("%d", sshPort)
			if podInfo.OtherResourceInfo["sshPort"] != sshPortAsString {
				t.Fatalf("Pod %s has a sshPort %d, but %s is saved in its podCache", pod.Object.Name, sshPort, podInfo.OtherResourceInfo["sshPort"])
			}
		}
		// Then all of the tokens
		for key, value := range podInfo.Tokens {
			hasKey := false
			for annotationKey, annotationValue := range pod.Object.ObjectMeta.Annotations {
				if annotationKey == key && annotationValue == "copyForFrontend" {
					hasKey = true
					break
				}
			}
			if !hasKey {
				t.Fatalf("Pod %s has key %s in tokens, but isn't specified in annotations", pod.Object.Name, key)
			}
			currentValue, err := pod.GetToken(key)
			if err != nil {
				t.Fatalf("Error retrieving token for pod %s: %s", pod.Object.Name, err.Error())
			}
			if currentValue != value {
				t.Fatalf("Pod %s has %s=%s in its pod cache, but the real value of the token is %s", pod.Object.Name, key, value, currentValue)
			}
		}
	}
}
