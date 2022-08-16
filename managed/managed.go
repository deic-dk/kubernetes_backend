package managed

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

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/server"
	"github.com/deic.dk/user_pods_k8s_backend/util"
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

const tokenByteLimit = 4096

type User struct {
	UserID string
	Name string
	Domain string
	Client k8sclient.K8sClient
}

func NewUser(userID string, client k8sclient.K8sClient) User {
	if userID == "" {
		return User{Client: client}
	}

	name, domain, _ := strings.Cut(userID, "@")
	return User{
		UserID: userID,
		Name: name,
		Domain: domain,
		Client: client,
	}
}

func (u *User) GetListOptions() metav1.ListOptions {
	opt := metav1.ListOptions{}
	if u.UserID != "" {
		opt.LabelSelector = fmt.Sprintf("user=%s,domain=%s", u.Name, u.Domain)
	}
	return opt
}

func (u *User) GetPodList() (*apiv1.PodList, error) {
	return u.Client.ListPods(u.GetListOptions())
}

// Struct for data to cache for quick getPods responses
// podTmpFiles[key] is for /tmp/key created by the pod,
// k8sPodInfo is for data about api objects relevant for the pod, e.g. sshport
type podTokens struct {
	podTmpFiles map[string]string
	k8sPodInfo map[string]string
}

type Pod struct {
	Object *apiv1.Pod
	Owner User
	Exists bool
	Client k8sclient.K8sClient
	tokens podTokens
}

func NewExistingPod(existingPod *apiv1.Pod, client k8sclient.K8sClient) Pod {
	var owner User
	user, hasUser := existingPod.ObjectMeta.Labels["user"]
	if !hasUser {
		owner = NewUser("", client)
	} else {
		var userStr string
		domain, hasDomain := existingPod.ObjectMeta.Labels["domain"]
		if hasDomain {
			userStr = fmt.Sprintf("%s@%s", user, domain)
		} else {
			userStr = user
		}
		owner = NewUser(userStr, client)
	}
	return Pod{
		Object: existingPod,
		Exists: true,
		Client: client,
		Owner: owner,
	}
}

func (p *Pod) getTokenDir() string {
	return fmt.Sprintf("%s/%s", p.Client.TokenDir, p.Object.Name)
}

func (p *Pod) getInfoDir() string {
	return fmt.Sprintf("%s/%s", p.Client.InfoDir, p.Object.Name)
}

func (p *Pod) getPodInfo() server.GetPodsResponse {
	var podInfo server.GetPodsResponse
	var ageSec float64
	var startTimeStr string
	// p.Object.Status.StartTime might not exist yet. Check to avoid panic
	startTime := p.Object.Status.StartTime
	if startTime == nil {
		ageSec = 0
	} else {
		ageSec = time.Now().Sub(startTime.Time).Seconds()
		startTimeStr = p.Object.Status.StartTime.Format("2006-01-02T15:04:05Z")
	}
	podInfo.Age = fmt.Sprintf("%d:%d:%d", int32(ageSec/3600), int32(ageSec/60)%60, int32(ageSec)%60)
	podInfo.ContainerName = p.Object.Spec.Containers[0].Name
	podInfo.ImageName = p.Object.Spec.Containers[0].Image
	podInfo.NodeIP = p.Object.Status.HostIP
	podInfo.Owner = p.Owner.UserID
	podInfo.PodIP = p.Object.Status.PodIP
	podInfo.PodName = p.Object.Name
	podInfo.Status = fmt.Sprintf("%s:%s", p.Object.Status.Phase, startTimeStr)

	// Initialize the tokens map so it can be written into in the following block
	podInfo.Tokens = make(map[string]string)
	for key, value := range p.Object.ObjectMeta.Annotations {
		// If this key is specified in the manifest to be copied from /tmp/key and shown to the user in the frontend
		if value == "copyForFrontend" {
			filename := fmt.Sprintf("%s/%s", p.getTokenDir(), key)
			content, err := ioutil.ReadFile(filename)
			if err != nil {
				// If the file is missing, just let the key be absent for the user, but log that it occurred
				fmt.Printf("Couldn't copy tokens (maybe not ready yet) from file %s: %s\n", filename, err.Error())
				continue
			}
			podInfo.Tokens[key] = string(content)
		}
	}

	k8sPodInfo, err := fillK8sPodInfo(p.Object)
	if err != nil {
		fmt.Printf("Couldn't copy k8s pod info for pod %s: %s\n", p.Object.Name, err.Error())
	}
	podInfo.K8sPodInfo = k8sPodInfo

	// TODO get url from ingress
	return podInfo
}

func (p *Pod) needsSshService() bool {
	listensSsh := false
	for _, container := range p.Object.Spec.Containers {
		for _, port := range container.Ports {
			if port.ContainerPort == 22 {
				listensSsh = true
				break
			}
		}
	}
	return listensSsh
}

// get the
func (p *Pod) getTmpFile(key string) (string, error) {
	var stdout, stderr bytes.Buffer
	var err error
	stdout, stderr, err = p.Client.PodExec([]string{"cat", fmt.Sprintf("/tmp/%s", key)}, p.Object, 0)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Couldn't call pod exec for pod %s: %s", p.Object.Name, err.Error()))
	}
	if stdout.Len() == 0 {
		return "", errors.New(fmt.Sprintf("Empty response. Stderr: %s", stderr.String()))
	}
	// read the first tokenByteLimit bytes from the buffer, and write it to the file
	var readBytes []byte
	if stdout.Len() < tokenByteLimit {
		readBytes = stdout.Bytes()
	} else {
		readBytes = make([]byte, tokenByteLimit)
		stdout.Read(readBytes)
	}
	return string(readBytes), nil
}

// attempt fill in p.tokens.podTmpFiles from the temp files in the running pod
func (p *Pod) fillAllTokens(oneTry bool) {
	var toCopy []string
	for key, value := range p.Object.ObjectMeta.Annotations {
		if value == "copyForFrontend" {
			toCopy = append(toCopy, key)
		}
	}
	for _, key := range toCopy {
		var err error
		if oneTry {
			// if reloading tokens of pods that should already have created /tmp/key
			p.tokens.podTmpFiles[key], err = p.getTmpFile(key)
			if err != nil {
				fmt.Printf("Error while refreshing token %s for pod %s: %s\n", key, p.Object.Name, err.Error())
			}
		} else {
			// give a new pod up to 10s to create /tmp/key before giving up
			for i := 0; i < 10; i++ {
				p.tokens.podTmpFiles[key], err = p.getTmpFile(key)
				if err != nil {
					time.Sleep(time.Second)
					continue
				} else {
					break
				}
			}
			// if it never succeeded, log the last error message
			if err != nil {
				fmt.Printf("Error while copying token %s for pod %s: %s\n", key, p.Object.Name, err.Error())
			}
		}
	}
}
