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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// TODO figure out how to get the namespace automatically from within the pod where this runs
const namespace = "sciencedata-dev"
const whitelistYamlURLRegex = "https:\\/\\/raw[.]githubusercontent[.]com\\/deic-dk\\/pod_manifests"
const sciencedataPrivateNet = "10.2."
const sciencedataInternalNet = "10.0."

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
	RemoteIP string
}

type GetPodsRequest struct {
	UserID string `json:"user_id"`
}

func getPodClient() v1.PodInterface {
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
	return clientset.CoreV1().Pods(namespace)
}

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

func getUserID(user string, domain string) string {
	if len(domain) > 0 {
		return fmt.Sprintf("%s@%s", user, domain)
	}
	return user
}

func getPods(username string) ([]GetPodsResponse, error) {
	var response []GetPodsResponse
	var opts metav1.ListOptions
	if len(username) < 1 {
		opts = metav1.ListOptions{}
	} else {
		user, domain, _ := strings.Cut(username, "@")
		opts = metav1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)}
	}
	podclient := getPodClient()
	podlist, err := podclient.List(context.TODO(), opts)
	if err != nil {
		panic(err.Error())
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

func helloWorld(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("hello"))
}

func serveGetPods(w http.ResponseWriter, r *http.Request) {
	// parse the request
	var request GetPodsRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	fmt.Printf("getPods request: %+v\n", request)

	// get the list of pods
	data, _ := getPods(request.UserID)

	// write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(data)
}

func createExamplePod(name string, user string, domain string) (*apiv1.Pod, error) {
	podclient := getPodClient()
	pod := getExamplePod(name, user, domain)
	result, err := podclient.Create(context.TODO(), pod, metav1.CreateOptions{})
	return result, err
}

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

func getUserString(request CreatePodRequest) string {
	userString := strings.Replace(request.UserID, "@", "-", -1)
	userString = strings.Replace(userString, ".", "-", -1)
	return userString
}

func applyCreatePodName(request CreatePodRequest, targetPod *apiv1.Pod) error {
	basePodName := fmt.Sprintf("%s-%s", targetPod.ObjectMeta.Name, getUserString(request))
	existingPods, err := getPods(request.UserID)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't getPods to find a unique pod name: %s", err.Error()))
	}
	podName := basePodName
	var exists bool
	for i := 1; i < 11; i++ {
		exists = false
		for _, existingPod := range existingPods {
			if existingPod.PodName == podName {
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

func getTargetPod(request CreatePodRequest) (apiv1.Pod, error) {
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
	err = applyCreatePodName(request, &targetPod)
	if err != nil {
		return targetPod, err
	}

	return targetPod, nil
}

func setAllEnvVars(request *CreatePodRequest, r *http.Request) {
	remoteIP := regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`).FindString(r.RemoteAddr)
	request.AllEnvVars = map[string]string{
		"HOME_SERVER": strings.Replace(remoteIP, sciencedataInternalNet, sciencedataPrivateNet, 1),
		"SD_UID": request.UserID,
	}
	request.RemoteIP = remoteIP
}

func getPVName(request CreatePodRequest) string {
	return fmt.Sprintf("nfs-%s-%s", request.RemoteIP, getUserString(request))
}

func getUserStoragePV(request CreatePodRequest) *apiv1.PersistentVolume {
	return &apiv1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: getPVName(request),
		},
		Spec: apiv1.PersistentVolumeSpec{
			AccessModes: []apiv1.PersistentVolumeAccessMode{
				"ReadWriteMany",
			},
			PersistentVolumeReclaimPolicy: apiv1.PersistentVolumeReclaimRetain,
			StorageClassName: "nfs",
			MountOptions: []string{
				"hard",
				"nfsvers=4.1",
			},
			PersistentVolumeSource: apiv1.PersistentVolumeSource{
				NFS: &apiv1.NFSVolumeSource{
					Server: request.RemoteIP,
					Path: fmt.Sprintf("/tank/storage/%s", request.UserID),
				},
			},
			ClaimRef: &apiv1.ObjectReference{
				Namespace: namespace,
				Name: getPVName(request),
				Kind: "PersistentVolumeClaim",
			},
		},
	}
}

func getUserStoragePVC(request CreatePodRequest) *apiv1.PersistentVolumeClaim {
	return &apiv1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name: getPVName(request),
		},
		Spec: apiv1.PersistentVolumeClaimSpec{
			//			StorageClassName: "nfs",
			AccessModes: []apiv1.PersistentVolumeAccessMode{
				"ReadWriteMany",
			},
			VolumeName: getPVName(request),
		},
	}
}

func serveCreatePod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request CreatePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	setAllEnvVars(&request, r)
	fmt.Printf("createPod request: %+v\n", request)

	targetPod, err := getTargetPod(request)
	if err != nil {
		fmt.Println("Error: Invalid targetPod: %s\n", err.Error())
		return
	}

	targetPV := getUserStoragePV(request)
	targetPVC := getUserStoragePVC(request)

	fmt.Printf("%v", targetPod)
	fmt.Printf("%v", targetPV)
	fmt.Printf("%v", targetPVC)
	//TODO getIngress
	//TODO copyHostkeys (in a nonblocking goroutine)
	//TODO mount pod-type-specific static PV
	//TODO mount user-specific PV

	// create pod (see getExamplePod) and return success/failure message
}

func main() {
	http.HandleFunc("/get_pods", serveGetPods)
	http.HandleFunc("/create_pod", serveCreatePod)
	http.ListenAndServe(":80", nil)
}
