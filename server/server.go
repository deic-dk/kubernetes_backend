package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/podcreator"
	"github.com/deic.dk/user_pods_k8s_backend/poddeleter"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	Deleted bool `json:"deleted"`
}

type watchMapEntry struct {
	authCheck    string
	readyChannel *util.ReadyChannel
}

type Server struct {
	Client          k8sclient.K8sClient
	GlobalConfig    util.GlobalConfig
	CreatingPods    map[string]watchMapEntry
	DeletingPods    map[string]watchMapEntry
	DeletingStorage map[string]watchMapEntry
	mutex           *sync.Mutex
}

type watchMapName int

const (
	CreatingPods    watchMapName = 0
	DeletingPods    watchMapName = 1
	DeletingStorage watchMapName = 2
)

func New(client k8sclient.K8sClient, globalConfig util.GlobalConfig) *Server {
	var m sync.Mutex
	return &Server{
		Client:          client,
		GlobalConfig:    globalConfig,
		CreatingPods:    make(map[string]watchMapEntry),
		DeletingPods:    make(map[string]watchMapEntry),
		DeletingStorage: make(map[string]watchMapEntry),
		mutex:           &m,
	}
}

// Add an entry to the specified watchMap (e.g. `s.CreatingPods`) for the given key.
// As soon as a value is ready in `entry.readyChannel`, the entry will be removed from the map.
func (s *Server) addToWatchMaps(key string, entry watchMapEntry, mapName watchMapName) {
	// Thread-safe add `key` to the map of events to wait for
	s.mutex.Lock()
	defer s.mutex.Unlock()
	switch mapName {
	case CreatingPods:
		s.CreatingPods[key] = entry
	case DeletingPods:
		s.DeletingPods[key] = entry
	case DeletingStorage:
		s.DeletingStorage[key] = entry
	}

	// Then watch for the finished signal, and once finished, remove `key` from the map
	go func() {
		entry.readyChannel.Receive()
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

// Gets the IP of the source that made the request, either r.RemoteAddr,
// or if it was forwarded, the first address in the X-Forwarded-For header
func (s *Server) getRemoteIP(r *http.Request) string {
	// When running this behind a manual reverse proxy, r.RemoteAddr is just the proxy's IP addr,
	// and X-Forward-For header should contain the silo's IP address.
	// This may be different with ingress.
	var remoteAddr string
	value, forwarded := r.Header["X-Forwarded-For"]
	if forwarded {
		remoteAddr = value[0]
	} else {
		remoteAddr = r.RemoteAddr
	}

	// Regex to get the IP address without port out of `r.RemoteAddr`
	// First check whether it's a valid v4 address
	v4regex := regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`)
	remoteIP := v4regex.FindString(remoteAddr)
	v4 := true
	if len(remoteIP) == 0 {
		v6regex := regexp.MustCompile(`([a-fA-F0-9]{1,4}:|:)+:[a-fA-F0-9]{1,4}`)
		remoteIP = v6regex.FindString(remoteAddr)
		v4 = false
		// If it's still empty, then return
		if len(remoteIP) == 0 {
			return remoteIP
		}
	}
	// Check whether the address is loopback.
	// If the request is from loopback, it is a test
	// and needs to be rewritten as though it came from a host where nfs shares are available
	if v4 {
		if strings.Contains(remoteIP, "127.0.0.1") {
			return s.GlobalConfig.TestingHost
		}
	} else {
		if strings.Contains(remoteIP, "::1") {
			return s.GlobalConfig.TestingHost
		}
	}

	// If it wasn't a loopback address, return the actual remoteIP
	return remoteIP
}

// Fills in a getPodsResponse with information about all the pods owned by the user.
// If the username string is empty, use all pods in the namespace.
func (s *Server) getPods(request GetPodsRequest) (GetPodsResponse, error) {
	var response GetPodsResponse
	user := managed.NewUser(request.UserID, s.Client, s.GlobalConfig)
	podList, err := user.ListPods()
	if err != nil {
		return response, err
	}
	for _, pod := range podList {
		podInfo := pod.GetPodInfo()
		// If this podName is in the server's map of creating/deleting pods,
		// then overwrite the podInfo.Status
		s.mutex.Lock()
		_, podCreating := s.CreatingPods[podInfo.PodName]
		if podCreating {
			podInfo.Status = "Creating"
		} else {
			_, podDeleting := s.DeletingPods[podInfo.PodName]
			if podDeleting {
				podInfo.Status = "Deleting"
			}
		}
		s.mutex.Unlock()
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
	request.RemoteIP = s.getRemoteIP(r)
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
		request.UserID,
		request.RemoteIP,
		request.ContainerEnvVars,
		s.Client,
		s.GlobalConfig,
	)
	if err != nil {
		return response, err
	}

	// create pod
	pod, err := creator.CreatePod(finished)
	if err != nil {
		return response, err
	}
	// If creation was requested successfully, add the readyChannel to the server's watchMap
	s.addToWatchMaps(
		pod.Object.Name,
		watchMapEntry{readyChannel: finished, authCheck: request.UserID},
		CreatingPods,
	)

	// If the readyChannel gets a `false`, then call for pod deletion
	go func() {
		if !finished.Receive() {
			s.deletePodIfFailedCreate(pod.Object.Name, request)
		}
	}()

	// Return the response
	response.PodName = pod.Object.Name
	return response, nil
}

// Handles the http request to create a pod for the user
func (s *Server) ServeCreatePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request CreatePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = s.getRemoteIP(r)
	fmt.Printf("createPod request: %+v\n", request)

	finished := util.NewReadyChannel(s.GlobalConfig.TimeoutCreate)
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
	// Thread-safe read in case a channel is added/removed concurrently
	s.mutex.Lock()
	entry, exists := s.CreatingPods[request.PodName]
	s.mutex.Unlock()
	if exists {
		// Check that the userID matches the pod
		if entry.authCheck != request.UserID {
			return response, errors.New(
				fmt.Sprintf("Requested userID %s does not match pod's owner %s", request.UserID, entry.authCheck),
			)
		}
		// Respond when there is a value in the ready channel
		response.Ready = entry.readyChannel.Receive()
		return response, nil
	}

	// If there was no entry for this pod in `s.CreatingPods`, return true iff the pod exists and is owned by the user.
	u := managed.NewUser(request.UserID, s.Client, s.GlobalConfig)
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
	// If there is an error, it may be internal, or it may be a user requesting for a pod they don't own.
	// To avoid giving the user information about pods they don't own, return `false` without error in either case.
	if err != nil {
		fmt.Printf("Error watching pod: %s\n", err.Error())
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func (s *Server) userHasRemainingPods(u managed.User) bool {
	podList, err := u.ListPods()
	if err != nil {
		fmt.Printf("Error, couldn't list pods for user %s: %s\n", u.UserID, err.Error())
		return false
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
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

func (s *Server) deletePodIfFailedCreate(podName string, createRequest CreatePodRequest) error {
	// Construct a deletePodRequest
	request := DeletePodRequest{
		PodName:  podName,
		UserID:   createRequest.UserID,
		RemoteIP: createRequest.RemoteIP,
	}
	fmt.Printf("Attempting to delete pod %s because it didn't reach desired state", podName)

	// Call for deletion
	finished := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
	_, err := s.deletePod(request, finished)
	if err != nil {
		fmt.Printf("Error: Failed deleting pod %s after it failed creation: %s\n", podName, err.Error())
		return err
	}

	// Wait and see whether it succeeded
	if !finished.Receive() {
		errorMessage := fmt.Sprintf("Error: Pod %s failed to reach deleted state. Deletion was triggered by failure to reach created state.\n", podName)
		fmt.Print(errorMessage)
		return errors.New(errorMessage)
	}
	return nil
}

func (s *Server) deletePod(request DeletePodRequest, finished *util.ReadyChannel) (DeletePodResponse, error) {
	response := DeletePodResponse{Requested: false}
	s.mutex.Lock()
	_, podIsBeingDeleted := s.DeletingPods[request.PodName]
	s.mutex.Unlock()
	if podIsBeingDeleted {
		finished.Send(false)
		return response, errors.New(fmt.Sprintf("pod %s is already being deleted", request.PodName))
	}

	// Try to initialize a podDeleter (this will check that the username matches)
	deleter, err := poddeleter.NewPodDeleter(request.PodName, request.UserID, s.Client, s.GlobalConfig)
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
	s.addToWatchMaps(
		request.PodName,
		watchMapEntry{readyChannel: finished, authCheck: request.UserID},
		DeletingPods)

	// Then if the user doesn't have remaining pods, call for deletion of their storage,
	// If this fails, log the error, but don't tell the user, because at this point their pod will be deleted.
	if !s.userHasRemainingPods(deleter.Pod.Owner) {
		// Check whether the user's storage is already being deleted
		s.mutex.Lock()
		_, cleaningStorage := s.DeletingStorage[deleter.Pod.Owner.Name]
		s.mutex.Unlock()
		// If it's not already being deleted, then call for deletion
		if !cleaningStorage {
			cleanedStorage := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
			err = deleter.Pod.Owner.DeleteUserStorage(cleanedStorage)
			if err != nil {
				fmt.Printf("Error: Couldn't call for deletion of user storage for %s: %s\n", deleter.Pod.Owner.UserID, err.Error())
			} else {
				s.addToWatchMaps(
					deleter.Pod.Owner.Name,
					watchMapEntry{readyChannel: cleanedStorage},
					DeletingStorage)
			}
		}
	}

	response.Requested = true
	return response, nil
}

func (s *Server) ServeDeletePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request DeletePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = s.getRemoteIP(r)
	fmt.Printf("deletePod request: %+v\n", request)

	finished := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
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

// Watch for the deletion of the pod with name `request.PodName`,
// Return with `response.Deleted` false iff:
// (the ready channel retuns false, or (there is no s.DeletingPods entry and the user owns a pod with that podName))
func (s *Server) watchDeletePod(request WatchDeletePodRequest) (WatchDeletePodResponse, error) {
	// Default true, so that if there is no entry in `s.DeletingPods`, there's no difference between
	// the pod not existing and the pod existing with a different owner than `request.UserID`
	response := WatchDeletePodResponse{Deleted: true}
	// Thread-safe read in case a channel is added/removed concurrently
	s.mutex.Lock()
	entry, exists := s.DeletingPods[request.PodName]
	s.mutex.Unlock()
	if exists {
		if entry.authCheck != request.UserID {
			return response, errors.New(
				fmt.Sprintf("Requested userID %s does not match pod's owner %s", request.UserID, entry.authCheck),
			)
		}
		response.Deleted = entry.readyChannel.Receive()
		return response, nil
	}

	u := managed.NewUser(request.UserID, s.Client, s.GlobalConfig)
	owned, err := u.OwnsPod(request.PodName)
	if err != nil {
		return response, err
	}
	response.Deleted = !owned
	return response, nil

}

func (s *Server) ServeWatchDeletePod(w http.ResponseWriter, r *http.Request) {
	var request WatchDeletePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	fmt.Printf("watchDeletePod request %+v\n", request)

	response, err := s.watchDeletePod(request)
	if err != nil {
		fmt.Printf("Error while watching for pod deletion: %s\n", err.Error())
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func (s *Server) deleteAllUserPods(userID string, finished *util.ReadyChannel) error {
	user := managed.NewUser(userID, s.Client, s.GlobalConfig)
	// Get a list of managed.Pod objects for all of the user's pods
	podList, err := user.ListPods()
	if err != nil {
		return err
	}

	var chanList []*util.ReadyChannel
	// For each pod,
	for _, pod := range podList {
		// Check that it isn't already being deleted
		s.mutex.Lock()
		_, deleting := s.DeletingPods[pod.Object.Name]
		s.mutex.Unlock()
		if deleting {
			continue
		}

		// Then initialize a deleter and call for the pod's deletion
		deleter := poddeleter.NewFromPod(pod)
		ch := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
		err := deleter.DeletePod(ch)
		// If something went wrong, log it
		if err != nil {
			fmt.Printf("Error calling deletion of pod %s: %s\n", pod.Object.Name, err.Error())
			continue
		}
		chanList = append(chanList, ch)
		// If the delete call was made successfully, then add the pod to `s.DeletingPods`,
		s.addToWatchMaps(
			pod.Object.Name,
			watchMapEntry{readyChannel: ch, authCheck: userID},
			DeletingPods)
	}

	// Finally, remove the user's storage PV and PVC
	cleanedStorage := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
	err = user.DeleteUserStorage(cleanedStorage)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't call for deletion of user storage for %s: %s", userID, err.Error()))
	}
	s.addToWatchMaps(
		user.Name,
		watchMapEntry{readyChannel: cleanedStorage},
		DeletingStorage)
	chanList = append(chanList, cleanedStorage)
	go util.CombineReadyChannels(chanList, finished)
	return nil
}

func (s *Server) ServeDeleteAllUserPods(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request DeleteAllPodsRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = s.getRemoteIP(r)
	fmt.Printf("deleteAllUserPods request: %+v\n", request)

	// give a long enough timout that it will accommodate slowly deleting PV/PVC in worst case
	finished := util.NewReadyChannel(2 * s.GlobalConfig.TimeoutDelete)
	err := s.deleteAllUserPods(request.UserID, finished)
	var status int
	var response DeleteAllPodsResponse
	if err != nil {
		status = http.StatusBadRequest
		response.Deleted = false
		fmt.Printf("Error: %s\n", err.Error())
	} else { // if the request was made without error
		// wait for the result, and if it succeeds, set an okay response
		if finished.Receive() {
			status = http.StatusOK
			response.Deleted = true
		} else { // if it was requested successfully but failed to delete everything,
			status = http.StatusOK
			response.Deleted = false
		}
	}

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

func (s *Server) cleanAllUnused(finished *util.ReadyChannel) error {
	var taskChannelList []*util.ReadyChannel

	// Clean orphaned services.
	// Find all the services that were created for a pod.
	serviceList, err := s.Client.ListServices(
		metav1.ListOptions{LabelSelector: "createdForPod"},
	)
	if err != nil {
		return err
	}
	// For all of the services that belong to a pod,
	for _, service := range serviceList.Items {
		podName, exists := service.Labels["createdForPod"]
		if !exists {
			return errors.New(fmt.Sprintf("Service %s didn't have createdForPod label", service.Name))
		}
		podList, err := s.Client.ListPods(
			metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", podName)},
		)
		if err != nil {
			return err
		}
		// If the pod that the service was created for no longer exists, then delete the service
		if len(podList.Items) == 0 {
			ch := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
			taskChannelList = append(taskChannelList, ch)
			// Make a watcher that will announce its deletion
			go func() {
				s.Client.WatchDeleteService(service.Name, ch)
				if ch.Receive() {
					fmt.Printf("Deleted SVC %s\n", service.Name)
				} else {
					fmt.Printf("Warning: failed to delete SVC %s\n", service.Name)
				}
			}()
			s.Client.DeleteService(service.Name)
		}
	}

	// Clean orphaned user storage.
	// Check for all PVCs (not PVs!) because they are namespaced
	pvcList, err := s.Client.ListPVC(metav1.ListOptions{})
	if err != nil {
		return err
	}
	// For all of the persistent volume claims in this namespace,
	for _, pvc := range pvcList.Items {
		// If the pvc is for user storage
		if strings.Contains(pvc.Name, "user-storage") {
			u := managed.NewUser(util.GetUserIDFromLabels(pvc.Labels), s.Client, s.GlobalConfig)
			userPodList, err := u.ListPods()
			if err != nil {
				return err
			}
			// If the user who owns this PVC doesn't have any pods, then delete the storage
			if len(userPodList) == 0 {
				ch := util.NewReadyChannel(s.GlobalConfig.TimeoutDelete)
				err := u.DeleteUserStorage(ch)
				if err != nil {
					return err
				}
				taskChannelList = append(taskChannelList, ch)
			}
		}
	}

	// Clean up pod caches
	// Get a list of every filename in tokenDir
	dir, err := os.Open(s.GlobalConfig.TokenDir)
	if err != nil {
		return err
	}
	fileNames, err := dir.Readdirnames(0)
	if err != nil {
		return err
	}
	// For each file in tokenDir, check if it belongs to a pod that doesn't exist
	for _, fileName := range fileNames {
		podList, err := s.Client.ListPods(
			metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", fileName)},
		)
		if err != nil {
			return err
		}
		// If there is no pod whose name matches this file, then it is an orphaned podcache
		if len(podList.Items) == 0 {
			err := os.Remove(fmt.Sprintf("%s/%s", s.GlobalConfig.TokenDir, fileName))
			if err != nil {
				return errors.New(fmt.Sprintf("Couldn't delete orphaned podcache %s: %s", fileName, err.Error()))
			}
		}
	}
	go util.CombineReadyChannels(taskChannelList, finished)

	return nil
}

func (s *Server) ServeCleanAllUnused(w http.ResponseWriter, r *http.Request) {
	remoteIP := s.getRemoteIP(r)
	fmt.Printf("Clean all request from IP %s", remoteIP)
	// Could limit this to a whitelisted IP range

	finished := util.NewReadyChannel(3 * s.GlobalConfig.TimeoutDelete)
	err := s.cleanAllUnused(finished)
	status := http.StatusOK
	if err != nil {
		fmt.Printf("Error during cleanAllUnused: %s\n", err.Error())
		status = http.StatusBadRequest
	} else {
		if !finished.Receive() {
			fmt.Printf("Warning: cleanAllUnused didn't finish successfully\n")
			status = http.StatusBadRequest
		}
	}

	// write the response
	w.WriteHeader(status)
}

func (s *Server) ReloadPodCaches() error {
	allPodList, err := s.Client.ListPods(metav1.ListOptions{})
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list pods: %s", err.Error()))
	}
	for _, podObject := range allPodList.Items {
		// If this is a pod without an owner, skip it
		userID := util.GetUserIDFromLabels(podObject.ObjectMeta.Labels)
		if userID == "" {
			continue
		}
		pod := managed.NewPod(&podObject, s.Client, s.GlobalConfig)
		err := pod.CreateAndSavePodCache(true)
		if err != nil {
			return errors.New(fmt.Sprintf("Failed to save podcache for pod %s: %s", podObject.Name, err.Error()))
		}
	}
	return nil
}
