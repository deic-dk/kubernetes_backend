package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/podcreator"
)

type GetPodsRequest struct {
	UserID   string `json:"user_id"`
	RemoteIP string
}

type GetPodsResponse []managed.PodInfo

type CreatePodRequest struct {
	YamlURL string `json:"yaml_url"`
	UserID  string `json:"user_id"`
	//Settings[container_name][env_var_name] = env_var_value
	ContainerEnvVars map[string]map[string]string `json:"settings"`
	AllEnvVars       map[string]string
	RemoteIP         string
}

type CreatePodResponse struct {
	PodName string `json:"pod_name"`
}

type DeletePodRequest struct {
	UserID   string `json:"user_id"`
	PodName  string `json:"pod_name"`
	RemoteIP string
}

type DeletePodResponse struct {
	PodName string `json:"pod_name"`
}

type DeleteAllPodsRequest struct {
	UserID   string `json:"user_id"`
	RemoteIP string
}

type DeleteAllPodsResponse struct {
	PodNames []string `json:"pod_names"`
}

type Server struct {
	Client       k8sclient.K8sClient
	CreatingPods map[string]struct{}
	DeletingPods map[string]struct{}
}

func New(client k8sclient.K8sClient) *Server {
	return &Server{
		Client:       client,
		CreatingPods: make(map[string]struct{}),
		DeletingPods: make(map[string]struct{}),
	}
}

// Fills in a getPodsResponse with information about all the pods owned by the user.
// If the username string is empty, use all pods in the namespace.
func (s *Server) getPods(request GetPodsRequest) (GetPodsResponse, error) {
	var response GetPodsResponse
	user := managed.NewUser(request.UserID, request.RemoteIP, s.Client)
	podList, err := user.ListPods()
	if err != nil {
		return response, err
	}
	for _, pod := range podList {
		podInfo := pod.GetPodInfo()
		response = append(response, podInfo)
	}
	return response, nil
}

// Handles the http request to get info about the user's pods
func (s *Server) ServeGetPods(w http.ResponseWriter, r *http.Request) {
	// parse the request
	var request GetPodsRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = getRemoteIP(r)
	fmt.Printf("getPods request: %+v\n", request)

	// get the list of pods
	response, err := s.getPods(request)
	var status int
	if err != nil {
		status = http.StatusBadRequest
	} else {
		status = http.StatusOK
	}

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

// Makes a PodCreator to request that kubernetes create the pod.
// Returns the pod's name without error if the request was made without error,
// Then quietly waits for the pod to reach Ready state and runs start jobs.
func (s *Server) createPod(request CreatePodRequest, finished chan<- bool) (CreatePodResponse, error) {
	var response CreatePodResponse
	// make podCreator
	creator, err := podcreator.NewPodCreator(
		request.YamlURL,
		managed.NewUser(request.UserID, request.RemoteIP, s.Client),
		request.ContainerEnvVars,
		s.Client,
	)
	if err != nil {
		return response, err
	}

	// create pod
	pod, err := creator.CreatePod(finished)
	if err != nil {
		return response, err
	}
	response.PodName = pod.Object.Name
	return response, nil
}

// Handles the http request to create a pod for the user
func (s *Server) ServeCreatePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request CreatePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = getRemoteIP(r)
	fmt.Printf("createPod request: %+v\n", request)

	finished := make(chan bool, 1)
	response, err := s.createPod(request, finished)

	var status int
	if err != nil {
		status = http.StatusBadRequest
		fmt.Printf("Error: %s\n", err.Error())
	} else {
		status = http.StatusOK
	}

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)

	go func() {
		success := <-finished
		fmt.Printf("Pod %s reached ready %+v\n", response.PodName, success)
		close(finished)
	}()
}

// Server utility functions

// Gets the IP of the source that made the request, either r.RemoteAddr,
// or if it was forwarded, the first address in the X-Forwarded-For header
func getRemoteIP(r *http.Request) string {
	// When running this behind caddy, r.RemoteAddr is just the caddy process's IP addr,
	// and X-Forward-For header should contain the silo's IP address.
	// This may be different with ingress.
	var siloIP string
	value, forwarded := r.Header["X-Forwarded-For"]
	if forwarded {
		siloIP = value[0]
	} else {
		siloIP = r.RemoteAddr
	}
	return regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`).FindString(siloIP)
}
