package managed

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"
	"encoding/gob"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

func (u *User) ListPods() ([]Pod, error) {
	var pods []Pod
	podList, err := u.Client.ListPods(u.GetListOptions())
	if err != nil {
		return pods, err
	}
	pods = make([]Pod, len(podList.Items))
	for i, pod := range(podList.Items) {
		pods[i] = NewExistingPod(&pod, u.Client)
	}
	return pods, nil
}

// Struct for data to cache for quick getPods responses
// podTmpFiles[key] is for /tmp/key created by the pod,
// otherResourceInfo is for data about other k8s resources related to the pod, e.g. sshport
type podCache struct {
	tokens map[string]string
	otherResourceInfo map[string]string
}

type PodInfo struct {
	PodName           string            `json:"pod_name"`
	ContainerName     string            `json:"container_name"`
	ImageName         string            `json:"image_name"`
	PodIP             string            `json:"pod_ip"`
	NodeIP            string            `json:"node_ip"`
	Owner             string            `json:"owner"`
	Age               string            `json:"age"`
	Status            string            `json:"status"`
	Url               string            `json:"url"`
	Tokens            map[string]string `json:"tokens"`
	OtherResourceInfo map[string]string `json:"k8s_pod_info"`
}

type Pod struct {
	Object *apiv1.Pod
	Owner User
	Exists bool
	Client k8sclient.K8sClient
	cache *podCache
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
	cache := &podCache{
		tokens: make(map[string]string),
		otherResourceInfo: make(map[string]string),
	}
	return Pod{
		Object: existingPod,
		Exists: true,
		Client: client,
		Owner: owner,
		cache: cache,
	}
}

func (p *Pod) getCacheFile() string {
	return fmt.Sprintf("%s/%s", p.Client.TokenDir, p.Object.Name)
}

func (p *Pod) GetPodInfo() PodInfo {
	var podInfo PodInfo
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

	err := p.loadPodCache()
	if err != nil {
		fmt.Printf("Error while loading tokens for pod %s: %s\n", p.Object.Name, err.Error())
	} else {
		podInfo.Tokens = p.cache.tokens
		podInfo.OtherResourceInfo = p.cache.otherResourceInfo
	}

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

// get the contents of /tmp/key inside the first container of the pod
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
	// read the first tokenByteLimit bytes from the buffer
	var readBytes []byte
	if stdout.Len() < tokenByteLimit {
		readBytes = stdout.Bytes()
	} else {
		readBytes = make([]byte, tokenByteLimit)
		stdout.Read(readBytes)
	}
	return string(readBytes), nil
}

// attempt fill in p.cache.podTmpFiles from the temp files in the running pod
func (p *Pod) fillAllTmpFiles(oneTry bool) {
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
			p.cache.tokens[key], err = p.getTmpFile(key)
			if err != nil {
				fmt.Printf("Error while refreshing token %s for pod %s: %s\n", key, p.Object.Name, err.Error())
			}
		} else {
			// give a new pod up to 10s to create /tmp/key before giving up
			for i := 0; i < 10; i++ {
				p.cache.tokens[key], err = p.getTmpFile(key)
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

func (p *Pod) ListServices() (*apiv1.ServiceList, error) {
	opt := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("createdForPod=%s", p.Object.Name),
	}
	return p.Client.ListServices(opt)
}

func (p *Pod) getSshPort() (string, error) {
	var sshPort int32 = 0
	serviceList, err := p.ListServices()
	if err != nil {
		return "", err
	}
	// List through all the services created for this pod
	for _, service := range serviceList.Items {
		// Find the one created for ssh
		if service.Name == fmt.Sprintf("%s-ssh", p.Object.Name) {
			for _, portEntry := range service.Spec.Ports {
				// Find the port object that points to 22 in the pod
				if portEntry.TargetPort == intstr.FromInt(22) {
					// The node port is what the client should try to connect to
					sshPort = portEntry.NodePort
					break
				}
			}
			break
		}
	}
	if sshPort == 0 {
		return "", errors.New("Ssh service nodePort not found")
	}
	return string(sshPort), nil
}

func (p *Pod) fillOtherResourceInfo() {
	// if any of k8s pod info should be copied (currently only sshPort is used), then make the directory
	if p.needsSshService() {
		sshPort, err := p.getSshPort()
		if err != nil {
			fmt.Printf("Error while copying ssh port for pod %s: %s\n", p.Object.Name, err.Error())
		} else {
			p.cache.otherResourceInfo["sshPort"] = sshPort
		}
	}
}

func (p *Pod) savePodCache() error {
	b := new(bytes.Buffer)
	e := gob.NewEncoder(b)
	// encode pod tokens into the bytes buffer
	err := e.Encode(p.cache)
	if err != nil {
		return err
	}

	// if the token file exists, delete it
	err = os.Remove(p.getCacheFile())
	if err != nil {
		// if there was an error other than that the file didn't exist
		if !os.IsNotExist(err) {
			return err
		}
	}

	// save the buffer
	err = ioutil.WriteFile(p.getCacheFile(), b.Bytes(), 0600)
	if err != nil {
		return err
	}
	return nil
}

func (p *Pod) loadPodCache() error {
	// create an io.Reader for the file
	file, err := os.Open(p.getCacheFile())
	if err != nil {
		return err
	}

	d := gob.NewDecoder(file)
	// decode the file's contents into p.cache
	err = d.Decode(p.cache)
	if err != nil {
		return err
	}

	return nil
}
