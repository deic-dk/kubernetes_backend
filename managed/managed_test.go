package managed

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/testingutil"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	"go.uber.org/goleak"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newUser(uid string) User {
	config := util.MustLoadGlobalConfig()
	if uid == "" {
		uid = config.TestUser
	}
	client := k8sclient.NewK8sClient(config)
	return NewUser(uid, client, config)
}

func checkStartJobSuccess(pod Pod) error {
	info := pod.GetPodInfo()
	// Check that all keys that should be there are in podInfo
	annotationString, hasTokenAnnotation := pod.Object.ObjectMeta.Annotations["sciencedata.dk/copy-token"]
	var annotationKeys []string
	if hasTokenAnnotation {
		annotationKeys = strings.Split(annotationString, ",")
		for _, annotationKey := range annotationKeys {
			hasKey := false
			for key := range info.Tokens {
				if key == annotationKey {
					hasKey = true
					break
				}
			}
			if !hasKey {
				return errors.New(fmt.Sprintf("Pod %s has key %s in copy-token annotation but not in pod info", pod.Object.Name, annotationKey))
			}
		}
	}
	// Check that all tokens in podInfo are supposed to be there
	for key, value := range info.Tokens {
		hasKey := false
		if !hasTokenAnnotation {
			return errors.New(fmt.Sprintf("Pod %s has key %s in podcache but doesn't have a copy-token annotation", pod.Object.Name, key))
		}
		for _, annotationKey := range annotationKeys {
			if annotationKey == key {
				hasKey = true
				break
			}
		}
		if !hasKey {
			return errors.New(fmt.Sprintf("Pod %s has key %s in tokens, but isn't specified in annotations", pod.Object.Name, key))
		}
		// Check whether the value in the podcache matches a newly retrieved key
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
	u := newUser("")

	// Make sure the user has some pods
	err := testingutil.EnsureUserHasNPods(u.UserID, 3)
	if err != nil {
		t.Fatalf("Couldn't create pods for user: %s", err.Error())
	}

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
	u := newUser("")

	// Make sure the user has some pods
	err := testingutil.EnsureUserHasNPods(u.UserID, 3)
	if err != nil {
		t.Fatalf("Couldn't create pods for user: %s", err.Error())
	}

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
	u := newUser("foo@bar.baz")
	finished := util.NewReadyChannel(time.Second)
	// It should return without error and receive true for a user whose storage doesn't exist
	err := u.DeleteUserStorage(finished)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !finished.Receive() {
		t.Fatal("Received false for deletion of nonexistant user storage")
	}

	// Create storage for this user
	ready := util.NewReadyChannel(u.GlobalConfig.TimeoutCreate)
	err = u.CreateUserStorageIfNotExist(ready, u.GlobalConfig.TestingHost)
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
		err := u.CreateUserStorageIfNotExist(ready, u.GlobalConfig.TestingHost)
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
	u := newUser("")
	defaultRequests := testingutil.GetStandardPodRequests()
	err := testingutil.EnsureUserHasEach(u.UserID, defaultRequests)
	if err != nil {
		t.Fatalf(err.Error())
	}

	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf(err.Error())
	}

	for _, pod := range podList {
		// check that NeedsSshService and NeedsIngress are correct
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
			if pod.NeedsIngress() != defaultRequests[podType].Supplementary.NeedsIngress {
				t.Fatalf("Pod %s NeedsIngress() returns %t but should be %t", pod.Object.Name, pod.NeedsIngress(), defaultRequests[podType].Supplementary.NeedsIngress)
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
	u := newUser("")
	defaultRequests := testingutil.GetStandardPodRequests()
	err := testingutil.EnsureUserHasEach(u.UserID, defaultRequests)
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

func TestIngress(t *testing.T) {
	// Ensure user pods are deleted first
	u := newUser("")
	err := testingutil.DeleteAllUserPods(u.UserID)
	if err != nil {
		t.Fatalf("Error deleting user pods: %s", err.Error())
	}

	testingRequests := testingutil.GetTestingPodRequests()
	podName, err := testingutil.CreatePod(testingRequests["http_hello_world"])
	if err != nil {
		t.Fatalf("Couldn't create testing pod: %s", err.Error())
	}

	finished := util.NewReadyChannel(u.GlobalConfig.TimeoutCreate)
	err = testingutil.WatchCreatePod(u.UserID, podName, finished)
	if err != nil {
		t.Fatalf("Couldn't watch creation of testing pod: %s", err.Error())
	}
	if !finished.Receive() {
		t.Fatalf("Testing pod didn't reach ready state")
	}

	pods, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list pods: %s", err.Error())
	}
	if len(pods) != 1 {
		t.Fatalf("The user should have exactly one pod but has %d", len(pods))
	}
	testPod := pods[0]
	t.Logf("Successfully created testing pod, attempting to cURL")

	// Try a few times to make the http request
	var failed error = nil
	for i := 0; i < 10; i++ {
		response, err := http.Get(fmt.Sprintf("https://%s", testPod.getIngressHost()))
		if err != nil {
			failed = err
			t.Logf("Failed %d time(s), waiting a second to try again", i+1)
			time.Sleep(1 * time.Second)
			continue
		}
		defer response.Body.Close()
		// This can get a 404 while the testing container's starting script is running
		if response.StatusCode == 200 {
			failed = nil
		} else {
			failed = errors.New(fmt.Sprintf("Http request to testing pod got response code %d", response.StatusCode))
			t.Logf("Failed %d time(s), waiting a second to try again", i+1)
			time.Sleep(1 * time.Second)
			continue
		}
		body, err := ioutil.ReadAll(response.Body)
		hello := string(body)
		if !strings.Contains(hello, "http hello world") {
			t.Fatalf("Didn't get expected response \"http hello world\", instead got %s", hello)
		}
	}
	if failed != nil {
		t.Fatal(failed)
	}

	// Clean up by deleting the testing pod
	_, err = testingutil.DeletePod(u.UserID, podName)
	if err != nil {
		t.Fatalf("Couldn't delete testing pod: %s", err.Error())
	}
}

func TestSleepBeforeLeakCheck(t *testing.T) {
	t.Log("Start waiting for ReadyChannel goroutines to finish\n")
	u := newUser("")
	time.Sleep(u.GlobalConfig.TimeoutDelete + u.GlobalConfig.TimeoutCreate + 30*time.Second)
	t.Log("Done waiting for ReadyChannel goroutines to finish\n")
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(
		m,
		goleak.IgnoreTopFunction("k8s.io/klog/v2.(*loggingT).flushDaemon"),
		goleak.IgnoreTopFunction("github.com/docker/spdystream.(*Connection).shutdown"),
	)
}
