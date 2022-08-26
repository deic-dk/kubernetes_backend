package main

import (
	"fmt"
	"net/http"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/server"
)

func main() {
	k8sClient := k8sclient.NewK8sClient()
	server := server.New(*k8sClient)

	http.HandleFunc("/get_pods", server.ServeGetPods)
	http.HandleFunc("/create_pod", server.ServeCreatePod)
	http.HandleFunc("/watch_create_pod", server.ServeWatchCreatePod)
	http.HandleFunc("/delete_pod", server.ServeDeletePod)
	http.HandleFunc("/watch_delete_pod", server.ServeWatchDeletePod)

	fmt.Printf("Listening\n")
	err := http.ListenAndServe(":80", nil)
	if err != nil {
		panic(fmt.Sprintf("Error running http server: %s\n", err.Error()))
	}
}
