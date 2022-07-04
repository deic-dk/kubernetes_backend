package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"encoding/json"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// TODO figure out how to get the namespace automatically from within the pod where this runs
const ns = "sciencedata-dev"

type GetPodsResponse struct {
	PodName string
	ContainerName string
	ImageName string
	PodIP string
	NodeIP string
	Owner string
	Age string
	Status string
	Ed25519Hostkey string
	RsaHostkey string
	Url string
	SshUrl string
}

//type CreatePodSettings struct {
//}

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
	return clientset.CoreV1().Pods(ns)
}

func getExamplePod(name string, user string, domain string) *apiv1.Pod {
	return &apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
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

//func createPod(manifestURL string, settings struct) (*apiv1.Pod, error) {
//}

func getPods(username string) []GetPodsResponse{
	user, domain, _ := strings.Cut(username, "@")
	opts := metav1.ListOptions{LabelSelector: fmt.Sprintf("user=%s,domain=%s", user, domain)}
	podclient := getPodClient()
	podlist, err := podclient.List(context.TODO(), opts)
	if err != nil {
		panic(err.Error())
	}
	var response []GetPodsResponse
	for i, n := range podlist.Items {
		var pod GetPodsResponse
		var ageSec = time.Now().Sub(n.Status.StartTime.Time).Seconds()
		pod.Age = fmt.Sprintf("%d:%d:%d", int32(ageSec / 3600), int32(ageSec / 60) % 60, int32(ageSec) % 60)
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
	//TODO get the username from the http.Request
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

func main() {
	http.HandleFunc("/", serveGetPods)
	http.ListenAndServe(":80", nil)
}
