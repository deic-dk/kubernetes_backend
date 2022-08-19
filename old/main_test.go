package main

import (
	"testing"

	"fmt"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	//v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//"context"
	//"time"
)

const (
	userID = "registeredtest7"
	userIP = "10.0.0.20"
)

func createJupyterRequest(userIP string, userID string) CreatePodRequest {
	return CreatePodRequest{
		UserID: userID,
		ContainerEnvVars: map[string]map[string]string{
			"jupyter": {
				"FILE":              "foo",
				"WORKING_DIRECTORY": "foobar",
			},
		},
		AllEnvVars: map[string]string{
			"HOME_SERVER": userIP,
			"SD_UID":      userID,
		},
		YamlURL:  "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		RemoteIP: userIP,
	}
}

func deleteJupyterRequest(userIP string, userID string, n int) DeletePodRequest {
	numString := ""
	if n > 0 {
		numString = fmt.Sprintf("-%d", n)
	}
	return DeletePodRequest{
		UserID:   userID,
		RemoteIP: userIP,
		PodName:  fmt.Sprintf("jupyter-%s%s", getUserString(userID), numString),
	}
}

func TestCreationDeletion(t *testing.T) {
	c := clientsetWrapper{clientset: getClientset()}

	// Settings for the test user

	// First clear the user's pods
	ch := make(chan bool, 1)
	err := c.deleteAllPodsUser(deleteJupyterRequest(userIP, userID, 0), ch)
	if err != nil {
		t.Fatalf("Couldn't delete user pods: %s", err.Error())
	}
	if !<-ch {
		t.Fatalf("Failure while deleting all user resources: %s", err.Error())
	} else {
		t.Log("Successfully deleted all user resources")
	}

	// Call createPod a few times
	request := createJupyterRequest(userIP, userID)
	n := 5
	podNames := make([]string, n)
	chans := make([]<-chan bool, n)
	for i := 0; i < n; i++ {
		ch := make(chan bool, 1)
		name, err := c.createPod(request, ch)
		if err != nil {
			t.Fatalf("Couldn't create pod: %s", err.Error())
		}
		podNames[i] = name
		chans[i] = ch
	}
	chanAll := make(chan bool, 1)
	combineBoolChannels(chans, chanAll)
	if <-chanAll {
		t.Logf("Success: created all pods")
	} else {
		t.Fatalf("Pods didn't reach ready state")
	}

	user, domain, _ := strings.Cut(userID, "@")
	podList, err := c.ClientListPods(v1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)})
	if err != nil {
		t.Fatalf("Couldn't list pods: %s", err.Error())
	}
	if len(podList.Items) == n {
		t.Logf("Correct number of pods exist")
	} else {
		t.Fatalf("%d pods exist. Expected %d", len(podList.Items), n)
	}

	storageListOptions := v1.ListOptions{
		LabelSelector: fmt.Sprintf("name=%s", getStoragePVName(userID)),
	}
	PVList, err := c.ClientListPV(storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list PVs: %s", err.Error())
	}
	if len(PVList.Items) == 1 {
		t.Logf("User storage PV exists")
	} else {
		t.Fatalf("User storage PV doesn't exist")
	}

	PVCList, err := c.ClientListPVC(storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list PVCs: %s", err.Error())
	}
	if len(PVCList.Items) == 1 {
		t.Logf("User storage PVC exists")
	} else {
		t.Fatalf("User storage PVC doesn't exist")
	}

	// Then test deletion
	chanAllDelete := make([]<-chan bool, n-1)
	for i := 0; i < (n - 1); i++ {
		ch := make(chan bool, 1)
		err := c.deletePod(deleteJupyterRequest(userIP, userID, i), ch)
		if err != nil {
			t.Fatalf("Failed to delete: %s", err.Error())
		}
		chanAllDelete[i] = ch
	}
	chanDeleted := make(chan bool, 1)
	combineBoolChannels(chanAllDelete, chanDeleted)
	if <-chanDeleted {
		t.Logf("Deleted %d of the pods", n-1)
	} else {
		t.Fatalf("Didn't succeed in deleteding pods")
	}

	// Now there should be one pod and the storage should still be present
	podList, err = c.ClientListPods(v1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)})
	if err != nil {
		t.Fatalf("Couldn't list pods: %s", err.Error())
	}
	if len(podList.Items) == 1 {
		t.Logf("Correct number of pods exist")
	} else {
		t.Fatalf("%d pods exist. Expected %d", len(podList.Items), n)
	}

	PVList, err = c.ClientListPV(storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list PVs: %s", err.Error())
	}
	if len(PVList.Items) == 1 {
		t.Logf("User storage PV still exists")
	} else {
		t.Fatalf("User storage PV doesn't exist")
	}

	PVCList, err = c.ClientListPVC(storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list PVCs: %s", err.Error())
	}
	if len(PVList.Items) == 1 {
		t.Logf("User storage PVC still exists")
	} else {
		t.Fatalf("User storage PVC doesn't exist")
	}

	// Delete the last pod
	chanFinalDelete := make(chan bool)
	err = c.deletePod(deleteJupyterRequest(userIP, userID, n-1), chanFinalDelete)
	if err != nil {
		t.Fatalf("Couldn't delete last pod: %s", err.Error())
	}
	if <-chanFinalDelete {
		t.Logf("Deleted final pod")
	} else {
		t.Fatalf("Final pod wasn't deleted")
	}

	podList, err = c.ClientListPods(v1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)})
	if err != nil {
		t.Fatalf("Couldn't list pods: %s", err.Error())
	}
	if len(podList.Items) == 0 {
		t.Logf("Correct number of pods exist")
	} else {
		t.Fatalf("%d pods exist. Expected %d", len(podList.Items), n)
	}

	// Now the storage should have been removed
	PVList, err = c.ClientListPV(storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list PVs: %s", err.Error())
	}
	if len(PVList.Items) == 0 {
		t.Logf("User storage PV successfully removed")
	} else {
		t.Fatalf("User storage PV still exists")
	}

	PVCList, err = c.ClientListPVC(storageListOptions)
	if err != nil {
		t.Fatalf("Couldn't list PVCs: %s", err.Error())
	}
	if len(PVList.Items) == 0 {
		t.Logf("User storage PVC successfully removed")
	} else {
		t.Fatalf("User storage PVC still exists")
	}

	t.Log("Success, cleaned up created resources")
}

//func TestTokenCopy(t *testing.T) {
//	// Test URI token for jupyter
//	// Test ssh keys for ubuntu
//  // Test that the dirs are cleaned after deletion
//}

//func TestAllImages(t *testing.T) {
//	// Get list of images available in testing repo
//	// Generate a request for each
//	// Check that each reaches ready state
//}

//func TestGetPods(t *testing.T) {
//	// Create a few pods with different users,
//	// Double check with a series of requests that only/all owned pods are shown
//}

//func TestFailedCreate(t *testing.T) {
//	// Make an image that will fail to reach ready state,
//	// ensure that related services are deleted
//}

//func TestLongKeyLength(t *testing.T) {
//	// Make an image with a token longer than the limit,
//	// ensure that only the maximum number of bytes is copied
//}
