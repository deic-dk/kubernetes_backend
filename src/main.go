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
	//	"k8s.io/apimachinery/pkg/runtime/schema"
	//	"k8s.io/client-go/dynamic"
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
	Settings map[string]map[string]string `json:"settings"`
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

func getPods(username string) []GetPodsResponse {
	user, domain, _ := strings.Cut(username, "@")
	opts := metav1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)}
	podclient := getPodClient()
	podlist, err := podclient.List(context.TODO(), opts)
	if err != nil {
		panic(err.Error())
	}
	var response []GetPodsResponse
	for _, n := range podlist.Items {
		var pod GetPodsResponse
		var ageSec = time.Now().Sub(n.Status.StartTime.Time).Seconds()
		pod.Age = fmt.Sprintf("%d:%d:%d", int32(ageSec/3600), int32(ageSec/60)%60, int32(ageSec)%60)
		pod.ContainerName = n.Spec.Containers[0].Name
		pod.ImageName = n.Spec.Containers[0].Image
		pod.NodeIP = n.Status.HostIP
		pod.Owner = fmt.Sprintf("%s@%s", n.ObjectMeta.Labels["user"], n.ObjectMeta.Labels["domain"])
		pod.PodIP = n.Status.PodIP
		pod.PodName = n.ObjectMeta.Name
		pod.Status = fmt.Sprintf("%s:%s", n.Status.Phase, n.Status.StartTime.Format("2006-01-02T15:04:05Z"))
		//TODO: hostkeys, url, sshurl
		response = append(response, pod)
	}
	return response
}

func helloWorld(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("hello"))
}

func serveGetPods(w http.ResponseWriter, r *http.Request) {
	data := getPods("foo@bar.baz")
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
		"user": user,
		"domain": domain,
	}
	for i, container := range pod.Spec.Containers {
		settings, exist := request.Settings[container.Name]
		// if there are settings for this container (if container.Name is a key in request.Settings)
		if exist {
			// then for each setting,
			for name, value := range settings {
				// find the env entry with a matching name, and set the value
				for ii, env := range container.Env {
					if env.Name == name {
						pod.Spec.Containers[i].Env[ii].Value = value
					}
				}
			}
		}
	}
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
	// TODO Make a unique name for targetPod
	return targetPod, nil
}

func createPod(w http.ResponseWriter, r *http.Request) {
	// Parse the POSTed request JSON and log the request
	var request CreatePodRequest
	decoder := json.NewDecoder(r.Body)
	decoder.Decode(&request)
	fmt.Printf("createPod request: %+v\n", request)

	//TODO set HOME_SERVER env var to 10.2.x.x from r.RemoteAddr and SD_UID from user_id

	targetPod, err := getTargetPod(request)
	if err != nil {
		fmt.Println("Error: Invalid targetPod: %s\n", err.Error())
		return
	}

	//TODO getIngress
	//TODO copyHostkeys (in a nonblocking goroutine)
	//TODO mount pod-type-specific static PV
	//TODO mount user-specific PV

	// create pod (see getExamplePod) and return success/failure message
}

func main() {
	http.HandleFunc("/get_pods", serveGetPods)
	http.HandleFunc("/create_pod", createPod)
	http.ListenAndServe(":80", nil)
}
