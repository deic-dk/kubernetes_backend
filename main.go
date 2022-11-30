package main

import (
	"fmt"
	"net/http"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/server"
	"github.com/deic.dk/user_pods_k8s_backend/util"
)

func main() {
	globalConfig := util.MustLoadGlobalConfig()
	k8sClient := k8sclient.NewK8sClient(globalConfig)
	server := server.New(k8sClient, globalConfig)
	server.ReloadPodCaches()

	http.HandleFunc("/get_pods", server.ServeGetPods)
	http.HandleFunc("/create_pod", server.ServeCreatePod)
	http.HandleFunc("/watch_create_pod", server.ServeWatchCreatePod)
	http.HandleFunc("/delete_pod", server.ServeDeletePod)
	http.HandleFunc("/watch_delete_pod", server.ServeWatchDeletePod)
	http.HandleFunc("/delete_all_user", server.ServeDeleteAllUserPods)
	http.HandleFunc("/clean_all_unused", server.ServeCleanAllUnused)
	http.HandleFunc("/get_podip_owner", server.ServeGetPodIPOwner)

	fmt.Printf("Listening\n")
	err := http.ListenAndServe(":80", nil)
	if err != nil {
		panic(fmt.Sprintf("Error running http server: %s\n", err.Error()))
	}
}
