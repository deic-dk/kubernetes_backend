package main

import (
	"context"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	apiv1 "k8s.io/api/core/v1"
)

// TODO figure out how to get the namespace automatically from within the pod where this runs
const ns = "sciencedata-dev"

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

func getExamplePod() *apiv1.Pod {
	return &apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "jupyter-test",
			Namespace: ns,
		},
		Spec: apiv1.PodSpec{
			Containers: []apiv1.Container{
				{
					Name: "jupyter",
					Image: "kube.sciencedata.dk:5000/jupyter_sciencedata_testing",
					Ports: []apiv1.ContainerPort{
						{
							ContainerPort: 8888,
							Protocol: apiv1.ProtocolTCP,
						},
					},
				},
			},
		},
	}
}

func hellok8() string {
	podclient := getPodClient()
	podlist, err := podclient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}
	// Try to create a pod
	pod := getExamplePod()
	result, err := podclient.Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		panic(err.Error())
	}
	fmt.Println("made " + result.ObjectMeta.Name)
	return fmt.Sprintf("%d pod(s) in sciencedata-dev\n", len(podlist.Items))
}

func helloWorld(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(hellok8()))
}

func main() {
	//	http.HandleFunc("/", helloWorld)
	//	http.ListenAndServe(":80", nil)
	fmt.Println(hellok8())
}
