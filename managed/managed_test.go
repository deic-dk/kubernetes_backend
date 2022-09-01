package managed

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testUser = "registeredtest7"
	remoteIP = "10.0.0.20"
)

// Test user functions
func TestNewUser(t *testing.T) {
	userIDs := []string{
		"foo@bar",
		"foo",
		"foo@bar.baz",
		"foo.bar@baz",
	}
	for _, uid := range userIDs {
		u := NewUser(uid, *k8sclient.NewK8sClient())
		labels := map[string]string{"user": u.Name, "domain": u.Domain}
		if u.UserID != util.GetUserIDFromLabels(labels) {
			t.Fatalf("User contstructed incorrectly with userID %s", uid)
		}
	}
}

func TestListOptions(t *testing.T) {
	c := *k8sclient.NewK8sClient()
	tests := []struct {
		input User
		want  metav1.ListOptions
	}{
		{NewUser("foo", c), metav1.ListOptions{LabelSelector: "user=foo,domain="}},
		{NewUser("foo@bar", c), metav1.ListOptions{LabelSelector: "user=foo,domain=bar"}},
		{NewUser("foo@bar.baz", c), metav1.ListOptions{LabelSelector: "user=foo,domain=bar.baz"}},
	}
	for _, test := range tests {
		if test.input.GetListOptions() != test.want {
			t.Fatalf("Bad list options for userID %s", test.input.UserID)
		}
	}
}

func TestListPods(t *testing.T) {
	u := NewUser(testUser, *k8sclient.NewK8sClient())
	// Use u.ListPods
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods")
	}

	// Then use a manual list from the k8sclient
	manualPodList, err := u.Client.ListPods(u.GetListOptions())
	// For each of the manually listed pods,
	for _, existingPod := range manualPodList.Items {
		// Look through ListPods and make sure it's there
		inPodList := false
		for _, listedPod := range podList {
			if listedPod.Object.Name == existingPod.Name {
				inPodList = true
				break
			}
		}
		if !inPodList {
			t.Fatalf("Pod %s wasn't listed in User.ListPods", existingPod.Name)
		}
	}
	if len(podList) != len(manualPodList.Items) {
		t.Fatalf("Mismatched number of user pods listed")
	}
}

func TestOwnership(t *testing.T) {
	u := NewUser(testUser, *k8sclient.NewK8sClient())
	// Use u.ListPods
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods")
	}
	if len(podList) == 0 {
		t.Fatalf("Need to have at least one pod running for this test")
	}
	for _, pod := range podList {
		owns, err := u.OwnsPod(pod.Object.Name)
		if err != nil {
			t.Fatalf(err.Error())
		}
		if !owns {
			t.Fatalf("User thinks they don't own a pod that they do")
		}
	}
	tryPodNames := []string{"foobar-pod", "user-pods-backend", "user-pods-backend-testing"}
	for _, name := range tryPodNames {
		owns, err := u.OwnsPod(name)
		if err != nil {
			t.Fatalf(err.Error())
		}
		if owns {
			t.Fatalf("User thinks they own pod %s, but they don't", name)
		}
	}
}

func TestUserString(t *testing.T) {
	c := *k8sclient.NewK8sClient()
	tests := []struct {
		input User
		want  string
	}{
		{NewUser("foo", c), "foo"},
		{NewUser("foo@bar", c), "foo-bar"},
		{NewUser("Foo@Bar", c), "Foo-Bar"},
		{NewUser("foo@bar.baz", c), "foo-bar-baz"},
		{NewUser("foo@bar.baz-baz", c), "foo-bar-baz-baz"},
		{NewUser("foo.bar@bar.baz", c), "foo-bar-bar-baz"},
	}
	for _, test := range tests {
		if test.input.GetUserString() != test.want {
			t.Fatalf("Bad list options for userID %s. Got %s, wanted %s", test.input.UserID, test.input.GetUserString(), test.want)
		}
	}
}

func TestCreateDeleteUserStorage(t *testing.T) {
	// It should return without error and receive true for a user whose storage doesn't exist
	u := NewUser("foo@bar.baz", *k8sclient.NewK8sClient())
	finished := util.NewReadyChannel(time.Second)
	err := u.DeleteUserStorage(finished)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !finished.Receive() {
		t.Fatal("Received false for deletion of nonexistant user storage")
	}

	// Create storage for this user
	ready := util.NewReadyChannel(30 * time.Second)
	err = u.CreateUserStorageIfNotExist(ready, remoteIP)
	if err != nil {
		t.Fatalf("Failed to create user storage %s", err.Error())
	}
	if !ready.Receive() {
		t.Fatal("Received false for creation of user storage")
	}

	// Check that the PV and PVC were created successfully and that they are bound
	pvcList, err := u.Client.ListPVC(u.GetStorageListOptions())
	if err != nil {
		t.Fatal(err.Error())
	}
	if len(pvcList.Items) != 1 {
		t.Fatalf("There should be exactly 1 pvc listed by the user's storageListOptions, but there are %d", len(pvcList.Items))
	}
	if pvcList.Items[0].Name != "user-storage-foo-bar-baz" {
		t.Fatalf("User PVC has incorrect name: %s", pvcList.Items[0].Name)
	}
	if pvcList.Items[0].Status.Phase != v1.ClaimBound {
		t.Fatalf("Created PVC not bound")
	}

	pvList, err := u.Client.ListPV(u.GetStorageListOptions())
	if err != nil {
		t.Fatal(err.Error())
	}
	if len(pvList.Items) != 1 {
		t.Fatalf("There should be exactly 1 pv listed by the user's storageListOptions, but there are %d", len(pvList.Items))
	}
	if pvList.Items[0].Name != "user-storage-foo-bar-baz" {
		t.Fatalf("User PVC has incorrect name: %s", pvList.Items[0].Name)
	}
	if pvList.Items[0].Status.Phase != v1.VolumeBound {
		t.Fatalf("Created PV not bound")
	}



	// Now that the user storage does exist, it should be possible to delete
	finished = util.NewReadyChannel(30 * time.Second)
	err = u.DeleteUserStorage(finished)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !finished.Receive() {
		t.Fatal("Received false for deletion of existing user storage")
	}
}

// Make sure that the targetStoragePV and PVC are valid for all usernames
func TestUserStorageValidity(t *testing.T) {
	userNames := []string{
		"foo",
		"foo@bar",
		"foo@bar.baz",
		"foo.bar@bar.baz",
		"foobar-baz",
	}

	// Create the storage for each userName
	var readyList []*util.ReadyChannel
	for _, userName := range userNames {
		u := NewUser(userName, *k8sclient.NewK8sClient())
		ready := util.NewReadyChannel(30 * time.Second)
		err := u.CreateUserStorageIfNotExist(ready, remoteIP)
		if err != nil {
			t.Fatalf("Couldn't create storage for user %s: %s", userName, err.Error())
		}
		readyList = append(readyList, ready)
	}
	if !util.ReceiveReadyChannels(readyList) {
		t.Fatalf("Not all user storages were created successfully")
	}

	// Delete the storage for each userName
	var finishedList []*util.ReadyChannel
	for _, userName := range userNames {
		u := NewUser(userName, *k8sclient.NewK8sClient())
		finished := util.NewReadyChannel(30 * time.Second)
		err := u.DeleteUserStorage(finished)
		if err != nil {
			t.Fatalf("Couldn't delete storage for user %s: %s", userName, err.Error())
		}
		finishedList = append(finishedList, finished)
	}
	if !util.ReceiveReadyChannels(finishedList) {
		t.Fatalf("Not all user storages were created successfully")
	}
}

func ensureUserHasPodOfType(podType string) error {
	u := NewUser(testUser, *k8sclient.NewK8sClient())
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

func TestPodData(t *testing.T) {
	podsThatNeedSsh := []string{"ubuntu"}
	podsThatDontNeedSsh := []string{"jupyter"}
	err := ensureUserHasPodOfType("jupyter")
	if err != nil {
		t.Fatalf(err.Error())
	}
	err = ensureUserHasPodOfType("ubuntu")
	if err != nil {
		t.Fatalf(err.Error())
	}
	u := NewUser(testUser, *k8sclient.NewK8sClient())
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf(err.Error())
	}

	// For each of the user's pods,
	for _, pod := range podList {
		// double check that the saved cache matches environment variables
		cache, err := pod.loadPodCache()
		if err != nil {
			t.Fatalf("Couldn't load pod cache for pod %s", pod.Object.Name)
		}
		info := pod.GetPodInfo()
		object := pod.Object
		if info.PodName != object.Name {
			t.Fatalf("Pod %s wrong name", object.Name)
		}
		if info.Owner != u.UserID {
			t.Fatalf("Pod %s wrong owner", object.Name)
		}
		for key, value := range info.Tokens {
			// try to re-copy the token from the running pod
			pod.getToken(key)
			if value != cache.Tokens[key] {
				t.Fatalf("Cached token %s had value %s, but in the running pod %s is currently %s", key, value, object.Name, cache.Tokens[key])
			}
		}
		sshPort, exists := info.OtherResourceInfo["sshPort"]
		if exists && !pod.needsSshService() {
			t.Fatalf("PodInfo has a listed Ssh port, but pod %s doesn't listen for ssh", object.Name)
		}
		if exists {
			newlyRetreivedSshPort, err := pod.getSshPort()
			if err != nil {
				t.Fatalf(err.Error())
			}
			if newlyRetreivedSshPort != sshPort {
				t.Fatalf("Pod %s has ssh service on port %s, but cached sshport %s", object.Name, newlyRetreivedSshPort, sshPort)
			}
		}

		// check that needsSshService is correct
		for _, podType := range podsThatNeedSsh {
			if strings.Contains(pod.Object.Name, podType) {
				if !pod.needsSshService() {
					t.Fatalf("pod %s needs ssh service but needsSshService returned false", pod.Object.Name)
				}
				svcList, err := pod.ListServices()
				if err != nil {
					t.Fatalf(err.Error())
				}
				hasSshService := false
				for _, svc := range svcList.Items {
					if svc.Name == fmt.Sprintf("%s-ssh", pod.Object.Name) {
						hasSshService = true
						break
					}
				}
				if !hasSshService {
					t.Fatalf("pod %s should have a service for ssh but none exists", pod.Object.Name)
				}
			}
		}
		for _, podType := range podsThatDontNeedSsh {
			if strings.Contains(pod.Object.Name, podType) {
				if pod.needsSshService() {
					t.Fatalf("pod %s doesn't need ssh service but needsSshService returned true", pod.Object.Name)
				}
			}
		}
	}
}

// Test start jobs and delete jobs with podcreator and poddeleter
