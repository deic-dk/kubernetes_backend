package server

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

	"github.com/deic.dk/user_pods_k8s_backend/util"
	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	watch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)


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
	Tokens        map[string]string `json:"tokens"`
	K8sPodInfo    map[string]string `json:"k8s_pod_info"`
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

type DeleteAllPodsRequest struct {
	UserID   string `json:"user_id"`
	RemoteIP string
}

type DeleteAllPodsResponse struct {
	PodNames []string `json:"pod_names"`
}

type podGetter struct {
	Client *k8sclient.K8sClient
}

type Server struct {
	Client *k8sclient.K8sClient
	Getter *podGetter
}

func New(client *k8sclient.K8sClient) *Server {
	return &Server{
		Client: client,
		Getter: &podGetter{Client: client},
	}
}

// Fills in a getPodsResponse with information about all the pods owned by the user.
// If the username string is empty, use all pods in the namespace.
func (g *podGetter) getPods(userID string) ([]getPodsResponse, error) {
	var response []getPodsResponse
	user := managed.NewUser(userID, g.Client)
	podList, err := user.GetPodList()
	if err != nil {
		return response, err
	}
	for _, existingPod := range podList.Items {
		podInfo := fillPodResponse(existingPod)
		response = append(response, podInfo)
	}
	return response, nil
}

func (g *podGetter) fillPodResponse(existingPod apiv1.Pod) getPodsResponse {
	var podInfo getPodsResponse
	var ageSec float64
	var startTimeStr string
	// existingPod.Status.StartTime might not exist yet. Check to avoid panic
	startTime := existingPod.Status.StartTime
	if startTime == nil {
		ageSec = 0
	} else {
		ageSec = time.Now().Sub(startTime.Time).Seconds()
		startTimeStr = existingPod.Status.StartTime.Format("2006-01-02T15:04:05Z")
	}
	podInfo.Age = fmt.Sprintf("%d:%d:%d", int32(ageSec/3600), int32(ageSec/60)%60, int32(ageSec)%60)
	podInfo.ContainerName = existingPod.Spec.Containers[0].Name
	podInfo.ImageName = existingPod.Spec.Containers[0].Image
	podInfo.NodeIP = existingPod.Status.HostIP
	podInfo.Owner = getUserID(existingPod.ObjectMeta.Labels["user"], existingPod.ObjectMeta.Labels["domain"])
	podInfo.PodIP = existingPod.Status.PodIP
	podInfo.PodName = existingPod.Name
	podInfo.Status = fmt.Sprintf("%s:%s", existingPod.Status.Phase, startTimeStr)

	// Initialize the tokens map so it can be written into in the following block
	podInfo.Tokens = make(map[string]string)
	for key, value := range existingPod.ObjectMeta.Annotations {
		// If this key is specified in the manifest to be copied from /tmp/key and shown to the user in the frontend
		if value == "copyForFrontend" {
			filename := fmt.Sprintf("%s/%s", getPodTokenDir(existingPod.Name), key)
			content, err := ioutil.ReadFile(filename)
			if err != nil {
				// If the file is missing, just let the key be absent for the user, but log that it occurred
				fmt.Printf("Couldn't copy tokens (maybe not ready yet) from file %s: %s\n", filename, err.Error())
				continue
			}
			podInfo.Tokens[key] = string(content)
		}
	}

	k8sPodInfo, err := fillK8sPodInfo(existingPod)
	if err != nil {
		fmt.Printf("Couldn't copy k8s pod info for pod %s: %s\n", existingPod.Name, err.Error())
	}
	podInfo.K8sPodInfo = k8sPodInfo

	// TODO get url from ingress
	return podInfo
}

