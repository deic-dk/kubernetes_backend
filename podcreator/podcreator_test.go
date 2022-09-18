package podcreator

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/testingutil"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newUser(uid string) managed.User {
	config := util.MustLoadGlobalConfig()
	client := k8sclient.NewK8sClient(config)
	return managed.NewUser(uid, client, config)
}

func checkEnvironmentVars(pod v1.Pod, request testingutil.CreatePodRequest, mandatoryEnvVars map[string]string) error {
	// Check environment variables are all set in the targetPod
	for containerName, envVars := range request.Settings {
		var targetPodContainer v1.Container
		hasContainer := false
		// Find the container with the matching name
		for _, container := range pod.Spec.Containers {
			if container.Name == containerName {
				targetPodContainer = container
				hasContainer = true
				break
			}
		}
		if !hasContainer {
			return errors.New(fmt.Sprintf("Pod %s doesn't have container %s, should be faulty input in the request containerEnvVars", pod.Name, containerName))
			continue
		}
		for key, value := range envVars {
			hasKey := false
			for _, env := range targetPodContainer.Env {
				if env.Name == key {
					hasKey = true
					if value != env.Value {
						return errors.New(fmt.Sprintf("targetPod %s container has key %s value %s, should be %s", pod.Name, key, env.Value, value))
					}
				}
			}
			if !hasKey {
				return errors.New(fmt.Sprintf("targetPod %s container doesn't have key %s", pod.Name, key))
			}
		}
	}

	// Check that mandatory environment variables are all set
	for _, targetContainer := range pod.Spec.Containers {
		for key, value := range mandatoryEnvVars {
			has := false
			for _, env := range targetContainer.Env {
				if env.Name == key {
					has = true
					if value != env.Value {
						return errors.New(fmt.Sprintf("targetPod %s container %s had incorrectly set key %s = %s, should be %s", pod.Name, targetContainer.Name, key, env.Value, value))
					}
				}
			}
			if !has {
				return errors.New(fmt.Sprintf("targetPod %s container %s doesn't have key %s", pod.Name, targetContainer.Name, key))
			}
		}
	}
	return nil
}

func echoEnvVarInPod(pod managed.Pod, envVar string, nContainer int) (string, string, error) {
	var stdout, stderr bytes.Buffer
	var err error
	stdout, stderr, err = pod.Client.PodExec([]string{"sh", "-c", fmt.Sprintf("echo $%s", envVar)}, pod.Object, nContainer)
	errBytes := stderr.Bytes()
	if err != nil {
		return "", string(errBytes), err
	}
	outBytes := stdout.Bytes()
	return string(outBytes), string(errBytes), nil
}

func TestPodCreation(t *testing.T) {
	// First delete all of the testUser's pods
	t.Log("Deleting all testUser pods")
	err := testingutil.DeleteAllUserPods(testingutil.TestUser)
	if err != nil {
		t.Fatal(err.Error())
	}

	// Then attempt to create two of each of the standard pod types
	defaultRequests := testingutil.GetStandardPodRequests()
	u := newUser(testingutil.TestUser)
	for _, defaultRequest := range defaultRequests {
		for i := 0; i < 2; i++ {
			request := defaultRequest
			// If this is the second pod of this type, add some envVars to the request
			if i == 1 {
				for container, vars := range request.Settings {
					for key, value := range vars {
						request.Settings[container][key] = fmt.Sprintf("%s-extra-with-$pecialchars/\\.'#@:æøå*\\$@.", value)
					}
				}
			}

			pc, err := NewPodCreator(request.YamlURL, u.UserID, testingutil.RemoteIP, request.Settings, u.Client, u.GlobalConfig)
			if err != nil {
				t.Fatalf("Could't initialize podcreator for %s", err.Error())
			}
			if pc.targetPod == nil {
				t.Fatal("Didn't initialize targetPod")
			}

			err = checkEnvironmentVars(*pc.targetPod, request, pc.getMandatoryEnvVars())
			if err != nil {
				t.Fatal(err.Error())
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
