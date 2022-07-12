package main

import (
	"testing"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"context"
	"fmt"
	"strings"
)

func TestCreatePod(t *testing.T) {
	clientset := getClientset()
	podClient := clientset.CoreV1().Pods(namespace)
	PVClient := clientset.CoreV1().PersistentVolumes()
	PVCClient := clientset.CoreV1().PersistentVolumeClaims(namespace)

	// Settings for the test user
	userID := "registeredtest7"
	userIP := "10.0.0.20"
	user, domain, _ := strings.Cut(userID, "@")

	// First clear the user's pods
	deleteRequest := DeletePodRequest{
		UserID: userID,
		RemoteIP: userIP,
	}
	err := deleteAllPodsUser(deleteRequest, podClient, PVClient, PVCClient)
	if err != nil {
		t.Fatalf("Couldn't delete user pods: %s", err.Error())
	}

	// Make the CreatePodRequest
	request := CreatePodRequest{
		UserID: userID,
		ContainerEnvVars: map[string]map[string]string{
			"jupyter": map[string]string{
				"FILE": "foo",
				"WORKING_DIRECTORY": "foobar",
			},
		},
		AllEnvVars: map[string]string{
			"HOME_SERVER": userIP,
			"SD_UID": userID,
		},
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		RemoteIP: userIP,
	}

	// Call createPod with the request a few times
	// name0, err := createPod(request, podClient, PVClient, PVCClient)
	name0, err := createPod(request, podClient, PVClient, PVCClient)
	if err != nil {
		t.Fatalf("Couldn't create pod: %s", err.Error())
	}
	name1, err := createPod(request, podClient, PVClient, PVCClient)
	if err != nil {
		t.Fatalf("Couldn't create pod: %s", err.Error())
	}
	name2, err := createPod(request, podClient, PVClient, PVCClient)
	if err != nil {
		t.Fatalf("Couldn't create pod: %s", err.Error())
	}

	t.Logf("Success. Created: %s, %s, %s", name0, name1, name2)

	testuserPodList, err := podClient.List(
		context.TODO(),
		v1.ListOptions{
			LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain),
		},
	)
	if len(testuserPodList.Items) != 3 {
		t.Fatalf("Incorrect number of pods exists after creation")
	}

	storageListOptions := v1.ListOptions{
		LabelSelector: fmt.Sprintf("name=%s", getStoragePVName(userIP, userID)),
	}
	volumeList, err := PVClient.List(context.TODO(), storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list volumes: %s", err.Error())
	}
	if len(volumeList.Items) != 1 {
		t.Fatalf("User storage PV doesn't exist")
	}
	claimList, err := PVCClient.List(context.TODO(), storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list volume claims: %s", err.Error())
	}
	if len(claimList.Items) != 1 {
		t.Fatalf("User storage PVC doesn't exist")
	}

	// Check the success of any started jobs (like copying keys)

	// Clean up
	err = deleteAllPodsUser(deleteRequest, podClient, PVClient, PVCClient)
	if err != nil {
		t.Fatalf("Couldn't delete user pods: %s", err.Error())
	}
	volumeList, err = PVClient.List(context.TODO(), storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list volumes: %s", err.Error())
	}
	if len(volumeList.Items) != 0 {
		t.Fatalf("User storage PV wasn't deleted")
	}
	claimList, err = PVCClient.List(context.TODO(), storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list volume claims: %s", err.Error())
	}
	if len(claimList.Items) != 0 {
		t.Fatalf("User storage PVC wasn't deleted")
	}

	t.Log("Success, cleaned up created resources")

}
