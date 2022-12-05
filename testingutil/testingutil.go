package testingutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/deic.dk/user_pods_k8s_backend/util"
)

type SupplementaryPodInfo struct {
	NeedsSsh     bool
	NeedsIngress bool
}

type CreatePodRequest struct {
	UserID        string                       `json:"user_id"`
	YamlURL       string                       `json:"yaml_url"`
	Settings      map[string]map[string]string `json:"settings"`
	Supplementary SupplementaryPodInfo
}

type CreatePodResponse struct {
	PodName string `json:"pod_name"`
}

type watchCreatePodRequest struct {
	PodName string `json:"pod_name"`
	UserID  string `json:"user_id"`
}

type watchCreatePodResponse struct {
	Ready bool `json:"ready"`
}

type deleteAllUserPodsRequest struct {
	UserID string `json:"user_id"`
}

type deleteAllUserPodsResponse struct {
	Deleted bool `json:"deleted"`
}

type deletePodRequest struct {
	UserID  string `json:"user_id"`
	PodName string `json:"pod_name"`
}

type deletePodResponse struct {
	Requested bool `json:"requested"`
}

type getPodNamesRequest struct {
	UserID   string `json:"user_id"`
	RemoteIP string
}

type reducedPodInfo struct {
	PodName string `json:"pod_name"`
}

type getPodNamesResponse []reducedPodInfo

func CreatePod(request CreatePodRequest) (string, error) {
	// Construct the request
	requestBody, err := json.Marshal(&request)
	if err != nil {
		return "", err
	}

	// Send the request
	response, err := http.Post("http://localhost/create_pod", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	// Decode the body
	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	var unmarshalled CreatePodResponse
	err = json.Unmarshal(responseBody, &unmarshalled)
	if err != nil {
		return "", err
	}

	// Return the result
	podName := unmarshalled.PodName
	if len(podName) == 0 {
		return "", errors.New("CreatePod request failed")
	}

	return podName, nil
}

func WatchCreatePod(userID string, podName string, finished *util.ReadyChannel) error {
	requestBody, err := json.Marshal(&watchCreatePodRequest{
		UserID:  userID,
		PodName: podName,
	})
	if err != nil {
		return err
	}

	response, err := http.Post("http://localhost/watch_create_pod", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}
	var unmarshalled watchCreatePodResponse
	err = json.Unmarshal(responseBody, &unmarshalled)
	if err != nil {
		return err
	}
	finished.Send(unmarshalled.Ready)

	return nil
}

func DeleteAllUserPods(userID string) error {
	// Construct the request
	requestBody, err := json.Marshal(&deleteAllUserPodsRequest{UserID: userID})
	if err != nil {
		return err
	}

	// Send the request
	response, err := http.Post("http://localhost/delete_all_user", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// Decode the body
	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}
	var unmarshalled deleteAllUserPodsResponse
	err = json.Unmarshal(responseBody, &unmarshalled)
	if err != nil {
		return err
	}

	if !unmarshalled.Deleted {
		return errors.New("deleteAllUserPods didn't complete successfully")
	}

	return nil
}

func DeletePod(userID string, podName string) (bool, error) {
	// Construct the request
	request := deletePodRequest{
		UserID:  userID,
		PodName: podName,
	}
	requestBody, err := json.Marshal(&request)
	if err != nil {
		return false, err
	}

	// Send the request
	response, err := http.Post("http://localhost/delete_pod", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	// Check status code
	if response.StatusCode != http.StatusOK {
		return false, errors.New(fmt.Sprintf("Got error code %s", response.Status))
	}
	// Decode the body
	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return false, err
	}
	var unmarshalled deletePodResponse
	err = json.Unmarshal(responseBody, &unmarshalled)
	if err != nil {
		return false, err
	}

	// Return the result

	return unmarshalled.Requested, nil
}

// Get a map of all standard pod types to their CreatePodRequests with default params
func GetStandardPodRequests() map[string]CreatePodRequest {
	config := util.MustLoadGlobalConfig()
	response := make(map[string]CreatePodRequest)
	response["jupyter"] = CreatePodRequest{
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		UserID:  config.TestUser,
		Settings: map[string]map[string]string{
			"jupyter": {"FILE": "", "WORKING_DIRECTORY": "jupyter"},
		},
		Supplementary: SupplementaryPodInfo{NeedsSsh: false, NeedsIngress: true},
	}
	response["ubuntu"] = CreatePodRequest{
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/ubuntu_sciencedata.yaml",
		UserID:  config.TestUser,
		Settings: map[string]map[string]string{
			"ubuntu-jammy": {"SSH_PUBLIC_KEY": config.TestSshKey},
		},
		Supplementary: SupplementaryPodInfo{NeedsSsh: true, NeedsIngress: false},
	}
	return response
}

// Get a map of all pod types useful for testing to their CreatePodRequests
func GetTestingPodRequests() map[string]CreatePodRequest {
	config := util.MustLoadGlobalConfig()
	response := make(map[string]CreatePodRequest)
	response["test_long_key"] = CreatePodRequest{
		YamlURL:       "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/test_long_key.yaml",
		UserID:        config.TestUser,
		Supplementary: SupplementaryPodInfo{NeedsSsh: false, NeedsIngress: false},
	}
	response["http_hello_world"] = CreatePodRequest{
		YamlURL:       "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/http_hello_world.yaml",
		UserID:        config.TestUser,
		Supplementary: SupplementaryPodInfo{NeedsSsh: false, NeedsIngress: true},
	}
	return response
}

// Get a list of podNames owned by the user from a get_pods request
func GetPodNames(userID string) ([]string, error) {
	var podNames []string
	request := getPodNamesRequest{UserID: userID}
	// Construct the request
	requestBody, err := json.Marshal(&request)
	if err != nil {
		return podNames, err
	}

	// Send the request
	response, err := http.Post("http://localhost/get_pods", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return podNames, err
	}
	defer response.Body.Close()
	// Check status code
	if response.StatusCode != http.StatusOK {
		return podNames, errors.New(fmt.Sprintf("Got error code %s", response.Status))
	}
	// Decode the body
	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return podNames, err
	}
	var unmarshalled getPodNamesResponse
	err = json.Unmarshal(responseBody, &unmarshalled)
	if err != nil {
		return podNames, err
	}

	// Return the result
	for _, value := range unmarshalled {
		podNames = append(podNames, value.PodName)
	}

	return podNames, nil
}

func EnsureUserHasNPods(userID string, n int) error {
	config := util.MustLoadGlobalConfig()
	userPodList, err := GetPodNames(userID)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't get_pods %s", err.Error()))
	}
	startingNumOfPods := len(userPodList)
	defaultRequests := GetStandardPodRequests()
	var podTypes []string
	for key, _ := range defaultRequests {
		podTypes = append(podTypes, key)
	}

	var readyChannels []*util.ReadyChannel
	// As long as the user has too few pods, create one of the standard ones
	for i := startingNumOfPods; i < n; i++ {
		// Cycle through each of the podTypes in the defaultRequests
		podType := podTypes[(i-startingNumOfPods)%len(podTypes)]
		podName, err := CreatePod(defaultRequests[podType])
		if err != nil {
			return errors.New(fmt.Sprintf("Failed while creating %s pod: %s", podType, err.Error()))
		}
		ready := util.NewReadyChannel(config.TimeoutCreate)
		go WatchCreatePod(userID, podName, ready)
		readyChannels = append(readyChannels, ready)
	}
	// Wait for all readyChannels to receive true
	if !util.ReceiveReadyChannels(readyChannels) {
		return errors.New("Not all pods reached ready state")
	}

	// Double check that the right number of pods exists now
	userPodList, err = GetPodNames(userID)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't get_pods %s", err.Error()))
	}
	if len(userPodList) < n {
		return errors.New(fmt.Sprintf("User should have %d pods now, but only %d exist.", n, len(userPodList)))
	}
	return nil
}

func EnsureUserHasEach(userID string, requests map[string]CreatePodRequest) error {
	config := util.MustLoadGlobalConfig()
	userPodList, err := GetPodNames(userID)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't get_pods %s", err.Error()))
	}

	var readyChannels []*util.ReadyChannel
	// For each of the requests, check that one exists and create it if not
	for podType, request := range requests {
		hasPod := false
		// Look through the user's PodList to see if one exists already
		for _, existingPodName := range userPodList {
			if strings.Contains(existingPodName, podType) {
				hasPod = true
				break
			}
		}
		// If the user doesn't have it already, then create it
		if !hasPod {
			podName, err := CreatePod(request)
			if err != nil {
				return errors.New(fmt.Sprintf("Failed while creating %s pod: %s", podType, err.Error()))
			}
			ready := util.NewReadyChannel(config.TimeoutCreate)
			go WatchCreatePod(userID, podName, ready)
			readyChannels = append(readyChannels, ready)
		}
	}
	// Wait for all readyChannels to receive true
	if !util.ReceiveReadyChannels(readyChannels) {
		return errors.New("Not all pods reached ready state")
	}
	return nil
}
