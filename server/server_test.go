package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/testingutil"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	"go.uber.org/goleak"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestMain(m *testing.M) {
	s := newServer()
	time.Sleep(s.GlobalConfig.TimeoutDelete + s.GlobalConfig.TimeoutCreate)
	goleak.VerifyTestMain(m)
}

func echoEnvVarInPod(pod managed.Pod, envVar string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	var err error
	stdout, stderr, err = pod.Client.PodExec([]string{"sh", "-c", fmt.Sprintf("echo %s", envVar)}, pod.Object, 0)
	errBytes := stderr.Bytes()
	if err != nil {
		return "", string(errBytes), err
	}
	outBytes := stdout.Bytes()
	return string(outBytes), string(errBytes), nil
}

func userPVAndPVCExist(u managed.User) (bool, error) {
	pvList, err := u.Client.ListPV(u.GetStorageListOptions())
	if err != nil {
		return false, err
	}
	if len(pvList.Items) != 1 {
		return false, nil
	}
	pvcList, err := u.Client.ListPVC(u.GetStorageListOptions())
	if err != nil {
		return false, err
	}
	if len(pvcList.Items) != 1 {
		return false, nil
	}
	return true, nil
}

func userPVOrPVCExist(u managed.User) (bool, error) {
	pvList, err := u.Client.ListPV(u.GetStorageListOptions())
	if err != nil {
		return false, err
	}
	if len(pvList.Items) > 0 {
		return true, nil
	}
	pvcList, err := u.Client.ListPVC(u.GetStorageListOptions())
	if err != nil {
		return false, err
	}
	if len(pvcList.Items) > 0 {
		return true, nil
	}
	return false, nil
}

func exampleSshService(podName string) *apiv1.Service {
	return &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-ssh", podName),
			Labels: map[string]string{
				"createdForPod": podName,
			},
		},
		Spec: apiv1.ServiceSpec{
			Ports: []apiv1.ServicePort{
				{
					Name:       "ssh",
					Protocol:   apiv1.ProtocolTCP,
					Port:       22,
					TargetPort: intstr.FromInt(22),
				},
			},
			Type: apiv1.ServiceTypeNodePort,
		},
	}
}

func newServer() *Server {
	config := util.MustLoadGlobalConfig()
	client := k8sclient.NewK8sClient(config)
	return New(client, config)
}

func dummyHttpRequest(forwarded string, remoteAddr string) *http.Request {
	request := &http.Request{}
	request.Header = make(map[string][]string)
	if forwarded != "" {
		request.Header["X-Forwarded-For"] = []string{forwarded, "somethingelse"}
	}
	request.RemoteAddr = remoteAddr
	return request
}

func TestRemoteIP(t *testing.T) {
	s := newServer()
	tests := []struct {
		input  *http.Request
		output string
	}{
		{dummyHttpRequest("10.0.0.20:1234", ""), "10.0.0.20"},
		{dummyHttpRequest("1.2.3.4:1234", ""), "1.2.3.4"},
		{dummyHttpRequest("1.2.3.4", ""), "1.2.3.4"},
		{dummyHttpRequest("1.2.3.4", "anything"), "1.2.3.4"},
		{dummyHttpRequest("", ""), ""},
		{dummyHttpRequest("foobar", ""), ""},
		{dummyHttpRequest("foobar", "foobar"), ""},
		{dummyHttpRequest("", "foobar"), ""},
		{dummyHttpRequest("", "1.2.3.4"), "1.2.3.4"},
		{dummyHttpRequest("", "127.0.0.1:1234"), s.GlobalConfig.TestingHost},
		{dummyHttpRequest("127.0.0.1", ""), s.GlobalConfig.TestingHost},
		{dummyHttpRequest("", "::1"), s.GlobalConfig.TestingHost},
		{dummyHttpRequest("", "[::1]:12345"), s.GlobalConfig.TestingHost},
		{dummyHttpRequest("", "[fe80::0]:1234"), "fe80::0"},
		{dummyHttpRequest("", "fe80::0"), "fe80::0"},
	}
	for _, test := range tests {
		output := s.getRemoteIP(test.input)
		if output != test.output {
			t.Fatalf("Failed getRemoteIP(%+v). Got %s, expected %s", test.input, output, test.output)
		}
	}
}

func TestDeleteAllUserPods(t *testing.T) {
	s := newServer()
	// First ensure that the user has at least 2 pods to delete
	err := testingutil.EnsureUserHasNPods(s.GlobalConfig.TestUser, 2)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Make sure the user storage exists
	u := managed.NewUser(s.GlobalConfig.TestUser, s.Client, s.GlobalConfig)
	storageExists, err := userPVAndPVCExist(u)
	if err != nil {
		t.Fatalf("Couldn't check storage exists: %s", err.Error())
	}
	if !storageExists {
		t.Fatal("User storage doesn't exist when it should")
	}

	t.Logf("User has at least two pods and their storage PV and PVC exist. Attempting deleteAllUserPods")

	// Now call delete all Pods and ensure that it works
	deleteAllRequest := DeleteAllPodsRequest{UserID: s.GlobalConfig.TestUser}
	finished := util.NewReadyChannel(2 * s.GlobalConfig.TimeoutDelete)
	err = s.deleteAllUserPods(deleteAllRequest.UserID, finished)
	if err != nil {
		t.Fatal(err.Error())
	}

	// Make sure they were all deleted successfully
	if finished.Receive() {
		t.Log("Deleted all user pods and storage successfully")
	} else {
		t.Fatal("Failed to delete all user pods and storage")
	}
	// Now that they're finished, s.DeletingPods should be empty
	s.mutex.Lock()
	for key, _ := range s.DeletingPods {
		t.Fatalf("key %s still exists in DeletingPods map after all pods were finished deleting", key)
	}
	s.mutex.Unlock()

	// Make sure that the test user has no remaining pods
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list pods: %s", err.Error())
	}
	if len(podList) != 0 {
		var podNameList []string
		for _, pod := range podList {
			podNameList = append(podNameList, pod.Object.Name)
		}
		podNames := strings.Join(podNameList, ", ")
		t.Fatalf("All of the user's pods should have been deleted, but %s remain", podNames)
	}

	// Make sure the user storage no longer exists
	storageExists, err = userPVOrPVCExist(u)
	if err != nil {
		t.Fatalf("Couldn't check storage exists: %s", err.Error())
	}
	if storageExists {
		t.Fatal("User storage does exist when it shouldn't")
	}

	// Make sure that there are no pod caches left for pods the user previously owned
	dir, err := os.Open(s.GlobalConfig.PodCacheDir)
	if err != nil {
		t.Fatalf("Couldn't open token directory: %s", err.Error())
	}
	fileNames, err := dir.Readdirnames(0)
	if err != nil {
		t.Fatalf("Couldn't read file names from token directory: %s", err.Error())
	}
	searchExp := regexp.MustCompile(u.GetUserString())
	for _, name := range fileNames {
		if searchExp.MatchString(name) {
			t.Fatalf("File %s exists and should have been cleaned by deleteAllUserPods", name)
		}
	}
	t.Logf("All user pods were deleted, there are no pod caches matching the username, and the PV and PVC were deleted")
}

func TestStandardPodCreation(t *testing.T) {
	s := newServer()
	// Double check that the user doesn't have any pods
	u := managed.NewUser(s.GlobalConfig.TestUser, s.Client, s.GlobalConfig)
	podList, err := u.ListPods()
	if err != nil {
		t.Fatal(err.Error())
	}
	// If there are some remaining, then call deleteAllUserPods
	if len(podList) != 0 {
		deleteAllRequest := DeleteAllPodsRequest{UserID: s.GlobalConfig.TestUser}
		finished := util.NewReadyChannel(2 * s.GlobalConfig.TimeoutDelete)
		err = s.deleteAllUserPods(deleteAllRequest.UserID, finished)
		if err != nil {
			t.Fatal(err.Error())
		}
		// Make sure they were all deleted successfully
		if finished.Receive() {
			t.Log("Deleted all user pods and storage successfully")
		} else {
			t.Fatal("Failed to delete all user pods and storage")
		}
	}

	// For each of the default requests, try to create and watch two pods through the server's functions
	defaultRequests := testingutil.GetStandardPodRequests()
	for _, request := range defaultRequests {
		for i := 0; i < 2; i++ {
			createRequest := CreatePodRequest{
				YamlURL:          request.YamlURL,
				UserID:           request.UserID,
				ContainerEnvVars: request.Settings,
				RemoteIP:         s.GlobalConfig.TestingHost,
			}
			finished := util.NewReadyChannel(s.GlobalConfig.TimeoutCreate)
			createResponse, err := s.createPod(createRequest, finished)
			podName := createResponse.PodName
			if err != nil {
				t.Fatal(err.Error())
			}

			// There should be an entry in CreatingPods until this finishes
			// Check by making a channel to receive from finished.Receive() for the select statement
			// so that if finished.Receive() returns before the select statement is reached, the default won't run.
			ch := make(chan bool, 1)
			go func() { ch <- finished.Receive() }()
			select {
			case <-ch:
				t.Logf("Pod %s was already created successfully before the CreatingPods entry could be checked", podName)
			default:
				s.mutex.Lock()
				_, exists := s.CreatingPods[podName]
				s.mutex.Unlock()
				if !exists {
					t.Fatalf("CreatingPods entry was absent for pod %s", podName)
				}
			}

			// Test watchCreatePod
			watchRequest := WatchCreatePodRequest{
				PodName: podName,
				UserID:  request.UserID,
			}
			response, err := s.watchCreatePod(watchRequest)
			if err != nil {
				t.Fatalf("Error while watching for pod %s creation: %s", podName, err.Error())
			}
			if response.Ready != finished.Receive() {
				t.Fatalf("watchCreatePod response for pod %s is %t while the ready channel got %t", podName, response.Ready, finished.Receive())
			}
			// Make sure the CreatingPods entry is now empty
			time.Sleep(time.Second)
			s.mutex.Lock()
			_, entryStillExists := s.CreatingPods[podName]
			s.mutex.Unlock()
			if entryStillExists {
				t.Fatalf("CreatingPods entry for pod %s still exists after creation finished", podName)
			}
		}
	}
}

func TestGetPods(t *testing.T) {
	s := newServer()
	u := managed.NewUser(s.GlobalConfig.TestUser, s.Client, s.GlobalConfig)

	// Make sure the user has at least two of each of the standard pods
	err := testingutil.EnsureUserHasNPods(s.GlobalConfig.TestUser, 2*len(testingutil.GetStandardPodRequests()))
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Now call getPods
	request := GetPodsRequest{UserID: s.GlobalConfig.TestUser, RemoteIP: s.GlobalConfig.TestingHost}
	response, err := s.getPods(request)
	if err != nil {
		t.Fatalf("getPods failed %s", err.Error())
	}
	// List the pods
	userPodList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods %s", err.Error())
	}

	// For each listed pod, ensure that it's present in the response
	if len(userPodList) != len(response) {
		t.Fatalf("%d pods were listed by the kubernetes API while %d pods are described by getPods", len(userPodList), len(response))
	}
	for _, existingPod := range userPodList {
		var entry managed.PodInfo
		for _, describedPod := range response {
			if describedPod.PodName == existingPod.Object.Name {
				entry = describedPod
				break
			}
		}
		if entry.PodName == "" {
			t.Fatalf("Pod %s wasn't listed in the getPods response", existingPod.Object.Name)
		}
	}
}

func TestDeletePod(t *testing.T) {
	s := newServer()
	u := managed.NewUser(s.GlobalConfig.TestUser, s.Client, s.GlobalConfig)

	err := testingutil.EnsureUserHasNPods(s.GlobalConfig.TestUser, 2*len(testingutil.GetStandardPodRequests()))
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Now there should be at least two pods. Pick the first one to delete
	userPodList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods %s", err.Error())
	}
	if len(userPodList) < 2 {
		t.Fatal("User should have at least 2 pods but doesn't")
	}
	podName := userPodList[0].Object.Name

	// Attempt to delete with a mismatched podName and userID
	t.Logf("Attempting to delete a pod not owned by the user")
	deleteRequest := DeletePodRequest{
		UserID:   fmt.Sprintf("%s-extrastring", s.GlobalConfig.TestUser),
		PodName:  podName,
		RemoteIP: s.GlobalConfig.TestingHost,
	}
	finished := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
	_, err = s.deletePod(deleteRequest, finished)
	if err == nil {
		t.Fatal("deletePod returned without error when the specified pod wasn't owned by the user")
	}
	if finished.Receive() {
		t.Fatal("finish channel received true after delete pod should have failed")
	}

	t.Logf("Confirmed that the user has at least two pods. Attempting to delete %s", podName)
	// Call for deletion
	deleteRequest = DeletePodRequest{
		UserID:   s.GlobalConfig.TestUser,
		PodName:  podName,
		RemoteIP: s.GlobalConfig.TestingHost,
	}
	finished = util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
	_, err = s.deletePod(deleteRequest, finished)
	if err != nil {
		t.Fatalf("Error calling deletePod: %s", err.Error())
	}

	// There should be an entry in DeletingPods until this finishes
	// Check by making a channel for a select statement
	ch := make(chan bool, 1)
	go func() { ch <- finished.Receive() }()
	select {
	case <-ch:
		t.Logf("Pod %s was already deleted before the DeletingPods entry could be checked", podName)
	default:
		s.mutex.Lock()
		_, exists := s.DeletingPods[podName]
		s.mutex.Unlock()
		if !exists {
			t.Fatalf("DeletingPods entry was absent for pod %s", podName)
		}
	}

	// Make sure it was deleted
	if !finished.Receive() {
		t.Fatal("Pod wasn't deleted correctly")
	}

	// Make sure the DeletingPods entry is now empty
	s.mutex.Lock()
	_, entryStillExists := s.DeletingPods[podName]
	s.mutex.Unlock()
	if entryStillExists {
		t.Fatal("DeletingPods entry still exists after deletion finished")
	}
	t.Logf("deletePod behaved correctly with at least one pod remaining")

	if !s.userHasRemainingPods(u) {
		t.Fatal("userHasRemainingPods should be true at this point")
	}

	t.Logf("Now deleting all but one pod")
	// Now delete pods until only one remains, so we can be sure that the PV and PVC are deleted in the end
	userPodList, err = u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods %s", err.Error())
	}
	var waitChanList []*util.ReadyChannel
	for i := 0; i < len(userPodList)-1; i++ {
		// Call for deletion
		deleteRequest := DeletePodRequest{
			UserID:   s.GlobalConfig.TestUser,
			PodName:  userPodList[i].Object.Name,
			RemoteIP: s.GlobalConfig.TestingHost,
		}
		finished := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
		_, err = s.deletePod(deleteRequest, finished)
		if err != nil {
			t.Fatalf("Error calling deletePod: %s", err.Error())
		}
		waitChanList = append(waitChanList, finished)
	}
	// Wait until they all finish deletion and make sure they were all successful
	if !util.ReceiveReadyChannels(waitChanList) {
		t.Fatal("Not all pods were deleted successfully")
	}

	// Now there should be one pod, so the user storage should still exist.
	storageExists, err := userPVAndPVCExist(u)
	if err != nil {
		t.Fatalf("Couldn't list PV and PVC %s", err.Error())
	}
	if !storageExists {
		t.Fatal("User storage was deleted by deletePod when the user has pods remaining")
	}
	if !s.userHasRemainingPods(u) {
		t.Fatal("userHasRemainingPods should be true at this point")
	}
	t.Logf("Now the user has only one pod, PV and PVC exist.")

	// Now delete the user's final pod
	userPodList, err = u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods %s", err.Error())
	}
	if len(userPodList) != 1 {
		t.Fatalf("User should only have 1 pod left but has %d", len(userPodList))
	}
	deleteRequest = DeletePodRequest{
		UserID:   s.GlobalConfig.TestUser,
		PodName:  userPodList[0].Object.Name,
		RemoteIP: s.GlobalConfig.TestingHost,
	}
	finished = util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
	_, err = s.deletePod(deleteRequest, finished)
	if err != nil {
		t.Fatalf("Error calling deletePod: %s", err.Error())
	}
	s.mutex.Lock()
	storageCleanedEntry, storageCleanedChannelExists := s.DeletingStorage[u.Name]
	s.mutex.Unlock()
	if storageCleanedChannelExists {
		// If the storageCleanedChannel does exist, then receive to check that the storage is cleaned
		if !storageCleanedEntry.readyChannel.Receive() {
			t.Fatal("storageCleanedChannel didn't receive true when deleting the user's last pod")
		}
	} else {
		// If the channel didn't exist, then the user storage should have already been deleted,
		// so proceed immediately to check it.
		t.Logf("storageCleanedChannel was removed from the server.DeletingStorage by the time this check was called")
	}
	// Check that the PV and PVC were deleted
	storageStillExists, err := userPVOrPVCExist(u)
	if err != nil {
		t.Fatalf("Couldn't check for PV or PVC %s", err.Error())
	}
	if storageStillExists {
		t.Fatal("User PV or PVC exists, but the storageClean readyChannel has already sent")
	}

	// Make sure the pod was deleted successfully
	if !finished.Receive() {
		t.Fatal("Pod didn't finish deleting")
	}
	if s.userHasRemainingPods(u) {
		t.Fatal("userHasRemainingPods should be false at this point")
	}
	t.Logf("Last pod and user storage were cleaned successfully")

	t.Logf("Attempting to call deletePod for a pod that doesn't exist")
	deleteRequest = DeletePodRequest{
		UserID:   s.GlobalConfig.TestUser,
		PodName:  "foobar-pod",
		RemoteIP: s.GlobalConfig.TestingHost,
	}
	finished = util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
	_, err = s.deletePod(deleteRequest, finished)
	if err == nil {
		t.Fatal("No error when calling deletePod on a pod that doesn't exist")
	}
	if finished.Receive() {
		t.Fatal("Finished channel received true after deletePod should have failed")
	}
}

func TestWatchers(t *testing.T) {
	s := newServer()
	defaultRequests := testingutil.GetStandardPodRequests()
	var podTypes []string
	for key, _ := range defaultRequests {
		podTypes = append(podTypes, key)
	}
	// take the first default pod type
	defaultRequest := defaultRequests[podTypes[0]]
	createRequest := CreatePodRequest{
		YamlURL:          defaultRequest.YamlURL,
		UserID:           defaultRequest.UserID,
		ContainerEnvVars: defaultRequest.Settings,
		RemoteIP:         s.GlobalConfig.TestingHost,
	}

	// Start the pod
	finished := util.NewReadyChannel(s.GlobalConfig.TimeoutCreate)
	response, err := s.createPod(createRequest, finished)
	if err != nil {
		t.Fatalf("Couldn't call for pod creation %s", err.Error())
	}

	t.Logf("Calling watchCreatePod with both correct and incorrect username")
	correctCreateRequest := WatchCreatePodRequest{PodName: response.PodName, UserID: createRequest.UserID}
	incorrectCreateRequest := WatchCreatePodRequest{PodName: response.PodName, UserID: fmt.Sprintf("%s-extra", createRequest.UserID)}
	errChan := make(chan error, 2)
	go func() {
		response, err := s.watchCreatePod(correctCreateRequest)
		if err != nil {
			errChan <- errors.New(fmt.Sprintf("Error while watching for pod creation %s", err.Error()))
		}
		if !response.Ready {
			errChan <- errors.New(fmt.Sprintf("Got false when watching for pod creation when it should have returned true"))
		}
		errChan <- nil
	}()
	go func() {
		response, err := s.watchCreatePod(incorrectCreateRequest)
		if err == nil {
			errChan <- errors.New(fmt.Sprintf("Didn't get error when watching for pod creating with incorrect user"))
		}
		if response.Ready {
			errChan <- errors.New(fmt.Sprintf("Got true when watching for pod creation with the incorrect userID"))
		}
		errChan <- nil
	}()
	// Now if both behaved correctly, there should be two `nil` errors
	for i := 0; i < 2; i++ {
		err := <-errChan
		if err != nil {
			t.Fatal(err.Error())
		}
	}

	// Make sure the pod started and start jobs ran successfully
	if !finished.Receive() {
		t.Fatal("Pod didn't reach ready state with completed start jobs")
	}

	// Now that it's finished, try watching it again, first with the correct user:
	// Should have no error and return true
	watchCreateResponse, err := s.watchCreatePod(correctCreateRequest)
	if err != nil {
		t.Fatalf("Error while watching for pod creation %s", err.Error())
	}
	if !watchCreateResponse.Ready {
		t.Fatal("Got false when watching for pod creation when it should have returned true")
	}
	// and then with the incorrect user:
	// Should have error and return false
	watchCreateResponse, err = s.watchCreatePod(incorrectCreateRequest)
	if err != nil {
		t.Logf("Got an error in watchCreatePod after creation with the incorrect user %s", err.Error())
	}
	if watchCreateResponse.Ready {
		t.Fatal("Got true when watching for pod creation with the incorrect userID")
	}

	t.Logf("Attempting to watch for pod deletion")
	deleteRequest := DeletePodRequest{PodName: response.PodName, UserID: createRequest.UserID}
	finishedDeleting := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
	_, err = s.deletePod(deleteRequest, finishedDeleting)

	t.Logf("Calling watchDeletePod with both correct and incorrect username")
	correctDeleteRequest := WatchDeletePodRequest{PodName: response.PodName, UserID: createRequest.UserID}
	incorrectDeleteRequest := WatchDeletePodRequest{PodName: response.PodName, UserID: fmt.Sprintf("%s-extra", createRequest.UserID)}
	errChan = make(chan error, 2)
	go func() {
		response, err := s.watchDeletePod(correctDeleteRequest)
		if err != nil {
			errChan <- errors.New(fmt.Sprintf("Error while watching for pod deletion %s", err.Error()))
		}
		if !response.Deleted {
			errChan <- errors.New(fmt.Sprintf("Got false when watching for pod deletion when it should have returned true"))
		}
		errChan <- nil
	}()
	go func() {
		response, err := s.watchDeletePod(incorrectDeleteRequest)
		if err == nil {
			errChan <- errors.New(fmt.Sprintf("Didn't get error when watching for pod deletion with incorrect user"))
		}
		if !response.Deleted {
			errChan <- errors.New(fmt.Sprintf("Got false when watching for pod deletion with the incorrect userID"))
		}
		errChan <- nil
	}()

	// Make sure the pod was deleted successfully
	if !finished.Receive() {
		t.Fatal("Pod wasn't successfully deleted")
	}

	// Now that it's finished, try watching it again
	// Because it's deleted now, the username can't matter
	watchDeleteResponse, err := s.watchDeletePod(correctDeleteRequest)
	if err != nil {
		t.Fatalf("Error while watching for pod deletion %s", err.Error())
	}
	if !watchDeleteResponse.Deleted {
		t.Fatal("Got false when watching for pod deletion when it should have returned true")
	}
}

func TestCleanAllUnused(t *testing.T) {
	s := newServer()

	// Ensure the testuser has some pods and their services that shouldn't be affected by cleanAllUnused
	defaultRequests := testingutil.GetStandardPodRequests()
	err := testingutil.EnsureUserHasEach(s.GlobalConfig.TestUser, defaultRequests)
	if err != nil {
		t.Fatal(err.Error())
	}

	// Make some junk user storage, services, and podcaches
	testUsernames := []string{"foo@bar", "foo@bar.baz", "foo"}
	readyChannels := make([]*util.ReadyChannel, len(testUsernames))
	for i, user := range testUsernames {
		u := managed.NewUser(user, s.Client, s.GlobalConfig)
		ready := util.NewReadyChannel(s.GlobalConfig.TimeoutCreate)
		err := u.CreateUserStorageIfNotExist(ready, s.GlobalConfig.TestingHost)
		if err != nil {
			t.Fatalf("Couldn't create storage for user %s, %s", user, err.Error())
		}
		readyChannels[i] = ready
	}
	if !util.ReceiveReadyChannels(readyChannels) {
		t.Fatal("Not all storages were created")
	}

	testPodNames := []string{"coolpod-1", "example-pod-foo-bar"}
	var testServices []*apiv1.Service
	for _, name := range testPodNames {
		// make the podcache
		filename := fmt.Sprintf("%s/%s", s.GlobalConfig.PodCacheDir, name)
		file, err := os.Create(filename)
		if err != nil {
			t.Fatalf("Couldn't create file %s, %s", filename, err.Error())
		}
		file.Close()
		if err != nil {
			t.Fatalf("Couldn't close file %s, %s", filename, err.Error())
		}
		// make the service
		service := exampleSshService(name)
		testServices = append(testServices, service)
		_, err = s.Client.CreateService(service)
		if err != nil {
			t.Fatalf("Couldn't create service %s, %s", service.Name, err.Error())
		}
	}

	finished := util.NewReadyChannel(3 * s.GlobalConfig.TimeoutDelete)
	err = s.cleanAllUnused(finished)
	if err != nil {
		t.Fatalf(err.Error())
	}
	if !finished.Receive() {
		t.Fatal("Didn't finish cleanAllUnused successfully")
	}

	t.Log("Checking whether all were deleted")

	// Check user storage
	for _, user := range testUsernames {
		u := managed.NewUser(user, s.Client, s.GlobalConfig)
		pvList, err := s.Client.ListPV(u.GetStorageListOptions())
		if err != nil {
			t.Fatal(err.Error())
		}
		if len(pvList.Items) != 0 {
			t.Fatalf("PV %s wasn't deleted", pvList.Items[0].Name)
		}
		pvcList, err := s.Client.ListPVC(u.GetStorageListOptions())
		if err != nil {
			t.Fatal(err.Error())
		}
		if len(pvcList.Items) != 0 {
			t.Fatalf("PVC %s wasn't deleted", pvcList.Items[0].Name)
		}
	}

	// Check podcaches
	for _, podName := range testPodNames {
		filename := fmt.Sprintf("%s/%s", s.GlobalConfig.PodCacheDir, podName)
		_, err := os.Stat(filename)
		if !os.IsNotExist(err) {
			t.Fatalf("Podcache %s was not deleted", filename)
		}
	}

	// Check services
	for _, service := range testServices {
		svcList, err := s.Client.ListServices(
			metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", service.Name)},
		)
		if err != nil {
			t.Fatal(err.Error())
		}
		if len(svcList.Items) != 0 {
			t.Fatalf("Service %s was not deleted", svcList.Items[0].Name)
		}
	}

	// Check that all the user's pods and their related parts still exist
	u := managed.NewUser(s.GlobalConfig.TestUser, s.Client, s.GlobalConfig)
	storageOkay, err := userPVAndPVCExist(u)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !storageOkay {
		t.Fatal("testUser storage not present after cleanAllUnused")
	}
	podList, err := u.ListPods()
	if err != nil {
		t.Fatal(err.Error())
	}
	for podType, _ := range defaultRequests {
		found := false
		var thisPod managed.Pod
		for _, pod := range podList {
			if strings.Contains(pod.Object.Name, podType) {
				found = true
				thisPod = pod
			}
		}
		if !found {
			t.Fatalf("Pod of type %s not found after cleanAllUnused", podType)
		}
		podName := thisPod.Object.Name

		// check podcache
		filename := fmt.Sprintf("%s/%s", s.GlobalConfig.PodCacheDir, podName)
		_, err := os.Stat(filename)
		if err != nil {
			t.Fatalf("podcache error for testUser pod %s after cleanAllUnused %s", podName, err.Error())
		}

		// check services
		if thisPod.NeedsSshService() {
			podSvcList, err := thisPod.ListServices()
			if err != nil {
				t.Fatal(err.Error())
			}
			found := false
			for _, svc := range podSvcList.Items {
				if svc.Name == fmt.Sprintf("%s-ssh", podName) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("Pod %s needs ssh service, but its ssh service wasn't present after cleanAllUnused", podName)
			}
		}
	}

	// delete the testUser pods to clean up
	deleteAllRequest := DeleteAllPodsRequest{UserID: s.GlobalConfig.TestUser}
	finished = util.NewReadyChannel(2 * s.GlobalConfig.TimeoutDelete)
	err = s.deleteAllUserPods(deleteAllRequest.UserID, finished)
	if err != nil {
		t.Fatal(err.Error())
	}
	// Make sure they were all deleted successfully
	if finished.Receive() {
		t.Log("Deleted all user pods and storage successfully")
	} else {
		t.Fatal("Failed to delete all user pods and storage")
	}
}

func TestReloadCache(t *testing.T) {
	s := newServer()
	// First ensure that the user has each of the standard pods
	defaultRequests := testingutil.GetStandardPodRequests()
	err := testingutil.EnsureUserHasEach(s.GlobalConfig.TestUser, defaultRequests)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Then delete their podCaches
	u := managed.NewUser(s.GlobalConfig.TestUser, s.Client, s.GlobalConfig)
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf(err.Error())
	}

	for _, pod := range podList {
		// Delete the cache file
		err := os.Remove(pod.GetCacheFilename())
		if err != nil {
			t.Fatalf("Error deleting podcache for pod %s: %s", pod.Object.Name, err.Error())
		}
	}

	// Reload the podCaches
	err = s.ReloadPodCaches()
	if err != nil {
		t.Fatal(err.Error())
	}

	// Check that all podCaches are there again
	for _, pod := range podList {
		_, err := os.Open(pod.GetCacheFilename())
		if err != nil {
			t.Fatalf("Error loading podCache for pod %s: %s", pod.Object.Name, err.Error())
		}
	}
}

func TestValidUser(t *testing.T) {
	tests := []struct {
		userID string
		valid  bool
	}{
		{"foo@bar@baz", false},
		{"foO@bar", false},
		{"foo", true},
		{"foo@bar", true},
		{"foo@bar.baz", true},
		{"foo@bar.baz-baz", true},
		{"foo-bar.baz@foo.bar-baz", true},
		{"foo.bar@bar.baz", true},
		{"foo@", false},
		{"foo.", false},
		{".foo", false},
		{"-foo", false},
	}
	for _, test := range tests {
		if validUserID(test.userID) != test.valid {
			t.Fatalf("validUserID fails for userID %s: %t and %t", test.userID, validUserID(test.userID), test.valid)
		}

		requests := testingutil.GetStandardPodRequests()
		var request testingutil.CreatePodRequest
		// Set `request` to the first available in the default requests
		for _, defaultRequest := range requests {
			request = defaultRequest
			break
		}

		// Try to create a pod with this userID
		request.UserID = test.userID
		podName, err := testingutil.CreatePod(request)
		// There should be an error iff this test.userID is valid
		if (err == nil) != test.valid {
			t.Fatalf("CreatePod had (didn't have) an error with the (in)valid userID %s", test.userID)
		}

		// Try to list this user's pods
		_, err = testingutil.GetPodNames(test.userID)
		if (err == nil) != test.valid {
			t.Fatalf("GetPods had (didn't have) an error with the (in)valid userID %s", test.userID)
		}

		// Try to delete the pod
		_, err = testingutil.DeletePod(test.userID, podName)
		if (err == nil) != test.valid {
			t.Fatalf("DeletePod had (didn't have) an error with the (in)valid userID %s", test.userID)
		}

		// Try to delete all the user's pods
		err = testingutil.DeleteAllUserPods(test.userID)
		if (err == nil) != test.valid {
			t.Fatalf("DeleteAllUserPods had (didn't have) an error with the (in)valid userID %s", test.userID)
		}
	}
}

func TestGetPodIPOwner(t *testing.T) {
	s := newServer()
	otherUserID := "misc@test.user"
	err := testingutil.EnsureUserHasNPods(s.GlobalConfig.TestUser, 1)
	if err != nil {
		t.Fatalf("Couldn't ensure user has pods: %s", err.Error())
	}
	err = testingutil.EnsureUserHasNPods(otherUserID, 1)
	if err != nil {
		t.Fatalf("Couldn't ensure user has pods: %s", err.Error())
	}
	testPods := func(userID string) {
		u := managed.NewUser(userID, s.Client, s.GlobalConfig)
		podList, err := u.ListPods()
		if err != nil {
			t.Fatalf("Couldn't list pods: %s", err.Error())
		}
		for _, pod := range podList {
			ip := pod.Object.Status.PodIP
			localRequest := GetPodIPOwnerRequest{
				PodIP: ip,
				RemoteIP: s.GlobalConfig.TestingHost,
			}
			returnedUserID := s.getPodIPOwner(localRequest)
			if returnedUserID != userID {
				t.Fatalf("Pod %s has IP %s and owner %s but server.getPodIPOwner returned %s", pod.Object.Name, ip, userID, returnedUserID)
			}
		}
	}
	testPods(s.GlobalConfig.TestUser)
	testPods(otherUserID)
}
