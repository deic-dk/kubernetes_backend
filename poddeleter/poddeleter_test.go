package poddeleter

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/testingutil"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newUser() managed.User {
	config := util.MustLoadGlobalConfig()
	client := k8sclient.NewK8sClient(config)
	return managed.NewUser(config.TestUser, client, config)
}

func ensureUserHasEach(requests map[string]testingutil.CreatePodRequest) error {
	u := newUser()
	userPodList, err := u.ListPods()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list user pods %s", err.Error()))
	}
	var chanList []*util.ReadyChannel
	// For each of the standard pod types,
	for podType, request := range requests {
		hasPod := false
		// Look through the test user's PodList to see if one exists already
		for _, pod := range userPodList {
			if strings.Contains(pod.Object.Name, podType) {
				hasPod = true
				break
			}
		}
		// If they don't already have one, then create it
		if !hasPod {
			podName, err := testingutil.CreatePod(request)
			if err != nil {
				return err
			}
			finished := util.NewReadyChannel(u.GlobalConfig.TimeoutCreate)
			go testingutil.WatchCreatePod(u.UserID, podName, finished)
			chanList = append(chanList, finished)
		}
	}
	if !util.ReceiveReadyChannels(chanList) {
		return errors.New("Not all pods created for testing reached ready state")
	}
	return nil
}

func TestFailDeletePods(t *testing.T) {
	// Make sure the user has one of each of the standard pod types to attempt to delete
	u := newUser()
	defaultRequests := testingutil.GetStandardPodRequests()
	err := testingutil.EnsureUserHasEach(u.UserID, defaultRequests)
	if err != nil {
		t.Fatalf("Couldn't ensure user had all pods: %s", err.Error())
	}

	// Then attempt to delete one with an incorrect userID
	podList, err := u.ListPods()
	if err != nil {
		t.Fatal(err.Error())
	}
	var podToDelete managed.Pod
	// Range over defaultRequests just to take the first key
	for podType, _ := range defaultRequests {
		found := false
		for _, pod := range podList {
			if strings.Contains(pod.Object.Name, podType) {
				found = true
				podToDelete = pod
				break
			}
		}
		if !found {
			t.Fatalf("Couldn't find pod of type %s after it should have been created", podType)
		}
		break
	}
	t.Logf("Making incorret attempts to delete pod %s", podToDelete.Object.Name)

	tryUserIDs := []string{"fail@user", "", "fail", "fail@user.id"}
	for _, tryUserID := range tryUserIDs {
		failPodDeleter, err := NewPodDeleter(podToDelete.Object.Name, tryUserID, u.Client, u.GlobalConfig)
		if err == nil {
			t.Fatalf("Initialized podDeleter without failure when using incorrect userID")
		}
		finished := util.NewReadyChannel(u.GlobalConfig.TimeoutDelete)
		err = failPodDeleter.DeletePod(finished)
		if err == nil {
			t.Fatalf("podDeleter that wasn't initialized correctly didn't return error when calling DeletePod")
		}
	}
}

func TestDeletePod(t *testing.T) {
	// Make sure the user has one of each of the standard pod types to attempt to delete
	u := newUser()
	defaultRequests := testingutil.GetStandardPodRequests()
	err := testingutil.EnsureUserHasEach(u.UserID, defaultRequests)
	if err != nil {
		t.Fatalf("Couldn't ensure user had all pods: %s", err.Error())
	}

	// Then delete one of each of the user's pods for each of the standard pod types,
	// first create the slice of podsToDelete by finding one of each type in the
	// user's podList
	podList, err := u.ListPods()
	if err != nil {
		t.Fatal(err.Error())
	}
	var podsToDelete []managed.Pod
	for podType := range defaultRequests {
		found := false
		for _, pod := range podList {
			if strings.Contains(pod.Object.Name, podType) {
				found = true
				podsToDelete = append(podsToDelete, pod)
				break
			}
		}
		if !found {
			t.Fatalf("Error, user should have a pod of type %s but doesn't", podType)
		}
	}

	// Then delete them all
	for _, pod := range podsToDelete {
		pd, err := NewPodDeleter(pod.Object.Name, u.UserID, u.Client, u.GlobalConfig)
		if err != nil {
			t.Fatalf("Couldn't initialize pod deleter %s", err.Error())
		}
		if pd.Pod.Object.Name != pod.Object.Name {
			t.Fatalf("Incorrect pod in podDeleter %s, expected %s", pd.Pod.Object.Name, pod.Object.Name)
		}

		// Make sure pod exists
		opt := metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", pod.Object.Name)}
		manualPodList, err := u.Client.ListPods(opt)
		if err != nil {
			t.Fatal(err.Error())
		}
		if len(manualPodList.Items) != 1 {
			t.Fatalf("Should be 1 pod %s, but there are %d", pod.Object.Name, len(manualPodList.Items))
		}

		// Get a list of its services
		serviceList, err := pd.Pod.ListServices()
		if err != nil {
			t.Fatal(err.Error())
		}

		// Call for deletion
		finished := util.NewReadyChannel(90 * time.Second)
		err = pd.DeletePod(finished)
		if err != nil {
			t.Fatal(err.Error())
		}
		// Wait for deletion
		if !finished.Receive() {
			t.Fatalf("Pod %s didn't delete", pod.Object.Name)
		}

		// Check deletion
		manualPodList, err = u.Client.ListPods(opt)
		if err != nil {
			t.Fatal(err.Error())
		}
		if len(manualPodList.Items) != 0 {
			t.Fatalf("Should be 1 pod %s, but there are %d", pod.Object.Name, len(manualPodList.Items))
		}

		// Check that delete jobs were successful
		// First that tokenFile was deleted
		tokenFile := fmt.Sprintf("/tmp/tokens/%s", pod.Object.Name)
		_, err = os.Stat(tokenFile)
		if !os.IsNotExist(err) {
			t.Fatalf("token file %s still exists", tokenFile)
		}
		// Then that services were deleted
		for _, svc := range serviceList.Items {
			opt := metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", svc.Name)}
			manualSvcList, err := u.Client.ListServices(opt)
			if err != nil {
				t.Fatal(err.Error())
			}
			if len(manualSvcList.Items) != 0 {
				t.Fatalf("Service %s wasn't deleted", svc.Name)
			}
		}
	}
}
