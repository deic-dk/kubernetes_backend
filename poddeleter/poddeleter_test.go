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
	"github.com/deic.dk/user_pods_k8s_backend/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testUser = "registeredtest7"
)

func newUser(uid string) managed.User {
	config := util.MustLoadGlobalConfig()
	client := k8sclient.NewK8sClient(config)
	return managed.NewUser(uid, client, config)
}

func ensureUserHasPodOfType(podType string) error {
	u := newUser(testUser)
	userPodList, err := u.ListPods()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list user pods %s", err.Error()))
	}
	hasPod := false
	for _, pod := range userPodList {
		if strings.Contains(pod.Object.Name, podType) {
			hasPod = true
			break
		}
	}
	if !hasPod {
		return errors.New(fmt.Sprintf("User doesn't have an existing pod of type %s", podType))
	}
	return nil
}

func TestDeletePod(t *testing.T) {
	// Make sure the user has one of each pod type to attempt to delete
	desiredPodTypes := []string{"ubuntu", "jupyter"}
	for _, podType := range desiredPodTypes {
		err := ensureUserHasPodOfType(podType)
		if err != nil {
			t.Fatal(err.Error())
		}
	}

	// Then delete all of the users pods, and for each of them, check that poddeleter works correctly
	u := newUser(testUser)
	podList, err := u.ListPods()
	if err != nil {
		t.Fatal(err.Error())
	}
	for _, pod := range podList {
		pd, err := NewPodDeleter(pod.Object.Name, testUser, u.Client, u.GlobalConfig)
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
