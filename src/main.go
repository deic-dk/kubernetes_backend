package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// TODO figure out how to get the namespace automatically from within the pod where this runs
const namespace = "sciencedata-dev"
const whitelistYamlURLRegex = "https:\\/\\/raw[.]githubusercontent[.]com\\/deic-dk\\/pod_manifests"
const sciencedataPrivateNet = "10.2."
const sciencedataInternalNet = "10.0."
const timeoutCreate = 30 * time.Second
const timeoutDelete = 30 * time.Second

type GetPodsRequest struct {
	UserID string `json:"user_id"`
}

type GetPodsResponse struct {
	PodName       string            `json:"pod_name"`
	ContainerName string            `json:"container_name"`
	ImageName     string            `json:"image_name"`
	PodIP         string            `json:"pod_ip"`
	NodeIP        string            `json:"node_ip"`
	Owner         string            `json:"owner"`
	Age           string            `json:"age"`
	Status        string            `json:"status"`
	Url           string            `json:"url"`
	SshUrl        string            `json:"ssh_url"`
	Tokens        map[string]string `json:"tokens"`
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

type clientsetWrapper struct {
	clientset *kubernetes.Clientset
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

// K8S CLIENT UTILITY FUNCTIONS

// Set up a watcher to pass to signalFunc, which should ch<-true when the desired event occurs
func (c *clientsetWrapper) watchFor(
	name string,
	timeout time.Duration,
	resourceType string,
	signalFunc func(watch.Interface, chan<- bool),
	ch chan<- bool,
) {
	listOptions := metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", name)}
	var err error
	var watcher watch.Interface
	// create a watcher for the API resource of the correct type
	switch resourceType {
	case "Pod":
		watcher, err = c.clientset.CoreV1().Pods(namespace).Watch(context.TODO(), listOptions)
	case "PV":
		watcher, err = c.clientset.CoreV1().PersistentVolumes().Watch(context.TODO(), listOptions)
	case "PVC":
		watcher, err = c.clientset.CoreV1().PersistentVolumeClaims(namespace).Watch(context.TODO(), listOptions)
	default:
		err = errors.New("Unsupported resource type for watcher")
	}
	if err != nil {
		ch <- false
		fmt.Printf("Error in watchFor: %s\n", err.Error())
		return
	}
	// In a goroutine, sleep for the timeout duration and then push ch<-false
	time.AfterFunc(timeout, func() {
		watcher.Stop()
		select {
		case ch <- false:
		default:
		}
	})
	// In this goroutine, call the function to ch<-true when the desired event occurs
	signalFunc(watcher, ch)
}

// Do ch<-value if the channel is ready to receive a value,
// otherwise do nothing
// This allows the goroutine attempting a send to continue without blocking
// To ensure ch can take a value, make it a buffered channel with enough space
func trySend(ch chan<- bool, value bool) {
	select {
	case ch <- value:
	default:
	}
}

// Push ch<-true when watcher receives an event for a ready pod
func signalPodReady(watcher watch.Interface, ch chan<- bool) {
	// Run this loop every time an event is ready in the watcher channel
	for event := range watcher.ResultChan() {
		// the event.Object is only sure to be an apiv1.Pod if the event.Type is Modified
		if event.Type == watch.Modified {
			// event.Object is a new runtime.Object with the pod in its state after the event
			eventPod := event.Object.(*apiv1.Pod)
			// Loop through the pod conditions to find the one that's "Ready"
			for _, condition := range eventPod.Status.Conditions {
				if condition.Type == apiv1.PodReady {
					// If the pod is ready, then stop watching, so the event loop will terminate
					if condition.Status == apiv1.ConditionTrue {
						fmt.Printf("READY POD: %s\n", eventPod.Name)
						watcher.Stop()
						trySend(ch, true)
					}
					break
				}
			}
		}
	}
}

// Log that an event.Object has been deleted
func announceDeleted(obj runtime.Object) {
	// get its kind
	// unfortunately the kind isn't stored in any of the fields, but is contained in the object type
	typeStr := fmt.Sprintf("%s", reflect.TypeOf(obj))
	var kindStr string
	switch typeStr {
	case "*v1.Pod":
		kindStr = "POD"
	case "*v1.PersistentVolume":
		kindStr = "PV"
	case "*v1.PersistentVolumeClaim":
		kindStr = "PVC"
	default:
		kindStr = "?"
		fmt.Printf("Unknown typestr: %s\n", typeStr)
	}

	// Get the object's name
	unstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		fmt.Printf("Error while announcing deletion: %s\n%+v\n", err.Error(), obj)
		return
	}
	metadata := unstructured["metadata"].(map[string]interface{})
	name := metadata["name"].(string)

	// And write the log
	fmt.Printf("DELETED %s: %s\n", kindStr, name)
}

// Push ch<-true when the object watcher is watching is deleted
func signalDeleted(watcher watch.Interface, ch chan<- bool) {
	for event := range watcher.ResultChan() {
		if event.Type == watch.Deleted {
			announceDeleted(event.Object)
			watcher.Stop()
			trySend(ch, true)
		}
	}
}

// Push ch<-true when the Persistent Volume is ready
func signalPVReady(watcher watch.Interface, ch chan<- bool) {
	for event := range watcher.ResultChan() {
		if event.Type == watch.Modified {
			pv := event.Object.(*apiv1.PersistentVolume)
			if pv.Status.Phase == apiv1.VolumeAvailable {
				fmt.Printf("AVAILABLE PV: %s\n", pv.Name)
				watcher.Stop()
				trySend(ch, true)
			}
		}
	}
}

// Push ch<-true when when Persistent Volume Claim is bound
func signalPVCReady(watcher watch.Interface, ch chan<- bool) {
	for event := range watcher.ResultChan() {
		if event.Type == watch.Modified {
			pvc := event.Object.(*apiv1.PersistentVolumeClaim)
			if pvc.Status.Phase == apiv1.ClaimBound {
				fmt.Printf("BOUND PVC: %s\n", pvc.Name)
				watcher.Stop()
				trySend(ch, true)
			}
		}
	}
}

func (c *clientsetWrapper) ClientListPods(opt metav1.ListOptions) (*apiv1.PodList, error) {
	return c.clientset.CoreV1().Pods(namespace).List(context.TODO(), opt)
}

func (c *clientsetWrapper) ClientDeletePod(name string, finished chan<- bool) error {
	go c.watchFor(name, timeoutDelete, "Pod", signalDeleted, finished)
	return c.clientset.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (c *clientsetWrapper) ClientCreatePod(target *apiv1.Pod, ready chan<- bool) (*apiv1.Pod, error) {
	go c.watchFor(target.Name, timeoutCreate, "Pod", signalPodReady, ready)
	return c.clientset.CoreV1().Pods(namespace).Create(context.TODO(), target, metav1.CreateOptions{})
}

func (c *clientsetWrapper) ClientListPVC(opt metav1.ListOptions) (*apiv1.PersistentVolumeClaimList, error) {
	return c.clientset.CoreV1().PersistentVolumeClaims(namespace).List(context.TODO(), opt)
}

func (c *clientsetWrapper) ClientDeletePVC(name string, finished chan<- bool) error {
	go c.watchFor(name, timeoutDelete, "PVC", signalDeleted, finished)
	return c.clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (c *clientsetWrapper) ClientCreatePVC(target *apiv1.PersistentVolumeClaim, ready chan<- bool) (*apiv1.PersistentVolumeClaim, error) {
	go c.watchFor(target.Name, timeoutCreate, "PVC", signalPVCReady, ready)
	return c.clientset.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), target, metav1.CreateOptions{})
}

func (c *clientsetWrapper) ClientListPV(opt metav1.ListOptions) (*apiv1.PersistentVolumeList, error) {
	return c.clientset.CoreV1().PersistentVolumes().List(context.TODO(), opt)
}

func (c *clientsetWrapper) ClientDeletePV(name string, finished chan<- bool) error {
	go c.watchFor(name, timeoutDelete, "PV", signalDeleted, finished)
	return c.clientset.CoreV1().PersistentVolumes().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (c *clientsetWrapper) ClientCreatePV(target *apiv1.PersistentVolume, ready chan<- bool) (*apiv1.PersistentVolume, error) {
	go c.watchFor(target.Name, timeoutCreate, "PV", signalPVReady, ready)
	return c.clientset.CoreV1().PersistentVolumes().Create(context.TODO(), target, metav1.CreateOptions{})
}

// GET PODS FUNCTIONS

// Wrapper for ClientListPods that selects labels for the given username,
// lists all pods for username=""
func (c *clientsetWrapper) getUserPodList(username string) (*apiv1.PodList, error) {
	var listOpts metav1.ListOptions
	if username == "" {
		listOpts = metav1.ListOptions{}
	} else {
		user, domain, _ := strings.Cut(username, "@")
		listOpts = metav1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)}
	}
	return c.ClientListPods(listOpts)
}

// "Un-cut" the username string from the user and domain strings
func getUserID(user string, domain string) string {
	if len(domain) > 0 {
		return fmt.Sprintf("%s@%s", user, domain)
	}
	return user
}

func fillPodResponse(existingPod apiv1.Pod) GetPodsResponse {
	var podInfo GetPodsResponse
	var ageSec = time.Now().Sub(existingPod.Status.StartTime.Time).Seconds()
	podInfo.Age = fmt.Sprintf("%d:%d:%d", int32(ageSec/3600), int32(ageSec/60)%60, int32(ageSec)%60)
	podInfo.ContainerName = existingPod.Spec.Containers[0].Name
	podInfo.ImageName = existingPod.Spec.Containers[0].Image
	podInfo.NodeIP = existingPod.Status.HostIP
	podInfo.Owner = getUserID(existingPod.ObjectMeta.Labels["user"], existingPod.ObjectMeta.Labels["domain"])
	podInfo.PodIP = existingPod.Status.PodIP
	podInfo.PodName = existingPod.Name
	podInfo.Status = fmt.Sprintf("%s:%s", existingPod.Status.Phase, existingPod.Status.StartTime.Format("2006-01-02T15:04:05Z"))

	// Initialize the tokens map so it can be written into in the following block
	podInfo.Tokens = make(map[string]string)
	for key, value := range existingPod.ObjectMeta.Annotations {
		// If this key is specified in the manifest to be copied from /tmp/key and shown to the user in the frontend
		if value == "copyForFrontend" {
			filename := fmt.Sprintf("/tmp/%s-%s", existingPod.Name, key)
			content, err := ioutil.ReadFile(filename)
			if err != nil {
				// If the file is missing, just let the key be absent for the user, but log that it occurred
				fmt.Printf("Error copying tokens from file %s: %s\n", filename, err.Error())
				continue
			}
			podInfo.Tokens[key] = string(content)
		}
	}

	// TODO get url from ingress
	// TODO get ssh_url from service if exists
	return podInfo
}

// Fills in a GetPodsResponse with information about all the pods owned by the user.
// If the username string is empty, use all pods in the namespace.
func (c *clientsetWrapper) getPods(username string) ([]GetPodsResponse, error) {
	var response []GetPodsResponse
	podList, err := c.getUserPodList(username)
	if err != nil {
		return response, err
	}
	for _, existingPod := range podList.Items {
		podInfo := fillPodResponse(existingPod)
		response = append(response, podInfo)
	}
	return response, nil
}

// Calls getPods using the http request, writes the http response with the getPods data
func (c *clientsetWrapper) serveGetPods(w http.ResponseWriter, r *http.Request) {
	// parse the request
	var request GetPodsRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	fmt.Printf("getPods request: %+v\n", request)

	// get the list of pods
	response, err := c.getPods(request.UserID)
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
				"FILE":              "foo",
				"WORKING_DIRECTORY": "foobar",
			},
		},
		AllEnvVars: map[string]string{
			"HOME_SERVER": userIP,
			"SD_UID":      userID,
		},
		YamlURL:  "https://raw.githubusercontent.com/deic-dk/pod_manifests/testing/jupyter_sciencedata.yaml",
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
func createExamplePod(name string, user string, domain string, clientset *kubernetes.Clientset) (*apiv1.Pod, error) {
	pod := getExamplePod(name, user, domain)
	return clientset.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
}

// Block until an input was received from each channel in chans,
// then send combined <- chans0 && chans1 && chans2...
func combineBoolChannels(chans []<-chan bool, combined chan<- bool) {
	output := true
	for _, ch := range chans {
		if !<-ch {
			output = false
		}
	}
	combined <- output
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
func getStoragePVName(userID string) string {
	return fmt.Sprintf("user-storage-%s", getUserString(userID))
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
func (c *clientsetWrapper) applyCreatePodName(request CreatePodRequest, targetPod *apiv1.Pod) error {
	basePodName := fmt.Sprintf("%s-%s", targetPod.Name, getUserString(request.UserID))
	user, domain, _ := strings.Cut(request.UserID, "@")
	existingPods, err := c.ClientListPods(metav1.ListOptions{
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
			if existingPod.Name == podName {
				exists = true
				break
			}
		}
		// if a pod with the name podName doesn't exist yet
		if !exists {
			// then set the target pod's name and finish
			targetPod.Name = podName
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
					ClaimName: getStoragePVName(request.UserID),
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

// Retrieve the yaml manifest and parse it into a pod API object to attempt to create
func (c *clientsetWrapper) getTargetPod(request CreatePodRequest) (apiv1.Pod, error) {
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
	err = c.applyCreatePodName(request, &targetPod)
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
	name := getStoragePVName(request.UserID)
	user, domain, _ := strings.Cut(request.UserID, "@")
	return &apiv1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"name":   name,
				"user":   user,
				"domain": domain,
				"server": request.RemoteIP,
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
	name := getStoragePVName(request.UserID)
	user, domain, _ := strings.Cut(request.UserID, "@")
	return &apiv1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				"name":   name,
				"user":   user,
				"domain": domain,
				"server": request.RemoteIP,
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
func (c *clientsetWrapper) ensureUserStorageExists(request CreatePodRequest, ready chan<- bool) error {
	name := getStoragePVName(request.UserID)
	listOptions := metav1.ListOptions{LabelSelector: fmt.Sprintf("name=%s", name)}
	PVready := make(chan bool, 1)
	PVCready := make(chan bool, 1)
	PVList, err := c.ClientListPV(listOptions)
	if err != nil {
		return err
	}
	if len(PVList.Items) == 0 {
		targetPV := getUserStoragePV(request)
		createdPV, err := c.ClientCreatePV(targetPV, PVready)
		if err != nil {
			return err
		}
		fmt.Printf("CREATED PV: %s\n", createdPV.Name)
	} else {
		PVready <- true
	}
	PVCList, err := c.ClientListPVC(listOptions)
	if err != nil {
		return err
	}
	if len(PVCList.Items) == 0 {
		targetPVC := getUserStoragePVC(request)
		createdPVC, err := c.ClientCreatePVC(targetPVC, PVCready)
		if err != nil {
			return err
		}
		fmt.Printf("CREATED PVC: %s\n", createdPVC.Name)
	} else {
		PVCready <- true
	}
	go combineBoolChannels([]<-chan bool{PVready, PVCready}, ready)
	return nil
}

// call a bash function inside of a pod, with the command given as a []string of bash words
func (c *clientsetWrapper) podExec(command []string, pod *apiv1.Pod) (bytes.Buffer, bytes.Buffer, error) {
	var stdout, stderr bytes.Buffer
	config, err := rest.InClusterConfig()
	if err != nil {
		return stdout, stderr, errors.New(fmt.Sprintf("Couldn't get rest config: %s", err.Error()))
	}
	restRequest := c.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(
			&apiv1.PodExecOptions{
				Container: pod.Spec.Containers[0].Name,
				Command:   command,
				Stdin:     false,
				Stdout:    true,
				Stderr:    true,
				TTY:       false,
			},
			scheme.ParameterCodec,
		)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", restRequest.URL())
	if err != nil {
		return stdout, stderr, errors.New(fmt.Sprintf("Couldn't create executor: %s", err.Error()))
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		return stdout, stderr, errors.New(fmt.Sprintf("Stream error: %s", err.Error()))
	}
	return stdout, stderr, nil
}

// Try up to 5 times to copy /tmp/"key" in the created pod into /tmp
func (c *clientsetWrapper) copyToken(key string, pod *apiv1.Pod) error {
	filename := fmt.Sprintf("%s/%s", getPodTokenDir(pod.Name), key)
	var stdout, stderr bytes.Buffer
	var err error
	for i := 0; i < 5; i++ {
		stdout, stderr, err = c.podExec([]string{"cat", fmt.Sprintf("/tmp/%s", key)}, pod)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		} else {
			// Limit output size to... 4kB?
			err = ioutil.WriteFile(filename, stdout.Bytes(), 0600)
			if err != nil {
				return errors.New(fmt.Sprintf("Couldn't write file %s: %s", filename, err.Error()))
			}
			return nil
		}
	}
	return errors.New(fmt.Sprintf("Timeout while trying to copy %s: %s", filename, stderr.String()))
}

func (c *clientsetWrapper) copyAllTokens(pod *apiv1.Pod) {
	var toCopy []string
	for key, value := range pod.ObjectMeta.Annotations {
		if value == "copyForFrontend" {
			toCopy = append(toCopy, key)
		}
	}
	if len(toCopy) > 0 {
		dirName := getPodTokenDir(pod.Name)
		// Check to see if the token directory exists
		_, err := os.Stat(dirName)
		if err != nil {
			// If there's an error in stat for a reason other than that the directory doesn't exist,
			if !os.IsNotExist(err) {
				fmt.Printf("Couldn't stat directory for copied tokens for %s: %s\n", pod.Name, err.Error())
				return
			}
		} else {
			// Then the directory exists and needs to be cleaned before being recreated
			err = os.RemoveAll(dirName)
			if err != nil {
				fmt.Printf("Couldn't remove old token directory for %s: %s\n", pod.Name, err.Error())
				return
			}
		}
		// Now the token directory will be created
		err = os.Mkdir(dirName, 0700)
		if err != nil {
			fmt.Printf("Couldn't create dir to copy tokens for %s: %s\n", pod.Name, err.Error())
			return
		}
	}
	for _, key := range toCopy {
		err := c.copyToken(key, pod)
		if err != nil {
			fmt.Printf("Error while copying key: %s", err.Error())
		}
	}
}

// Perform tasks that should be done for each created pod
func (c *clientsetWrapper) createPodStartJobs(pod *apiv1.Pod, podReady <-chan bool, storageReady <-chan bool, finished chan<- bool) {
	if !(<-podReady && <-storageReady) {
		fmt.Printf("Pod %s and/or user storage didn't reach ready state. Start jobs not attempted.\n", pod.Name)
		trySend(finished, false)
		return
	}

	// Perform start jobs here
	c.copyAllTokens(pod)

	trySend(finished, true)
}

// Create the pod and other necessary objects, start jobs that should run with pod creation
// If successful, return the name of the created pod and nil error
func (c *clientsetWrapper) createPod(request CreatePodRequest, createPodFinished chan<- bool) (string, error) {
	// generate the pod api object to attempt to create
	targetPod, err := c.getTargetPod(request)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Invalid targetPod: %s\n", err.Error()))
	}

	// if the pod requires a PV and PVC for the user, check that those exist, create if not
	hasUserStorage := false
	for _, volume := range targetPod.Spec.Volumes {
		if volume.Name == "sciencedata" {
			hasUserStorage = true
		}
	}
	userStorageReady := make(chan bool, 1)
	if hasUserStorage {
		err = c.ensureUserStorageExists(request, userStorageReady)
		if err != nil {
			return "", errors.New(fmt.Sprintf("Couldn't ensure user storage exists: %s", err.Error()))
		}
	} else {
		userStorageReady <- true
	}

	// create the pod
	podReady := make(chan bool, 1)
	createdPod, err := c.ClientCreatePod(&targetPod, podReady)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Failed to create pod: %s", err.Error()))
	}
	fmt.Printf("CREATED POD: %s\n", createdPod.Name)
	//TODO getIngress
	//TODO copyHostkeys (in a nonblocking goroutine)
	go c.createPodStartJobs(createdPod, userStorageReady, podReady, createPodFinished)

	return createdPod.Name, nil
}

// Calls createPod with the http request, writes the success/failure http response
func (c *clientsetWrapper) serveCreatePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request CreatePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	setAllEnvVars(&request, r)
	fmt.Printf("createPod request: %+v\n", request)

	finished := make(chan bool, 1)
	podName, err := c.createPod(request, finished)

	var status int
	var response CreatePodResponse
	if err != nil {
		status = http.StatusBadRequest
		response.PodName = ""
		fmt.Printf("Error: %s\n", err.Error())
	} else {
		status = http.StatusOK
		response.PodName = podName
	}

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)

	go func() {
		<-finished
		close(finished)
	}()
}

// DELETE POD FUNCTIONS

// Delete lingering PV and PVCs for user storage if they exist
func (c *clientsetWrapper) cleanUserStorage(request DeletePodRequest, finished chan<- bool) error {
	if request.UserID == "" {
		return nil
	}
	name := getStoragePVName(request.UserID)
	opts := metav1.ListOptions{LabelSelector: fmt.Sprintf("name=%s", name)}
	PVCfinished := make(chan bool, 1)
	pvcList, err := c.ClientListPVC(opts)
	if err != nil {
		return err
	}
	// If there is a PVC to be deleted, request it and listen on PVCfinished
	if len(pvcList.Items) > 0 {
		err = c.ClientDeletePVC(name, PVCfinished)
		if err != nil {
			return errors.New(fmt.Sprintf("Failed to request deletion of PVC: %s", err.Error()))
		}
	} else {
		// Otherwise, PVCfinished should be signalled now
		PVCfinished <- true
	}

	// Repeat for the Persistent Volume
	PVfinished := make(chan bool, 1)
	pvList, err := c.ClientListPV(opts)
	if err != nil {
		return err
	}
	if len(pvList.Items) > 0 {
		err = c.ClientDeletePV(name, PVfinished)
		if err != nil {
			return errors.New(fmt.Sprintf("Failed to request deletion of PV: %s", err.Error()))
		}
	} else {
		PVfinished <- true
	}
	go combineBoolChannels([]<-chan bool{PVCfinished, PVfinished}, finished)
	return nil
}

// Delete all the pods owned by request.UserID
// Convenience function for testing
func (c *clientsetWrapper) deleteAllPodsUser(request DeletePodRequest, finished chan<- bool) error {
	if request.UserID == "" {
		return errors.New("Need username of owner of pods to be deleted")
	}
	podList, err := c.getUserPodList(request.UserID)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list user's pods: %s", err.Error()))
	}
	var allChans []<-chan bool
	for _, pod := range podList.Items {
		podChan := make(chan bool, 1)
		err = c.ClientDeletePod(pod.Name, podChan)
		if err != nil {
			return errors.New(fmt.Sprintf("Error while deleting pod: %s", err.Error()))
		}
		allChans = append(allChans, podChan)
		err = c.cleanTempFiles(pod.Name)
		if err != nil {
			return errors.New(fmt.Sprintf("Error while cleaning pod files: %s", err.Error()))
		}
	}
	storageChan := make(chan bool, 1)
	err = c.cleanUserStorage(request, storageChan)
	if err != nil {
		return errors.New(fmt.Sprintf("Error while removing user storage: %s", err.Error()))
	}
	allChans = append(allChans, storageChan)
	go combineBoolChannels(allChans, finished)
	return nil
}

// Get the user ID to whom a PV belongs if it is a valid user storage,
// otherwise return an empty string
func getPVUserID(pv apiv1.PersistentVolume) string {
	user := pv.ObjectMeta.Labels["user"]
	domain := pv.ObjectMeta.Labels["domain"]
	uid := getUserID(user, domain)
	if pv.Name != getStoragePVName(uid) {
		return ""
	}
	return uid
}

func getPodTokenDir(podName string) string {
	return fmt.Sprintf("/tmp/tokens/%s", podName)
}

func (c *clientsetWrapper) cleanTempFiles(podName string) error {
	dirName := getPodTokenDir(podName)
	_, err := os.Stat(dirName)
	// If the tmp directory for tokens doesn't exist, return without error
	if os.IsNotExist(err) {
		return nil
	}
	// If there is a different error, return it
	if err != nil {
		return err
	}
	err = os.RemoveAll(dirName)
	if err != nil {
		return err
	}
	return nil
}

func (c *clientsetWrapper) deletePodCleanJobs(request DeletePodRequest, cleanStorage bool, podDeleted <-chan bool, finished chan<- bool) {
	if !<-podDeleted {
		fmt.Printf("Pod %s didn't finish deleting before timeout. Cleanup jobs not attempted.\n", request.PodName)
		trySend(finished, false)
		return
	}

	tempFilesOkay := true
	err := c.cleanTempFiles(request.PodName)
	if err != nil {
		fmt.Printf("Couldn't clean directory of temp files for %s: %s\n", request.PodName, err.Error())
		tempFilesOkay = false
	}

	storageClean := true
	// if the user has no other pods, then:
	if cleanStorage {
		ch := make(chan bool, 1)
		err = c.cleanUserStorage(request, ch)
		if err != nil {
			fmt.Printf("Couldn't clean user storage for %s after pod deletion: %s\n", request.UserID, err.Error())
		}
		// Block until PV and PVC are deleted or timeout
		storageClean = <-ch
	}

	// Once all jobs finish, finished<-true iff all jobs finished successfully
	finished <- (tempFilesOkay && storageClean)
}

// Delete a pod and remove user storage if no longer in use,
// return nil when the delete request was made successfully,
// push finished<-true when clean up tasks complete successfully
func (c *clientsetWrapper) deletePod(request DeletePodRequest, finished chan<- bool) error {
	// check whether the pod exists, searching by user if username given
	podList, err := c.getUserPodList(request.UserID)
	if err != nil {
		return errors.New(fmt.Sprintf("Error: Couldn't list pods to check for deletion: %s", err.Error()))
	}
	foundPod := false
	for _, pod := range podList.Items {
		if pod.ObjectMeta.Name == request.PodName {
			foundPod = true
			break
		}
	}
	if !foundPod {
		return errors.New("Pod doesn't exist or isn't owned by given user, cannot be deleted")
	}

	cleanStorage := false
	// If there are no other pods owned by the user, then clean user storage after successful pod deletion
	if len(podList.Items) < 2 {
		cleanStorage = true
	}

	podDeleted := make(chan bool, 1)
	err = c.ClientDeletePod(request.PodName, podDeleted)
	if err != nil {
		return errors.New(fmt.Sprintf("Error: Failed to request deletion of Pod: %s", err.Error()))
	}
	go c.deletePodCleanJobs(request, cleanStorage, podDeleted, finished)

	return nil
}

// Calls deletePod with the http request, writes the success/failure http response
func (c *clientsetWrapper) serveDeletePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request DeletePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	request.RemoteIP = regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`).FindString(r.RemoteAddr)
	fmt.Printf("deletePod request: %+v\n", request)

	finished := make(chan bool)
	err := c.deletePod(request, finished)
	var status int
	var response DeletePodResponse
	if err != nil {
		status = http.StatusBadRequest
		response.PodName = ""
		fmt.Printf("Error: %s\n", err.Error())
	} else {
		status = http.StatusOK
		response.PodName = request.PodName
	}

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)

	// close the channel to avoid leaks
	go func() {
		<-finished
		close(finished)
	}()
}

// Get the user ID to whom a PVC belongs if it is a valid user storage,
// otherwise return an empty string
func getPVCUserID(pvc apiv1.PersistentVolumeClaim) string {
	user := pvc.ObjectMeta.Labels["user"]
	domain := pvc.ObjectMeta.Labels["domain"]
	uid := getUserID(user, domain)
	if pvc.Name != getStoragePVName(uid) {
		return ""
	}
	return uid
}

// Remove all the unused user storage and tempfiles
func (c *clientsetWrapper) cleanAllUnused(finished chan<- bool) error {
	podList, err := c.getUserPodList("")
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list all pods: %s\n", err.Error()))
	}

	// Get a list of all pods and pod owners
	var podNameList []string
	userIDsWithPods := make(map[string]bool)
	for _, pod := range podList.Items {
		podNameList = append(podNameList, pod.Name)
		userID := getUserID(
			pod.ObjectMeta.Labels["user"],
			pod.ObjectMeta.Labels["domain"],
		)
		if userID != "" {
			// use a map so that if a user has multiple pods, they will appear once in the list of keys
			userIDsWithPods[userID] = true
		}
	}

	var allChans []<-chan bool

	// Clean Persistent Volume Claims
	PVCList, err := c.ClientListPVC(metav1.ListOptions{})
	for _, pvc := range PVCList.Items {
		owner := getPVCUserID(pvc)
		// If this PV is a user storage, it will have an owner with a nonempty name
		if owner != "" {
			// check whether the owner has any pods running
			inUse := false
			for userID := range userIDsWithPods {
				if userID == owner {
					inUse = true
					break
				}
			}
			// if the owner doesn't have any pods, the PV should be deleted
			if !inUse {
				deleted := make(chan bool, 1)
				err := c.ClientDeletePVC(pvc.Name, deleted)
				if err != nil {
					return errors.New(fmt.Sprintf("Couldn't delete PV %s: %s\n", pvc.Name, err.Error()))
				}
				allChans = append(allChans, deleted)
			}
		}
	}

	// Clean Persistent Volumes
	PVList, err := c.ClientListPV(metav1.ListOptions{})
	for _, pv := range PVList.Items {
		owner := getPVUserID(pv)
		// If this PV is a user storage, it will have an owner with a nonempty name
		if owner != "" {
			// check whether the owner has any pods running
			inUse := false
			for userID := range userIDsWithPods {
				if userID == owner {
					inUse = true
					break
				}
			}
			// if the owner doesn't have any pods, the PV should be deleted
			if !inUse {
				deleted := make(chan bool, 1)
				err := c.ClientDeletePV(pv.Name, deleted)
				if err != nil {
					return errors.New(fmt.Sprintf("Couldn't delete PV %s: %s\n", pv.Name, err.Error()))
				}
				allChans = append(allChans, deleted)
			}
		}
	}

	// Clean Temporary Files
	files, err := ioutil.ReadDir(getPodTokenDir(""))
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list token directory: %s\n", err.Error()))
	}
	for _, file := range files {
		filename := file.Name()
		inUse := false
		for _, podName := range podNameList {
			if podName == filename {
				inUse = true
				break
			}
		}
		if !inUse {
			err = os.RemoveAll(getPodTokenDir(filename))
			if err != nil {
				return errors.New(fmt.Sprintf("Couldn't delete unused files: %s\n", err.Error()))
			}
		}
	}

	go combineBoolChannels(allChans, finished)
	return nil
}

func (c *clientsetWrapper) serveCleanAllUnused(w http.ResponseWriter, r *http.Request) {
	finished := make(chan bool, 1)
	err := c.cleanAllUnused(finished)
	status := http.StatusOK
	reply := "Success\n"
	if err != nil {
		fmt.Printf("Error while cleaning unused: %s\n", err.Error())
		status = http.StatusBadRequest
		reply = "Error\n"
	}
	// write the response
	w.WriteHeader(status)
	fmt.Fprint(w, reply)
	go func() {
		<-finished
		close(finished)
	}()
}

func main() {
	csWrapper := clientsetWrapper{
		clientset: getClientset(),
	}
	// By writing serveGetPods etc as methods on a clientsetWrapper, the clientset can
	// be created in main() and accessed inside the http.HandleFuncs without passing another argument
	http.HandleFunc("/get_pods", csWrapper.serveGetPods)
	http.HandleFunc("/create_pod", csWrapper.serveCreatePod)
	http.HandleFunc("/delete_pod", csWrapper.serveDeletePod)
	http.HandleFunc("/clean_unused", csWrapper.serveCleanAllUnused)
	err := http.ListenAndServe(":80", nil)
	if err != nil {
		fmt.Printf("Error running server: %s\n", err.Error())
	}
}
