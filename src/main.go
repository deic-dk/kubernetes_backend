package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	watch "k8s.io/apimachinery/pkg/watch"
)

// TODO figure out how to get the namespace automatically from within the pod where this runs
const namespace = "sciencedata-dev"
const whitelistYamlURLRegex = "https:\\/\\/raw[.]githubusercontent[.]com\\/deic-dk\\/pod_manifests"
const sciencedataPrivateNet = "10.2."
const sciencedataInternalNet = "10.0."
const podReadyTimeout = 10 * time.Second

type GetPodsRequest struct {
	UserID string `json:"user_id"`
}

type GetPodsResponse struct {
	PodName        string
	ContainerName  string
	ImageName      string
	PodIP          string
	NodeIP         string
	Owner          string
	Age            string
	Status         string
	Ed25519Hostkey string
	RsaHostkey     string
	Url            string
	SshUrl         string
}

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

type clientsetHandler struct {
	clientset *kubernetes.Clientset
	podClient v1.PodInterface
	PVClient  v1.PersistentVolumeInterface
	PVCClient v1.PersistentVolumeClaimInterface
}

// Generate the structs with methods for interacting with the k8s api.
func getClientset() *kubernetes.Clientset {
	// Generate the API config from ENV and /var/run/secrets/kubernetes.io/serviceaccount inside a pod
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// Generate the clientset from the config
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	return clientset
}

// GET PODS FUNCTIONS

// "Un-cut" the username string from the user and domain strings
func getUserID(user string, domain string) string {
	if len(domain) > 0 {
		return fmt.Sprintf("%s@%s", user, domain)
	}
	return user
}

// Fills in a GetPodsResponse with information about all the pods owned by the user.
// If the username string is empty, use all pods in the namespace.
func getPods(username string, client v1.PodInterface) ([]GetPodsResponse, error) {
	var response []GetPodsResponse
	var opts metav1.ListOptions
	if len(username) < 1 {
		opts = metav1.ListOptions{}
	} else {
		user, domain, _ := strings.Cut(username, "@")
		opts = metav1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)}
	}
	podlist, err := client.List(context.TODO(), opts)
	if err != nil {
		return response, err
	}
	for _, n := range podlist.Items {
		var pod GetPodsResponse
		var ageSec = time.Now().Sub(n.Status.StartTime.Time).Seconds()
		pod.Age = fmt.Sprintf("%d:%d:%d", int32(ageSec/3600), int32(ageSec/60)%60, int32(ageSec)%60)
		pod.ContainerName = n.Spec.Containers[0].Name
		pod.ImageName = n.Spec.Containers[0].Image
		pod.NodeIP = n.Status.HostIP
		pod.Owner = getUserID(n.ObjectMeta.Labels["user"], n.ObjectMeta.Labels["domain"])
		pod.PodIP = n.Status.PodIP
		pod.PodName = n.ObjectMeta.Name
		pod.Status = fmt.Sprintf("%s:%s", n.Status.Phase, n.Status.StartTime.Format("2006-01-02T15:04:05Z"))
		//TODO: hostkeys, url, sshurl
		response = append(response, pod)
	}
	return response, nil
}

// Calls getPods using the http request, writes the http response with the getPods data
func (c *clientsetHandler) serveGetPods(w http.ResponseWriter, r *http.Request) {
	// parse the request
	var request GetPodsRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	fmt.Printf("getPods request: %+v\n", request)

	// get the list of pods
	response, err := getPods(request.UserID, c.podClient)
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

// CREATE POD FUNCTIONS

// Generate a request for testing getPod
func getTestCreatePodRequest(userID string, userIP string) CreatePodRequest {
	request := CreatePodRequest{
		UserID: userID,
		ContainerEnvVars: map[string]map[string]string{
			"jupyter": {
				"FILE": "foo",
				"WORKING_DIRECTORY": "foobar",
			},
		},
		AllEnvVars: map[string]string{
			"HOME_SERVER": userIP,
			"SD_UID": userID,
		},
		YamlURL: "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
		RemoteIP: userIP,
	}
	return request
}

// Generate an example api object to test pod creation
func getExamplePod(name string, user string, domain string) *apiv1.Pod {
	return &apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"user":   user,
				"domain": domain,
			},
		},
		Spec: apiv1.PodSpec{
			Containers: []apiv1.Container{
				{
					Name:  "jupyter",
					Image: "kube.sciencedata.dk:5000/jupyter_sciencedata_testing",
					Ports: []apiv1.ContainerPort{
						{
							ContainerPort: 8888,
							Protocol:      apiv1.ProtocolTCP,
						},
					},
				},
			},
		},
	}
}

// Example function for testing pod creation
func createExamplePod(name string, user string, domain string, podclient v1.PodInterface) (*apiv1.Pod, error) {
	pod := getExamplePod(name, user, domain)
	result, err := podclient.Create(context.TODO(), pod, metav1.CreateOptions{})
	return result, err
}

// Set values in the CreatePodRequest not stated in the http request json
func setAllEnvVars(request *CreatePodRequest, r *http.Request) {
	remoteIP := regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`).FindString(r.RemoteAddr)
	request.AllEnvVars = map[string]string{
		"HOME_SERVER": strings.Replace(remoteIP, sciencedataInternalNet, sciencedataPrivateNet, 1),
		"SD_UID":      request.UserID,
	}
	request.RemoteIP = remoteIP
}

// Make a unique name for the user's /tank/storage PV and PVC (same name used for both)
func getStoragePVName(remoteIP string, userID string) string {
	return fmt.Sprintf("nfs-%s-%s", remoteIP, getUserString(userID))
}

// Generate a unique string for each username that can be used in the api objects
func getUserString(userID string) string {
	userString := strings.Replace(userID, "@", "-", -1)
	userString = strings.Replace(userString, ".", "-", -1)
	return userString
}

// Retrieve the yaml manifest from our git repository
func getYaml(url string) (string, error) {
	allowed, err := regexp.MatchString(whitelistYamlURLRegex, url)
	if err != nil {
		return "", err
	}
	if allowed {
		response, err := http.Get(url)
		if err != nil {
			return "", errors.New(fmt.Sprintf("Could not fetch manifest from given url: %s", url))
		}
		defer response.Body.Close()

		// if the GET status isn't "200 OK"
		if response.StatusCode != 200 {
			return "", errors.New(fmt.Sprintf("Didn't find a file at the given url: %s", url))
		}

		body, err := ioutil.ReadAll(response.Body)
		if err != nil {
			return "", errors.New(fmt.Sprintf("Could not parse manifest from given url: %s", url))
		}

		return string(body), nil
	} else {
		return "", errors.New(fmt.Sprintf("YamlURL %s not matched to whitelist", url))
	}
}

// Fill in the pod's environment variables from the settings in the CreatePodRequest
func applyCreatePodRequestSettings(request CreatePodRequest, pod *apiv1.Pod) {
	user, domain, _ := strings.Cut(request.UserID, "@")
	pod.ObjectMeta.Labels = map[string]string{
		"user":   user,
		"domain": domain,
	}
	for i, container := range pod.Spec.Containers {
		envVars, exist := request.ContainerEnvVars[container.Name]
		// if there are settings for this container (if container.Name is a key in request.ContainerEnvVars)
		if exist {
			// then for each setting,
			for name, value := range envVars {
				// find the env entry with a matching name, and set the value
				for ii, env := range container.Env {
					if env.Name == name {
						pod.Spec.Containers[i].Env[ii].Value = value
					}
				}
			}
		}
		// for each envvar that should be set in every container,
		for name, value := range request.AllEnvVars {
			overwrite := false
			// try to overwrite the value if the var already exists
			for ii, env := range pod.Spec.Containers[i].Env {
				if env.Name == name {
					pod.Spec.Containers[i].Env[ii].Value = value
					overwrite = true
				}
			}
			// otherwise, append the var
			if !overwrite {
				pod.Spec.Containers[i].Env = append(pod.Spec.Containers[i].Env, apiv1.EnvVar{
					Name:  name,
					Value: value,
				})
			}
		}
	}
}

// Attempt to find a unique name for the pod. If successful, set it in the apiv1.Pod
func applyCreatePodName(request CreatePodRequest, targetPod *apiv1.Pod, client v1.PodInterface) error {
	basePodName := fmt.Sprintf("%s-%s", targetPod.ObjectMeta.Name, getUserString(request.UserID))
	user, domain, _ := strings.Cut(request.UserID, "@")
	existingPods, err := client.List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain),
		})
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't getPods to find a unique pod name: %s", err.Error()))
	}
	podName := basePodName
	var exists bool
	for i := 1; i < 11; i++ {
		exists = false
		for _, existingPod := range existingPods.Items {
			if existingPod.ObjectMeta.Name == podName {
				exists = true
				break
			}
		}
		// if a pod with the name podName doesn't exist yet
		if !exists {
			// then set the target pod's name and finish
			targetPod.ObjectMeta.Name = podName
			return nil
		}
		// otherwise try again with the next name
		podName = fmt.Sprintf("%s-%d", basePodName, i)
	}
	// if all 10 names are in use,
	return errors.New(fmt.Sprintf("Couldn't find a unique name for %s-(1-9), all are in use", basePodName))
}

// Dynamically generate the pod.Spec.Volume entry for an unsatisfied pod.Spec.Container[].VolumeMount
func getCreatePodSpecVolume(volumeMount apiv1.VolumeMount, request CreatePodRequest) (apiv1.Volume, error) {
	switch volumeMount.Name {
	case "local":
		return apiv1.Volume{
			Name: "local",
			VolumeSource: apiv1.VolumeSource{
				PersistentVolumeClaim: &apiv1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("local-claim-%s", strings.ReplaceAll(volumeMount.MountPath, "/", "-")),
				},
			},
		}, nil
	case "sciencedata":
		return apiv1.Volume{
			Name: "sciencedata",
			VolumeSource: apiv1.VolumeSource{
				PersistentVolumeClaim: &apiv1.PersistentVolumeClaimVolumeSource{
					ClaimName: getStoragePVName(request.RemoteIP, request.UserID),
				},
			},
		}, nil
	default:
		return apiv1.Volume{}, errors.New(
			fmt.Sprintf("Not known how to dynamically create an entry for this volume mount %+v", volumeMount),
		)
	}
}

// Make sure that any VolumeMounts that aren't specified in Spec.Volumes get added.
// This should be used for e.g. the user's storage, which should be generated at runtime
// for the given user.
func applyCreatePodVolumes(targetPod *apiv1.Pod, request CreatePodRequest) error {
	for _, container := range targetPod.Spec.Containers {
		for _, volumeMount := range container.VolumeMounts {
			// For each volume mount, first check whether the volume is specified in pod.Spec.Volumes
			satisfied := false
			for _, volume := range targetPod.Spec.Volumes {
				if volume.Name == volumeMount.Name {
					satisfied = true
					break
				}
			}
			if !satisfied {
				targetVolumeSpec, err := getCreatePodSpecVolume(volumeMount, request)
				if err != nil {
					return err
				}
				targetPod.Spec.Volumes = append(targetPod.Spec.Volumes, targetVolumeSpec)
			}
		}
	}
	return nil
}

// Generate api object for the pod to attempt to create
func getTargetPod(request CreatePodRequest, client v1.PodInterface) (apiv1.Pod, error) {
	var targetPod apiv1.Pod

	// Get the manifest
	yaml, err := getYaml(request.YamlURL)
	if err != nil {
		return targetPod, errors.New(fmt.Sprintf("Couldn't get manifest: %s", err.Error()))
	}

	// And convert it from []byte -> runtime.Object -> unstructured -> apiv1.Pod
	deserializer := scheme.Codecs.UniversalDeserializer()
	object, _, err := deserializer.Decode([]byte(yaml), nil, nil)
	if err != nil {
		return targetPod, errors.New(fmt.Sprintf("Couldn't deserialize manifest: %s", err.Error()))
	}
	unstructuredPod, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return targetPod, errors.New(fmt.Sprintf("Couldn't convert runtime.Object: %s", err.Error()))
	}
	// Fill out targetPod with the data from the manifest
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredPod, &targetPod)
	if err != nil {
		return targetPod, errors.New(fmt.Sprintf("Couldn't parse manifest as apiv1.Pod: %s", err.Error()))
	}

	// Fill in values in targetPod according to the request
	applyCreatePodRequestSettings(request, &targetPod)
	// Find and set a unique podName in the format pod.metadata.name-user-domain-x
	err = applyCreatePodName(request, &targetPod, client)
	if err != nil {
		return targetPod, err
	}
	err = applyCreatePodVolumes(&targetPod, request)
	if err != nil {
		return targetPod, err
	}

	return targetPod, nil
}

// Generate an api object for the PV to attempt to create for the user's /tank/storage
func getUserStoragePV(request CreatePodRequest) *apiv1.PersistentVolume {
	name := getStoragePVName(request.RemoteIP, request.UserID)
	user, domain, _ := strings.Cut(request.UserID, "@")
	return &apiv1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"name":   name,
				"user":   user,
				"domain": domain,
			},
		},
		Spec: apiv1.PersistentVolumeSpec{
			AccessModes: []apiv1.PersistentVolumeAccessMode{
				"ReadWriteMany",
			},
			PersistentVolumeReclaimPolicy: apiv1.PersistentVolumeReclaimRetain,
			StorageClassName:              "nfs",
			MountOptions: []string{
				"hard",
				"nfsvers=4.1",
			},
			PersistentVolumeSource: apiv1.PersistentVolumeSource{
				NFS: &apiv1.NFSVolumeSource{
					Server: request.RemoteIP,
					Path:   fmt.Sprintf("/tank/storage/%s", request.UserID),
				},
			},
			ClaimRef: &apiv1.ObjectReference{
				Namespace: namespace,
				Name:      name,
				Kind:      "PersistentVolumeClaim",
			},
			Capacity: apiv1.ResourceList{
				apiv1.ResourceStorage: resource.MustParse("10Gi"),
			},
		},
	}
}

// Generate an api object for the PVC to attempt to create for the user's /tank/storage
func getUserStoragePVC(request CreatePodRequest) *apiv1.PersistentVolumeClaim {
	name := getStoragePVName(request.RemoteIP, request.UserID)
	user, domain, _ := strings.Cut(request.UserID, "@")
	return &apiv1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				"name":   name,
				"user":   user,
				"domain": domain,
			},
		},
		Spec: apiv1.PersistentVolumeClaimSpec{
			//			StorageClassName: "nfs",
			AccessModes: []apiv1.PersistentVolumeAccessMode{
				"ReadWriteMany",
			},
			VolumeName: name,
			Resources: apiv1.ResourceRequirements{
				Requests: apiv1.ResourceList{
					apiv1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
	}
}

// Check that the PV and PVC for the user's /tank/storage directory exist
// Should be called iff the pod has a volume named "sciencedata"
func ensureUserStorageExists(
	request CreatePodRequest,
	PVClient v1.PersistentVolumeInterface,
	PVCClient v1.PersistentVolumeClaimInterface,
) error {
	name := getStoragePVName(request.RemoteIP, request.UserID)
	listOptions := metav1.ListOptions{LabelSelector: fmt.Sprintf("name=%s", name)}
	PVList, err := PVClient.List(context.TODO(), listOptions)
	if err != nil {
		return err
	}
	if len(PVList.Items) == 0 {
		targetPV := getUserStoragePV(request)
		createdPV, err := PVClient.Create(context.TODO(), targetPV, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		fmt.Printf("CREATED PV: %s\n", createdPV.ObjectMeta.Name)
	}
	PVCList, err := PVCClient.List(context.TODO(), listOptions)
	if err != nil {
		return err
	}
	if len(PVCList.Items) == 0 {
		targetPVC := getUserStoragePVC(request)
		createdPVC, err := PVCClient.Create(context.TODO(), targetPVC, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		fmt.Printf("CREATED PVC: %s\n", createdPVC.ObjectMeta.Name)
	}
	return nil
}

// Write true into ch when watcher receives an event for a ready pod
func waitPodReadySignal(watcher watch.Interface, ch chan bool) {
	// Run this loop every time an event is ready in the watcher channel
	for event := range watcher.ResultChan() {
		// event.Object is a new runtim.Object with the pod in its state after the event
		eventPod := event.Object.(*apiv1.Pod)
		// Loop through the pod conditions to find the one that's "Ready"
		for _, condition := range eventPod.Status.Conditions {
			if condition.Type == apiv1.PodReady {
				// If the pod is ready, then stop watching, so the event loop will terminate
				if condition.Status == apiv1.ConditionTrue {
					watcher.Stop()
					ch <- true
				}
				break
			}
		}
	}
}

// Block until returning either true (pod is ready) or false (timeout reached)
func waitPodReady(pod *apiv1.Pod, client v1.PodInterface) bool {
	// Create a watcher object
	listOptions := metav1.SingleObject(pod.ObjectMeta)
	watcher, err := client.Watch(context.TODO(), listOptions)
	if err != nil {
		fmt.Printf("Error preparing start jobs for %s, couldn't watch for status: %s\n",
			pod.ObjectMeta.Name,
			err.Error(),
		)
		return false
	}

	// Make a channel for waitPodReadySignal to use when the pod is ready
	readyChannel := make(chan bool)
	go waitPodReadySignal(watcher, readyChannel)
	// Write `false` into the channel after the timeout
	time.AfterFunc(podReadyTimeout, func() { readyChannel <- false })
	// return the first input into the channel
	return <- readyChannel
}

// Perform tasks that should be done for each created pod
func createPodStartJobs(pod *apiv1.Pod, client v1.PodInterface) {
	ready := waitPodReady(pod, client)
	if !ready {
		fmt.Printf("Pod %s didn't reach ready state. Start jobs not attempted.\n", pod.ObjectMeta.Name)
		return
	}
	// Perform start jobs here
}

// Create the pod and other necessary objects, start jobs that should run with pod creation
// If successful, return the name of the created pod and nil error
func createPod(
	request CreatePodRequest,
	podClient v1.PodInterface,
	PVClient v1.PersistentVolumeInterface,
	PVCClient v1.PersistentVolumeClaimInterface,
) (string, error) {
	// generate the pod api object to attempt to create
	targetPod, err := getTargetPod(request, podClient)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Error: Invalid targetPod: %s\n", err.Error()))
	}

	// if the pod requires a PV and PVC for the user, check that those exist, create if not
	hasUserStorage := false
	for _, volume := range targetPod.Spec.Volumes {
		if volume.Name == "sciencedata" {
			hasUserStorage = true
		}
	}
	if hasUserStorage {
		err = ensureUserStorageExists(request, PVClient, PVCClient)
		if err != nil {
			return "", errors.New(fmt.Sprintf("Error: Couldn't ensure user storage exists: %s", err.Error()))
		}
	}

	// create the pod
	createdPod, err := podClient.Create(context.TODO(), &targetPod, metav1.CreateOptions{})
	if err != nil {
		return "", errors.New(fmt.Sprintf("Error: Failed to create pod: %s", err.Error()))
	}
	fmt.Printf("CREATED POD: %s\n", createdPod.ObjectMeta.Name)
	//TODO getIngress
	//TODO copyHostkeys (in a nonblocking goroutine)
	go createPodStartJobs(createdPod, podClient)

	return createdPod.ObjectMeta.Name, nil
}

// Calls createPod with the http request, writes the success/failure http response
func (c *clientsetHandler) serveCreatePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request CreatePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	setAllEnvVars(&request, r)
	fmt.Printf("createPod request: %+v\n", request)

	podName, err := createPod(request, c.podClient, c.PVClient, c.PVCClient)

	var status int
	var response CreatePodResponse
	if err != nil {
		status = http.StatusBadRequest
		response.PodName = ""
	} else {
		status = http.StatusOK
		response.PodName = podName
	}

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

// DELETE POD FUNCTIONS

// Delete lingering PV and PVCs for user storage if they exist
func cleanUserStorage(
	request DeletePodRequest,
	PVClient v1.PersistentVolumeInterface,
	PVCClient v1.PersistentVolumeClaimInterface,
) error {
	name := getStoragePVName(request.RemoteIP, request.UserID)
	opts := metav1.ListOptions{LabelSelector: fmt.Sprintf("name=%s", name)}
	pvcList, err := PVCClient.List(context.TODO(), opts)
	if err != nil {
		return err
	}
	if len(pvcList.Items) > 0 {
		err = PVCClient.Delete(context.TODO(), name, metav1.DeleteOptions{})
		if err != nil {
			return errors.New(fmt.Sprintf("Error: Failed to delete PVC: %s", err.Error()))
		}
	}
	pvList, err := PVClient.List(context.TODO(), opts)
	if err != nil {
		return err
	}
	if len(pvList.Items) > 0 {
		err = PVClient.Delete(context.TODO(), name, metav1.DeleteOptions{})
		if err != nil {
			return errors.New(fmt.Sprintf("Error: Failed to delete PV: %s", err.Error()))
		}
	}
	return nil
}

// Delete all the pods owned by request.UserID
// Convenience function for testing
func deleteAllPodsUser(
	request DeletePodRequest,
	podClient v1.PodInterface,
	PVClient v1.PersistentVolumeInterface,
	PVCClient v1.PersistentVolumeClaimInterface,
) error {
	if request.UserID == "" {
		return errors.New("Need username of owner of pods to be deleted")
	}
	user, domain, _ := strings.Cut(request.UserID, "@")
	podlist, err := podClient.List(
		context.TODO(),
		metav1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)},
	)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list user's pods: %s", err.Error()))
	}
	for _, pod := range podlist.Items {
		err = podClient.Delete(context.TODO(), pod.ObjectMeta.Name, metav1.DeleteOptions{})
		if err != nil {
			return errors.New(fmt.Sprintf("Error while deleting pod: %s", err.Error()))
		}
	}
	err = cleanUserStorage(request, PVClient, PVCClient)
	if err != nil {
		return errors.New(fmt.Sprintf("Error while removing user storage: %s", err.Error()))
	}
	return nil
}

// Delete a pod and remove user storage if no longer in use
func deletePod(
	request DeletePodRequest,
	podClient v1.PodInterface,
	PVClient v1.PersistentVolumeInterface,
	PVCClient v1.PersistentVolumeClaimInterface,
) error {
	// check whether the pod exists, searching by user if username given
	var listOpts metav1.ListOptions
	if len(request.UserID) < 1 {
		listOpts = metav1.ListOptions{}
	} else {
		user, domain, _ := strings.Cut(request.UserID, "@")
		listOpts = metav1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)}
	}
	podlist, err := podClient.List(context.TODO(), listOpts)
	if err != nil {
		return errors.New(fmt.Sprintf("Error: Couldn't list pods to check for deletion: %s", err.Error()))
	}
	indexDelete := -1
	for i, pod := range podlist.Items {
		if pod.ObjectMeta.Name == request.PodName {
			indexDelete = i
			break
		}
	}
	// The index isn't used in the podClient.Delete request, but this serves as a necessary check
	// that the user and domain tags of the pod match the request.UserID
	if indexDelete == -1 {
		return errors.New("Pod doesn't exist, cannot be deleted")
	}

	// delete it
	deleteOpts := metav1.DeleteOptions{} // could include GracePeriodSeconds option here
	err = podClient.Delete(context.TODO(), request.PodName, deleteOpts)
	if err != nil {
		return errors.New(fmt.Sprintf("Error: Failed to delete: %s", err.Error()))
	}

	// If there are no pods remaining owned by this user,
	if len(podlist.Items) < 2 {
		err = cleanUserStorage(request, PVClient, PVCClient)
	}

	return nil
}

// Calls deletePod with the http request, writes the success/failure http response
func (c *clientsetHandler) serveDeletePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request DeletePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`).FindString(r.RemoteAddr)
	fmt.Printf("createPod request: %+v\n", request)

	err := deletePod(request, c.podClient, c.PVClient, c.PVCClient)
	var status int
	var response DeletePodResponse
	if err != nil {
		status = http.StatusBadRequest
		response.PodName = ""
	} else {
		status = http.StatusOK
		response.PodName = request.PodName
	}

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

func main() {
	clientset := getClientset()
	podClient := clientset.CoreV1().Pods(namespace)
	PVClient := clientset.CoreV1().PersistentVolumes()
	PVCClient := clientset.CoreV1().PersistentVolumeClaims(namespace)
	handler := clientsetHandler{
		clientset: clientset,
		podClient: podClient,
		PVClient:  PVClient,
		PVCClient: PVCClient,
	}
	// By writing serveGetPods etc as methods on a clientsetHandler, the podClient etc can
	// be created in main() and accessed inside the http.HandleFuncs without passing another argument
	http.HandleFunc("/get_pods", handler.serveGetPods)
	http.HandleFunc("/create_pod", handler.serveCreatePod)
	http.HandleFunc("/delete_pod", handler.serveDeletePod)
	http.ListenAndServe(":80", nil)
}
