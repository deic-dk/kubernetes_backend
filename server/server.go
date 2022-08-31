package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/podcreator"
	"github.com/deic.dk/user_pods_k8s_backend/poddeleter"
	"github.com/deic.dk/user_pods_k8s_backend/util"
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

type WatchCreatePodRequest struct {
	PodName string `json:"pod_name"`
	UserID  string `json:"user_id"`
}

type WatchCreatePodResponse struct {
	Ready bool `json:"ready"`
}

type DeletePodRequest struct {
	UserID   string `json:"user_id"`
	PodName  string `json:"pod_name"`
	RemoteIP string
}

type DeletePodResponse struct {
	Requested bool `json:"requested"`
}

type WatchDeletePodRequest struct {
	PodName string `json:"pod_name"`
	UserID  string `json:"user_id"`
}

type WatchDeletePodResponse struct {
	Deleted bool `json:"deleted"`
}

type DeleteAllPodsRequest struct {
	UserID   string `json:"user_id"`
	RemoteIP string
}

type DeleteAllPodsResponse struct {
	Requested bool `json:"requested"`
}

type Server struct {
	Client          k8sclient.K8sClient
	CreatingPods    map[string]*util.ReadyChannel
	DeletingPods    map[string]*util.ReadyChannel
	DeletingStorage map[string]*util.ReadyChannel
	mutex           *sync.Mutex
}

type serverMapName int

const (
	CreatingPods    serverMapName = 0
	DeletingPods    serverMapName = 1
	DeletingStorage serverMapName = 2
)

func New(client k8sclient.K8sClient) *Server {
	var m sync.Mutex
	return &Server{
		Client:          client,
		CreatingPods:    make(map[string]*util.ReadyChannel),
		DeletingPods:    make(map[string]*util.ReadyChannel),
		DeletingStorage: make(map[string]*util.ReadyChannel),
		mutex:           &m,
	}
}

// Add an entry to s.CreatingPods or s.DeletingPods with `podName` as the key and `finished` as the value.
// As soon as a value is ready in finished, the entry will be removed from the map.
// `creating` should be true if adding to s.CreatingPods and false if adding to s.DeletingPods.
func (s *Server) AddToWatchMaps(key string, finished *util.ReadyChannel, mapName serverMapName) {
	// Thread-safe add podName to the map of creating pods
	s.mutex.Lock()
	defer s.mutex.Unlock()
	switch mapName {
	case CreatingPods:
		s.CreatingPods[key] = finished
	case DeletingPods:
		s.DeletingPods[key] = finished
	case DeletingStorage:
		s.DeletingStorage[key] = finished
	}

	// Then watch for the finished signal, and once finished, remove podName from the map
	go func() {
		finished.Receive()
		s.mutex.Lock()
		defer s.mutex.Unlock()
		switch mapName {
		case CreatingPods:
			delete(s.CreatingPods, key)
		case DeletingPods:
			delete(s.DeletingPods, key)
		case DeletingStorage:
			delete(s.DeletingStorage, key)
		}
	}()
}

// Fills in a getPodsResponse with information about all the pods owned by the user.
// If the username string is empty, use all pods in the namespace.
func (s *Server) getPods(request GetPodsRequest) (GetPodsResponse, error) {
	var response GetPodsResponse
	user := managed.NewUser(request.UserID, s.Client)
	podList, err := user.ListPods()
	if err != nil {
		return response, err
	}
	for _, pod := range podList {
		podInfo := pod.GetPodInfo()
		// If this podName is in the server's map of creating/deleting pods,
		// then overwrite the podInfo.Status
		_, podCreating := s.CreatingPods[podInfo.PodName]
		if podCreating {
			podInfo.Status = "Creating"
		} else {
			_, podDeleting := s.DeletingPods[podInfo.PodName]
			if podDeleting {
				podInfo.Status = "Deleting"
			}
		}
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
	request.RemoteIP = util.GetRemoteIP(r)
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
func (s *Server) createPod(request CreatePodRequest, finished *util.ReadyChannel) (CreatePodResponse, error) {
	var response CreatePodResponse
	// make podCreator
	creator, err := podcreator.NewPodCreator(
		request.YamlURL,
		managed.NewUser(request.UserID, s.Client),
		request.RemoteIP,
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
	s.AddToWatchMaps(pod.Object.Name, finished, CreatingPods)
	response.PodName = pod.Object.Name
	return response, nil
}

// Handles the http request to create a pod for the user
func (s *Server) ServeCreatePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request CreatePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = util.GetRemoteIP(r)
	fmt.Printf("createPod request: %+v\n", request)

	finished := util.NewReadyChannel(s.Client.TimeoutCreate)
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
		if finished.Receive() {
			fmt.Printf("Completed start jobs for Pod %s\n", response.PodName)
		} else {
			fmt.Printf("Warning: failed to create pod %s or complete start jobs\n", response.PodName)
			// TODO attempt to clean up the failed pod
		}
	}()
}

func (s *Server) watchCreatePod(request WatchCreatePodRequest) (WatchCreatePodResponse, error) {
	response := WatchCreatePodResponse{Ready: false}
	readyChannel, exists := s.CreatingPods[request.PodName]
	if exists {
		// If the readyChannel receives `false`, then go ahead and return false
		if !readyChannel.Receive() {
			return response, nil
		}
	}
	u := managed.NewUser(request.UserID, s.Client)
	// Now the readyChannel either received true or wasn't stored in `s.CreatingPods`
	// Return true iff the pod exists and is owned by the user
	owned, err := u.OwnsPod(request.PodName)
	if err != nil {
		return response, err
	}
	response.Ready = owned
	return response, nil
}

func (s *Server) ServeWatchCreatePod(w http.ResponseWriter, r *http.Request) {
	var request WatchCreatePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	fmt.Printf("watchCreatePod request %+v\n", request)

	response, err := s.watchCreatePod(request)
	status := http.StatusOK
	if err != nil {
		fmt.Printf("Error watching pod: %s\n", err.Error())
		status = http.StatusBadRequest
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

func (s *Server) userHasRemainingPods(u managed.User) bool {
	podList, err := u.ListPods()
	if err != nil {
		fmt.Printf("Error, couldn't list pods for user %s: %s\n", u.UserID, err.Error())
		return false
	}
	// For each of the user's pods,
	for _, pod := range podList {
		// check whether it's being deleted.
		_, inDeletingMap := s.DeletingPods[pod.Object.Name]
		// If there is a pod that is not being deleted, then go ahead and return true
		if !inDeletingMap {
			return true
		}
	}
	return false
}

func (s *Server) deletePod(request DeletePodRequest, finished *util.ReadyChannel) (DeletePodResponse, error) {
	response := DeletePodResponse{Requested: false}
	_, podIsBeingDeleted := s.DeletingPods[request.PodName]
	if podIsBeingDeleted {
		finished.Send(false)
		return response, errors.New(fmt.Sprintf("pod %s is already being deleted", request.PodName))
	}

	// Try to initialize a podDeleter (this will check that the username matches)
	deleter, err := poddeleter.NewPodDeleter(request.PodName, request.UserID, s.Client)
	if err != nil {
		finished.Send(false)
		return response, errors.New(fmt.Sprintf("Error starting pod deletion for %s: %s", request.PodName, err.Error()))
	}
	// Attempt to call for deletion
	err = deleter.DeletePod(finished)
	if err != nil {
		finished.Send(false)
		return response, err
	}
	// If that was successful, the server should keep track that this pod is deleting
	s.AddToWatchMaps(request.PodName, finished, DeletingPods)

	// Then if the user doesn't have remaining pods, call for deletion of their storage,
	// If this fails, log the error, but don't tell the user, because at this point their pod will be deleted.
	if !s.userHasRemainingPods(deleter.Pod.Owner) {
		cleanedStorage := util.NewReadyChannel(s.Client.TimeoutDelete)
		err = deleter.Pod.Owner.CleanUserStorage(cleanedStorage)
		if err != nil {
			fmt.Printf("Error: Couldn't call for deletion of user storage for %s: %s\n", deleter.Pod.Owner.UserID, err.Error())
		}
		s.AddToWatchMaps(deleter.Pod.Owner.Name, cleanedStorage, DeletingStorage)
	}

	response.Requested = true
	return response, nil
}

func (s *Server) ServeDeletePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request DeletePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = util.GetRemoteIP(r)
	fmt.Printf("deletePod request: %+v\n", request)

	finished := util.NewReadyChannel(s.Client.TimeoutDelete)
	response, err := s.deletePod(request, finished)
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
}

func (s *Server) watchDeletePod(request WatchDeletePodRequest) bool {
	readyChannel, exists := s.DeletingPods[request.PodName]
	if !exists {
		return true
	}
	return readyChannel.Receive()
}

func (s *Server) ServeWatchDeletePod(w http.ResponseWriter, r *http.Request) {
	var request WatchDeletePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	fmt.Printf("watchDeletePod request %+v\n", request)

	response := WatchDeletePodResponse{}
	response.Deleted = s.watchDeletePod(request)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func (s *Server) deleteAllUserPods(userID string, finished *util.ReadyChannel) error {
	user := managed.NewUser(userID, s.Client)
	// Get a list of managed.Pod objects for all of the user's pods
	podList, err := user.ListPods()
	if err != nil {
		return err
	}

	var chanList []*util.ReadyChannel
	// For each pod,
	for _, pod := range podList {
		// Check that it isn't already being deleted
		_, deleting := s.DeletingPods[pod.Object.Name]
		if deleting {
			continue
		}

		// Then initialize a deleter and call for the pod's deletion
		deleter := poddeleter.NewFromPod(pod)
		ch := util.NewReadyChannel(s.Client.TimeoutDelete)
		err := deleter.DeletePod(ch)
		// If something went wrong, log it
		if err != nil {
			fmt.Printf("Error calling deletion of pod %s: %s\n", pod.Object.Name, err.Error())
			continue
		}
		chanList = append(chanList, ch)
		// If the delete call was made successfully, then add the pod to `s.DeletingPods`,
		s.AddToWatchMaps(pod.Object.Name, ch, DeletingPods)
	}

	// Finally, remove the user's storage PV and PVC
	cleanedStorage := util.NewReadyChannel(s.Client.TimeoutDelete)
	err = user.CleanUserStorage(cleanedStorage)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't call for deletion of user storage for %s: %s", userID, err.Error()))
	}
	s.AddToWatchMaps(user.Name, cleanedStorage, DeletingStorage)
	chanList = append(chanList, cleanedStorage)
	go util.CombineReadyChannels(chanList, finished)
	return nil
}

func (s *Server) ServeDeleteAllUserPods(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request DeleteAllPodsRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = util.GetRemoteIP(r)
	fmt.Printf("deleteAllUserPods request: %+v\n", request)

	finished := util.NewReadyChannel(s.Client.TimeoutDelete)
	err := s.deleteAllUserPods(request.UserID, finished)
	var status int
	var response DeleteAllPodsResponse
	if err != nil {
		status = http.StatusBadRequest
		response.Requested = false
		fmt.Printf("Error: %s\n", err.Error())
	} else {
		status = http.StatusOK
		response.Requested = true
	}

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}
