package testingutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"

	"github.com/deic.dk/user_pods_k8s_backend/util"
)

const (
	TestSshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFFaL0dy3Dq4DA5GCqFBKVWZntBSF0RIeVd9/qdhIj2n joshua@myhost"
	TestUser   = "registeredtest7"
	RemoteIP   = "10.0.0.20"
	HomeServer = "10.2.0.20"
)

type CreatePodRequest struct {
	UserID   string                       `json:"user_id"`
	YamlURL  string                       `json:"yaml_url"`
	Settings map[string]map[string]string `json:"settings"`
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

// Get a map of all standard pod types to their CreatePodRequests with default params
func GetStandardPodRequests() map[string]CreatePodRequest {
	response := make(map[string]CreatePodRequest)
	response["jupyter"] = CreatePodRequest{
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		UserID:  TestUser,
		Settings: map[string]map[string]string{
			"jupyter": {"FILE": "", "WORKING_DIRECTORY": "jupyter"},
		},
	}
	response["ubuntu"] = CreatePodRequest{
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/ubuntu_sciencedata.yaml",
		UserID:  TestUser,
		Settings: map[string]map[string]string{
			"ubuntu-jammy": {"SSH_PUBLIC_KEY": TestSshKey},
		},
	}
	return response
}
