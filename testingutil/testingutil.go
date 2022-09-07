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
)

type createPodRequest struct {
	UserID   string                       `json:"user_id"`
	YamlURL  string                       `json:"yaml_url"`
	Settings map[string]map[string]string `json:"settings"`
}

type createPodResponse struct {
	PodName string `json:"pod_name"`
}

type watchCreatePodRequest struct {
	PodName string `json:"pod_name"`
	UserID  string `json:"user_id"`
}

type watchCreatePodResponse struct {
	Ready bool `json:"ready"`
}

func CreatePod(userID string, yamlURL string, settings map[string]map[string]string) (string, error) {
	// Construct the request
	requestBody, err := json.Marshal(&createPodRequest{
		UserID:   userID,
		YamlURL:  yamlURL,
		Settings: settings,
	})
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
	var unmarshalled createPodResponse
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
