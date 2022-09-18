package managed

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/testingutil"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testUser = "registeredtest7"
	remoteIP = "10.0.0.20"
)

func newUser(uid string) User {
	config := util.MustLoadGlobalConfig()
	client := k8sclient.NewK8sClient(config)
	return NewUser(uid, client, config)
}

func checkStartJobSuccess(pod Pod) error {
	info := pod.GetPodInfo()
	// Check that all keys that should be there are in podInfo
	for annotationKey, annotationValue := range pod.Object.ObjectMeta.Annotations {
		if annotationValue == "copyForFrontend" {
			hasKey := false
			for key := range info.Tokens {
				if key == annotationKey {
					hasKey = true
					break
				}
			}
			if !hasKey {
				return errors.New(fmt.Sprintf("Pod %s has key %s in annotations but not in pod info", pod.Object.Name, annotationKey))
			}
		}
	}
	// Check that all keys in podInfo are supposed to be there and have correct values
	for key, value := range info.Tokens {
		hasKey := false
		for annotationKey, annotationValue := range pod.Object.ObjectMeta.Annotations {
			if annotationKey == key && annotationValue == "copyForFrontend" {
				hasKey = true
				break
			}
		}
		if !hasKey {
			return errors.New(fmt.Sprintf("Pod %s has key %s in tokens, but isn't specified in annotations", pod.Object.Name, key))
		}
		currentValue, err := pod.GetToken(key)
		if err != nil {
			return errors.New(fmt.Sprintf("Error retrieving token for pod %s: %s", pod.Object.Name, err.Error()))
		}
		if currentValue != value {
			return errors.New(fmt.Sprintf("Pod %s has %s=%s in its pod cache, but the real value of the token is %s", pod.Object.Name, key, value, currentValue))
		}
	}

	// Check that ssh service exists if it's supposed to
	sshPort, exists := info.OtherResourceInfo["sshPort"]
	if exists != pod.NeedsSshService() {
		return errors.New(fmt.Sprintf("Pod %s has podInfo with(out) sshPort and doesn't (does) need ssh service", pod.Object.Name))
	}
	if exists {
		newlyRetreivedSshPort, err := pod.getSshPort()
		if err != nil {
			return errors.New(fmt.Sprintf(err.Error()))
		}
		if newlyRetreivedSshPort != sshPort {
			return errors.New(fmt.Sprintf("Pod %s has ssh service on port %s, but cached sshport %s", pod.Object.Name, newlyRetreivedSshPort, sshPort))
		}
	}
	return nil
}

// Test user functions
func TestNewUser(t *testing.T) {
	userIDs := []string{
		"foo@bar",
		"foo",
		"foo@bar.baz",
		"foo.bar@baz",
	}
	for _, uid := range userIDs {
		u := newUser(uid)
		labels := map[string]string{"user": u.Name, "domain": u.Domain}
		if u.UserID != util.GetUserIDFromLabels(labels) {
			t.Fatalf("User contstructed incorrectly with userID %s", uid)
		}
	}
}

func TestListOptions(t *testing.T) {
	tests := []struct {
		input User
		want  metav1.ListOptions
	}{
		{newUser("foo"), metav1.ListOptions{LabelSelector: "user=foo,domain="}},
		{newUser("foo@bar"), metav1.ListOptions{LabelSelector: "user=foo,domain=bar"}},
		{newUser("foo@bar.baz"), metav1.ListOptions{LabelSelector: "user=foo,domain=bar.baz"}},
	}
	for _, test := range tests {
		if test.input.GetListOptions() != test.want {
			t.Fatalf("Bad list options for userID %s", test.input.UserID)
		}
	}
}

func TestListPods(t *testing.T) {
	u := newUser(testUser)
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
	u := newUser(testUser)
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
	tests := []struct {
		input User
		want  string
	}{
		{newUser("foo"), "foo"},
		{newUser("foo@bar"), "foo-bar"},
		{newUser("Foo@Bar"), "Foo-Bar"},
		{newUser("foo@bar.baz"), "foo-bar-baz"},
		{newUser("foo@bar.baz-baz"), "foo-bar-baz-baz"},
		{newUser("foo.bar@bar.baz"), "foo-bar-bar-baz"},
	}
	for _, test := range tests {
		if test.input.GetUserString() != test.want {
			t.Fatalf("Bad list options for userID %s. Got %s, wanted %s", test.input.UserID, test.input.GetUserString(), test.want)
		}
	}
}

func TestCreateDeleteUserStorage(t *testing.T) {
	// It should return without error and receive true for a user whose storage doesn't exist
	u := newUser("foo@bar.baz")
	finished := util.NewReadyChannel(time.Second)
	err := u.DeleteUserStorage(finished)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !finished.Receive() {
		t.Fatal("Received false for deletion of nonexistant user storage")
	}

	// Create storage for this user
	ready := util.NewReadyChannel(u.GlobalConfig.TimeoutCreate)
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
	finished = util.NewReadyChannel(u.GlobalConfig.TimeoutDelete)
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
		u := newUser(userName)
		ready := util.NewReadyChannel(u.GlobalConfig.TimeoutCreate)
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
		u := newUser(userName)
		finished := util.NewReadyChannel(u.GlobalConfig.TimeoutDelete)
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

func TestPodData(t *testing.T) {
	u := newUser(testUser)
	defaultRequests := testingutil.GetStandardPodRequests()
	err := testingutil.EnsureUserHasEach(u.UserID, defaultRequests, u.GlobalConfig)
	if err != nil {
		t.Fatalf(err.Error())
	}

	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf(err.Error())
	}

	for _, pod := range podList {
		// check that NeedsSshService is correct
		podType := ""
		for key := range defaultRequests {
			if strings.Contains(pod.Object.Name, key) {
				podType = key
				break
			}
		}
		// If the pod matches one of the pod types from the default request map,
		if podType != "" {
			if pod.NeedsSshService() != defaultRequests[podType].Supplementary.NeedsSsh {
				t.Fatalf("Pod %s NeedsSshService() returns %t but should be %t", pod.Object.Name, pod.NeedsSshService(), defaultRequests[podType].Supplementary.NeedsSsh)
			}
		}

		info := pod.GetPodInfo()
		if info.PodName != pod.Object.Name {
			t.Fatalf("Pod %s wrong name", pod.Object.Name)
		}
		if info.Owner != u.UserID {
			t.Fatalf("Pod %s wrong owner", pod.Object.Name)
		}

		err := checkStartJobSuccess(pod)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestJobs(t *testing.T) {
	// Make sure the user has one of each of the standard pod types to attempt to rerun jobs
	u := newUser(testingutil.TestUser)
	defaultRequests := testingutil.GetStandardPodRequests()
	err := testingutil.EnsureUserHasEach(u.UserID, defaultRequests, u.GlobalConfig)
	if err != nil {
		t.Fatalf("Couldn't ensure user had all pods: %s", err.Error())
	}

	podList, err := u.ListPods()
	if err != nil {
		t.Fatal(err.Error())
	}

	for _, pod := range podList {
		readyToDelete := util.NewReadyChannel(time.Second)
		readyToDelete.Send(true)
		finishedDeleteJobs := util.NewReadyChannel(u.GlobalConfig.TimeoutDelete)
		pod.RunDeleteJobsWhenReady(readyToDelete, finishedDeleteJobs)
		if !finishedDeleteJobs.Receive() {
			t.Fatalf("Pod %s failed to complete delete jobs", pod.Object.Name)
		}
		// Now check that podcache and potential services have been deleted
		_, err := pod.loadPodCache()
		if !os.IsNotExist(err) {
			t.Fatalf("Pod %s loading cache after delete job gets error \"%s\" when should be does not exist", pod.Object.Name, err.Error())
		}
		serviceList, err := pod.ListServices()
		if err != nil {
			t.Fatalf("Pod %s couldn't list services: %s", pod.Object.Name, err.Error())
		}
		if len(serviceList.Items) != 0 {
			t.Fatalf("Pod %s still has remaining services after delete job", pod.Object.Name)
		}

		var readyToStartJobs []*util.ReadyChannel
		finishedStartJobs := util.NewReadyChannel(u.GlobalConfig.TimeoutCreate)
		pod.RunStartJobsWhenReady(readyToStartJobs, finishedStartJobs)
		if !finishedStartJobs.Receive() {
			t.Fatalf("Pod %s didn't finish start jobs", pod.Object.Name)
		}

		err = checkStartJobSuccess(pod)
		if err != nil {
			t.Fatal(err)
		}

		// Now just check podCache deletion and reloading in reload mode
		// (because RunStartJobsWhenReady allows multiple attempts to get tokens)
		err = os.Remove(pod.GetCacheFilename())
		if err != nil {
			t.Fatalf("Error deleting podcache for pod %s: %s", pod.Object.Name, err.Error())
		}
		err = pod.CreateAndSavePodCache(true)
		if err != nil {
			t.Fatalf("Error reloading podCache for pod %s: %s", pod.Object.Name, err.Error())
		}
		// Check after this last reload
		err = checkStartJobSuccess(pod)
		if err != nil {
			t.Fatal(err)
		}
	}
}
