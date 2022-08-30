package server

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"errors"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/util"
)

const(
	testUser = "registeredtest7"
	remoteIP = "10.0.0.20"
)

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

func ensureUserHasNPods(n int) error {
	s := New(*k8sclient.NewK8sClient())
	u := managed.NewUser(testUser, "", s.Client)
	userPodList, err := u.ListPods()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list user pods %s", err.Error()))
	}
	if len(userPodList) < n {
		// Then create n jupyter pods
		request := CreatePodRequest{
			YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
			UserID: testUser,
			RemoteIP: remoteIP,
			ContainerEnvVars: map[string]map[string]string{
				"jupyter": {"FILE": "", "WORKING_DIRECTORY": "jupyter"},
			},
		}
		for i := 0; i < n; i++ {
			finished := util.NewReadyChannel(s.Client.TimeoutDelete)
			_, err := s.createPod(request, finished)
			if err != nil {
				return errors.New(fmt.Sprintf("Couldn't call for pod creation %s", err.Error()))
			}
			// Make sure the pod started and start jobs ran successfully
			if !finished.Receive() {
				return errors.New(fmt.Sprintf("Pod didn't reach ready state with completed start jobs"))
			}
		}
	}

	// Double check
	userPodList, err = u.ListPods()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list user pods %s", err.Error()))
	}
	if len(userPodList) < n {
		return errors.New(fmt.Sprintf("User doesn't have at least two pods running after they should have been created."))
	}
	return nil
}


func TestDeleteAllUserPods(t *testing.T) {
	s := New(*k8sclient.NewK8sClient())
	// First ensure that the user has at least 1 pod to delete
	err := ensureUserHasNPods(1)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Make sure the user storage exists
	u := managed.NewUser(testUser, "", s.Client)
	storageExists, err := userPVAndPVCExist(u)
	if err != nil {
		t.Fatalf("Couldn't check storage exists: %s", err.Error())
	}
	if !storageExists {
		t.Fatalf("User storage doesn't exist when it should")
	}

	t.Logf("User has at least one pod and their storage PV and PVC exist. Attempting deleteAllUserPods")

	// Now call delete all Pods and ensure that it works
	deleteAllRequest := DeleteAllPodsRequest{UserID: testUser}
	finished := util.NewReadyChannel(s.Client.TimeoutDelete)
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
	for key, _ := range s.DeletingPods {
		t.Fatalf("key %s still exists in DeletingPods map after all pods were finished deleting", key)
	}

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
		t.Fatalf("User storage does exist when it shouldn't")
	}

	// Make sure that there are no pod caches left for pods the user previously owned
	dir, err := os.Open("/tmp/tokens")
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

func TestCreateJupyter(t *testing.T) {
	fileEnvVar := "testValue42"
	s := New(*k8sclient.NewK8sClient())
	request := CreatePodRequest{
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		UserID: testUser,
		RemoteIP: remoteIP,
		ContainerEnvVars: map[string]map[string]string{
			"jupyter": {"FILE": fileEnvVar, "WORKING_DIRECTORY": "jupyter"},
		},
	}

	// Check that the pod cache doesn't exist yet
	_, err := os.Stat("/tmp/tokens/jupyter-registeredtest7")
	if err == nil {
		t.Fatalf("jupyter pod cache existed before creating the pod")
	}

	t.Logf("Attempting to create a new jupyter pod")

	// Start the pod
	finished := util.NewReadyChannel(s.Client.TimeoutDelete)
	response, err := s.createPod(request, finished)
	if err != nil {
		t.Fatalf("Couldn't call for pod creation %s", err.Error())
	}
	// Make sure the pod started and start jobs ran successfully
	if !finished.Receive() {
		t.Fatalf("Pod didn't reach ready state with completed start jobs")
	}

	// Make sure that the test user has this pod and no others
	u := managed.NewUser(testUser, "", s.Client)
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list pods: %s", err.Error())
	}
	if len(podList) != 1 {
		t.Fatalf("%d pods exist for the test user, where there should only be the one created", len(podList))
	}
	if podList[0].Object.Name != response.PodName {
		t.Fatalf("The created pod has a different name than what was returned")
	}

	t.Logf("Checking the pod's cache and environment variables")
	// Check that the environment variables were set correctly in the pod
	stdout, stderr, err := echoEnvVarInPod(podList[0], "$FILE")
	if err != nil {
		t.Fatalf("Couldn't test environment variable in jupyter pod:\nstderr: %s\nerror: %s", stderr, err.Error())
	}
	// (Note that there may be some kind of EOF character at the end of the stdout buffer)
	if fileEnvVar != strings.TrimSpace(stdout) {
		t.Fatalf("Didn't get correct environment variable in Jupyter pod. Expected %s, got %s", fileEnvVar, stdout)
	}

	// Check that the pod cache exists now
	_, err = os.Stat("/tmp/tokens/jupyter-registeredtest7")
	if err != nil {
		t.Fatalf("Jupyter pod cache wasn't saved")
	}
}

func TestAFewMoreJupyterPods(t *testing.T) {
	s := New(*k8sclient.NewK8sClient())
	request := CreatePodRequest{
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		UserID: testUser,
		RemoteIP: remoteIP,
		ContainerEnvVars: map[string]map[string]string{
			"jupyter": {"FILE": "", "WORKING_DIRECTORY": "jupyter"},
		},
	}
	t.Logf("Attempting to create a few more jupyter pods")
	var chanList []*util.ReadyChannel
	// make this map to check that each pod was created
	podNamesMap := make(map[string]struct{})
	// make this slice to have a sequence corresponding to chanList
	var podNamesList []string
	for i := 0; i < 3; i++ {
		finished := util.NewReadyChannel(s.Client.TimeoutDelete)
		response, err := s.createPod(request, finished)
		if err != nil {
			t.Fatalf("Couldn't call for pod creation %s", err.Error())
		}
		podNamesMap[response.PodName] = struct{}{}
		podNamesList = append(podNamesList, response.PodName)
		chanList = append(chanList, finished)
	}
	// For each pod whose creation was requested,
	for i, podName := range podNamesList {
		// receive from the corresponding ReadyChannel into a chan, so a select statement can see whether it's ready.
		ch := make(chan bool, 1)
		go func() {ch <- chanList[i].Receive()}()
		select {
		// Either a value should be ready in the channel,
		case <-ch:
			t.Logf("The ReadyChannel for pod %s is present in the server's CreatingPodsMap", podName)
		// or the ReadyChannel should exist in the CreatingPodsMap
		default:
			_, exists := s.CreatingPods[podName]
			if !exists {
				t.Fatalf("ReadyChannel for pod %s is not finished but was already removed from CreatingPods", podName)
			}
		}
	}

	// Check that all pods were created successfully
	allReady := util.ReceiveReadyChannels(chanList)
	if !allReady {
		t.Fatalf("One or more pods didn't reach ready state with completed jobs")
	}
	t.Logf("All pods and start jobs completed successfully")

	// Check that each of the pods that were supposed to be created now exist when listing for the user
	u := managed.NewUser(testUser, "", s.Client)
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list pods: %s", err.Error())
	}
	for _, pod := range podList {
		_, createdInThisTest := podNamesMap[pod.Object.Name]
		if createdInThisTest {
			delete(podNamesMap, pod.Object.Name)
		}
	}
	for podName, _ := range podNamesMap {
		t.Fatalf("pod %s was supposed to be created, but wasn't present in the list", podName)
	}
	t.Logf("Each pod that was supposed to have been created now exists when listing")
}

func TestGetPods(t *testing.T) {
	s := New(*k8sclient.NewK8sClient())
	u := managed.NewUser(testUser, "", s.Client)

	// Make sure the user has at least two pods and create them if not
	err := ensureUserHasNPods(2)
	if err != nil {
		t.Fatalf(err.Error())
	}
	t.Logf("User has at least two pods running")

	// Now call getPods
	request := GetPodsRequest{UserID: testUser, RemoteIP: remoteIP}
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
	t.Logf("Each of the user's pods is described in the getPods response")
}

func TestDeletePod(t *testing.T) {
	s := New(*k8sclient.NewK8sClient())
	u := managed.NewUser(testUser, "", s.Client)

	// There should be at least two jupyter pods owned by the testUser from the previous tests.
	// If not, make them.
	ensureUserHasNPods(2)

	// Now there should be at least two pods. Pick the first one to delete
	userPodList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods %s", err.Error())
	}
	if len(userPodList) < 2 {
		t.Fatalf("User should have at least 2 pods but doesn't")
	}
	podName := userPodList[0].Object.Name

	// Attempt to delete with a mismatched podName and userID
	t.Logf("Attempting to delete a pod not owned by the user")
	deleteRequest := DeletePodRequest{
		UserID: fmt.Sprintf("%s-extrastring", testUser),
		PodName: podName,
		RemoteIP: remoteIP,
	}
	finished := util.NewReadyChannel(s.Client.TimeoutDelete)
	err = s.deletePod(deleteRequest, finished)
	if err == nil {
		t.Fatalf("deletePod returned without error when the specified pod wasn't owned by the user")
	}
	if finished.Receive() {
		t.Fatalf("finish channel received true after delete pod should have failed")
	}

	t.Logf("Confirmed that the user has at least two pods. Attempting to delete %s", podName)
	// Call for deletion
	deleteRequest = DeletePodRequest{
		UserID: testUser,
		PodName: podName,
		RemoteIP: remoteIP,
	}
	finished = util.NewReadyChannel(s.Client.TimeoutDelete)
	err = s.deletePod(deleteRequest, finished)
	if err != nil {
		t.Fatalf("Error calling deletePod: %s", err.Error())
	}

	// There should be an entry in DeletingPods until this finishes
	// Check by making a channel for a select statement
	ch := make(chan bool, 1)
	go func() {ch <- finished.Receive()}()
	select {
	case <-ch:
		t.Logf("Pod %s was already deleted before the DeletingPods entry could be checked", podName)
	default:
		_, exists := s.DeletingPods[podName]
		if !exists {
			t.Fatalf("DeletingPods entry was absent for pod %s", podName)
		}
	}

	// Make sure it was deleted
	if !finished.Receive() {
		t.Fatalf("Pod wasn't deleted correctly")
	}

	// Make sure the DeletingPods entry is now empty
	_, entryStillExists := s.DeletingPods[podName]
	if entryStillExists {
		t.Fatalf("DeletingPods entry still exists after deletion finished")
	}
	t.Logf("deletePod behaved correctly with at least one pod remaining")

	if !s.userHasRemainingPods(u) {
		t.Fatalf("userHasRemainingPods should be true at this point")
	}

	t.Logf("Now deleting all but one pod...")
	// Now delete pods until only one remains, so we can be sure that the PV and PVC are deleted in the end
	userPodList, err = u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods %s", err.Error())
	}
	var waitChanList []*util.ReadyChannel
	for i := 0; i < len(userPodList) - 1; i++ {
		// Call for deletion
		deleteRequest := DeletePodRequest{
			UserID: testUser,
			PodName: userPodList[i].Object.Name,
			RemoteIP: remoteIP,
		}
		finished := util.NewReadyChannel(s.Client.TimeoutDelete)
		err = s.deletePod(deleteRequest, finished)
		if err != nil {
			t.Fatalf("Error calling deletePod: %s", err.Error())
		}
		waitChanList = append(waitChanList, finished)
	}
	// Wait until they all finish deletion and make sure they were all successful
	if !util.ReceiveReadyChannels(waitChanList) {
		t.Fatalf("Not all pods were deleted successfully")
	}

	// Now there should be one pod, so the user storage should still exist.
	storageExists, err := userPVAndPVCExist(u)
	if err != nil {
		t.Fatalf("Couldn't list PV and PVC %s", err.Error())
	}
	if !storageExists {
		t.Fatalf("User storage was deleted by deletePod when the user has pods remaining")
	}
	if !s.userHasRemainingPods(u) {
		t.Fatalf("userHasRemainingPods should be true at this point")
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
		UserID: testUser,
		PodName: userPodList[0].Object.Name,
		RemoteIP: remoteIP,
	}
	finished = util.NewReadyChannel(s.Client.TimeoutDelete)
	err = s.deletePod(deleteRequest, finished)
	if err != nil {
		t.Fatalf("Error calling deletePod: %s", err.Error())
	}
	storageCleanedChannel, storageCleanedChannelExists := s.DeletingStorage[u.Name]
	if storageCleanedChannelExists {
		// If the storageCleanedChannel does exist, then receive to check that the storage is cleaned
		if !storageCleanedChannel.Receive() {
			t.Fatalf("storageCleanedChannel didn't receive true when deleting the user's last pod")
		}
	} else {
		// If the channel didn't exist, then the user storage should have already been deleted,
		// so proceed immediately to check it.
		t.Logf("storageCleanedChannel was removed from the server map by the time this check was called")
	}
	// Check that the PV and PVC were deleted
	storageStillExists, err := userPVOrPVCExist(u)
	if err != nil {
		t.Fatalf("Couldn't check for PV or PVC %s", err.Error())
	}
	if storageStillExists {
		t.Fatalf("User PV or PVC exists, but the storageClean readyChannel has already sent")
	}

	// Make sure the pod was deleted successfully
	if !finished.Receive() {
		t.Fatalf("Pod didn't finish deleting")
	}
	if s.userHasRemainingPods(u) {
		t.Fatalf("userHasRemainingPods should be false at this point")
	}
	t.Logf("Last pod and user storage were cleaned successfully")

	t.Logf("Attempting to call deletePod for a pod that doesn't exist")
	deleteRequest = DeletePodRequest{
		UserID: testUser,
		PodName: "foobar-pod",
		RemoteIP: remoteIP,
	}
	finished = util.NewReadyChannel(s.Client.TimeoutDelete)
	err = s.deletePod(deleteRequest, finished)
	if err == nil {
		t.Fatalf("No error when calling deletePod on a pod that doesn't exist")
	}
	if finished.Receive() {
		t.Fatalf("Finished channel received true after deletePod should have failed")
	}
}
