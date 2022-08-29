package server

import (
	"testing"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/util"
)

func TestDeleteAllUserPods(t *testing.T) {
	s := New(*k8sclient.NewK8sClient())
	request := DeleteAllPodsRequest{UserID: "registeredtest7"}
	finished := util.NewReadyChannel(s.Client.TimeoutDelete)
	err := s.deleteAllUserPods(request.UserID, finished)
	if err != nil {
		t.Fatal(err.Error())
	}
	if finished.Receive() {
		t.Log("Deleted all user pods and storage successfully")
	} else {
		t.Fatal("Failed to delete all user pods and storage")
	}
}

func TestCreateJupyter(t *testing.T) {
	s := New(*k8sclient.NewK8sClient())
	request := CreatePodRequest{
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		UserID: "registeredtest7",
		RemoteIP: "10.0.0.20",
		ContainerEnvVars: map[string]map[string]string{
			"jupyter": {"FILE": "", "WORKING_DIRECTORY": "jupyter"},
		},
	}
	finished := util.NewReadyChannel(s.Client.TimeoutDelete)
	response, err := s.createPod(request, finished)
	if err != nil {
		t.Fatalf("Couldn't call for pod creation %s", err.Error())
	}
	if !finished.Receive() {
		t.Fatalf("Pod didn't reach ready state with completed start jobs")
	}

	u := managed.NewUser("registeredtest7", "", s.Client)
	podList, err := u.ListPods()
	if err != nil {
		t.Logf("Couldn't list pods: %s", err.Error())
	}
	if len(podList) != 1 {
		t.Logf("%d pods exist for the test user, where there should only be the one created", len(podList))
	}
	if podList[0].Object.Name != response.PodName {
		t.Logf("The created pod has a different name than what was returned")
	}
}

func TestAFewMoreJupyterPods(t *testing.T) {
	s := New(*k8sclient.NewK8sClient())
	request := CreatePodRequest{
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		UserID: "registeredtest7",
		RemoteIP: "10.0.0.20",
		ContainerEnvVars: map[string]map[string]string{
			"jupyter": {"FILE": "", "WORKING_DIRECTORY": "jupyter"},
		},
	}
	var chanList []*util.ReadyChannel
	podNames := make(map[string]struct{})
	for i := 0; i < 5; i++ {
		finished := util.NewReadyChannel(s.Client.TimeoutDelete)
		response, err := s.createPod(request, finished)
		if err != nil {
			t.Fatalf("Couldn't call for pod creation %s", err.Error())
		}
		podNames[response.PodName] = struct{}{}
		chanList = append(chanList, finished)
	}
	allReady := util.ReceiveReadyChannels(chanList)
	if !allReady {
		t.Fatalf("One or more pods didn't reach ready state with completed jobs")
	}

	u := managed.NewUser("registeredtest7", "", s.Client)
	podList, err := u.ListPods()
	if err != nil {
		t.Logf("Couldn't list pods: %s", err.Error())
	}
	for _, pod := range podList {
		_, createdInThisTest := podNames[pod.Object.Name]
		if createdInThisTest {
			delete(podNames, pod.Object.Name)
		}
	}
	for podName, _ := range podNames {
		t.Logf("pod %s was supposed to be created, but wasn't present in the list", podName)
	}
}
