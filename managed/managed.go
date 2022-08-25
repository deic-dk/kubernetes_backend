package managed

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const tokenByteLimit = 4096

type User struct {
	UserID string
	Name   string
	Domain string
	SiloIP string
	Client k8sclient.K8sClient
}

func NewUser(userID string, siloIP string, client k8sclient.K8sClient) User {
	if userID == "" {
		return User{Client: client}
	}

	name, domain, _ := strings.Cut(userID, "@")
	return User{
		UserID: userID,
		Name:   name,
		Domain: domain,
		Client: client,
		SiloIP: siloIP,
	}
}

// Return the user's siloIP in the subnet where data can be accessed by the pods.
func (u *User) GetSiloIPDataNet() string {
	return strings.Replace(u.SiloIP, "10.0.", "10.2.", 1)
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
	for i := 0; i < len(podList.Items); i++ {
		pods[i] = NewPod(&podList.Items[i], u.Client)
	}
	return pods, nil
}

// Make a unique string to identify userID in api objects
func (u *User) GetUserString() string {
	userString := strings.Replace(u.UserID, "@", "-", -1)
	userString = strings.Replace(userString, ".", "-", -1)
	return userString
}

// Return a unique name for the user's /tank/storage PV and PVC (same name used for both)
func (u *User) GetStoragePVName() string {
	return fmt.Sprintf("user-storage-%s", u.GetUserString())
}

// Return list options for finding the user's PV and PVC (since they have the same name)
func (u *User) GetStorageListOptions() metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: fmt.Sprintf("name=%s", u.GetStoragePVName())}
}

// Generate an api object for the PV to attempt to create for the user's /tank/storage
func (u *User) GetTargetStoragePV() *apiv1.PersistentVolume {
	return &apiv1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: u.GetStoragePVName(),
			Labels: map[string]string{
				"name":   u.GetStoragePVName(),
				"user":   u.Name,
				"domain": u.Domain,
				"server": u.SiloIP,
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
					Server: u.SiloIP,
					Path:   fmt.Sprintf("/tank/storage/%s", u.UserID),
				},
			},
			ClaimRef: &apiv1.ObjectReference{
				Namespace: u.Client.Namespace,
				Name:      u.GetStoragePVName(),
				Kind:      "PersistentVolumeClaim",
			},
			Capacity: apiv1.ResourceList{
				apiv1.ResourceStorage: resource.MustParse("10Gi"),
			},
		},
	}
}

// Generate an api object for the PVC to attempt to create for the user's /tank/storage
func (u *User) GetTargetStoragePVC() *apiv1.PersistentVolumeClaim {
	return &apiv1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: u.Client.Namespace,
			Name:      u.GetStoragePVName(),
			Labels: map[string]string{
				"name":   u.GetStoragePVName(),
				"user":   u.UserID,
				"domain": u.Domain,
				"server": u.SiloIP,
			},
		},
		Spec: apiv1.PersistentVolumeClaimSpec{
			//			StorageClassName: "nfs",
			AccessModes: []apiv1.PersistentVolumeAccessMode{
				"ReadWriteMany",
			},
			VolumeName: u.GetStoragePVName(),
			Resources: apiv1.ResourceRequirements{
				Requests: apiv1.ResourceList{
					apiv1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
	}
}

// Struct for data to cache for quick getPods responses
// podTmpFiles[key] is for /tmp/key created by the pod,
// otherResourceInfo is for data about other k8s resources related to the pod, e.g. sshport
type podCache struct {
	Tokens            map[string]string
	OtherResourceInfo map[string]string
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
	Owner  User
	Client k8sclient.K8sClient
	cache  *podCache
}

func NewPod(existingPod *apiv1.Pod, client k8sclient.K8sClient) Pod {
	var owner User
	user, hasUser := existingPod.ObjectMeta.Labels["user"]
	if !hasUser {
		owner = NewUser("", "", client)
	} else {
		var userStr string
		domain, hasDomain := existingPod.ObjectMeta.Labels["domain"]
		if hasDomain {
			userStr = fmt.Sprintf("%s@%s", user, domain)
		} else {
			userStr = user
		}
		owner = NewUser(userStr, "", client)
	}
	cache := &podCache{
		Tokens:            make(map[string]string),
		OtherResourceInfo: make(map[string]string),
	}
	return Pod{
		Object: existingPod,
		Client: client,
		Owner:  owner,
		cache:  cache,
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
	if err == nil {
		podInfo.Tokens = p.cache.Tokens
		podInfo.OtherResourceInfo = p.cache.OtherResourceInfo
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

// attempt fill in p.cache.podTmpFiles[key] for each file /tmp/key in the running pod's first container
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
			p.cache.Tokens[key], err = p.getTmpFile(key)
			if err != nil {
				fmt.Printf("Error while refreshing token %s for pod %s: %s\n", key, p.Object.Name, err.Error())
			}
		} else {
			// give a new pod up to 10s to create /tmp/key before giving up
			for i := 0; i < 10; i++ {
				p.cache.Tokens[key], err = p.getTmpFile(key)
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
	return fmt.Sprintf("%d", sshPort), nil
}

// fill p.cache.OtherResourceInfo with information about other k8s resources relevant to the pod
func (p *Pod) fillOtherResourceInfo() {
	if p.needsSshService() {
		sshPort, err := p.getSshPort()
		if err != nil {
			fmt.Printf("Error while copying ssh port for pod %s: %s\n", p.Object.Name, err.Error())
		} else {
			p.cache.OtherResourceInfo["sshPort"] = sshPort
		}
	}
	// other information about related resources that should be cached for inclusion in GetPodInfo
	// should be included here
}

func (p *Pod) savePodCache() error {
	b := new(bytes.Buffer)
	e := gob.NewEncoder(b)
	// encode pod cache into the bytes buffer
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

func (p *Pod) CheckState() {
	// reload with client.ListPods and check p.Object.Status
	// if needs user storage, check that it's present
	// if needsssh, check that it's present
	// check that cache is filled
}

func (p *Pod) WatchForReady() {
	// watch for pod status
}

// Wait until each channel in requiredToStartJobs has an input,
// then if each input is true, attempt to perform all start jobs.
// send true into finishedStartJobs when all jobs finish successfully,
// or send false if any step fails
func (p *Pod) RunStartJobsWhenReady(requiredToStartJobs []*util.ReadyChannel, finishedStartJobs *util.ReadyChannel) {
	// block this function until a result is read from each channel in requiredToStartJobs
	ready := util.ReceiveReadyChannels(requiredToStartJobs)
	if !ready {
		fmt.Printf("Pod %s, related services, and/or user storage didn't reach ready state. Start jobs not attempted.\n", p.Object.Name)
		finishedStartJobs.Send(false)
		return
	}

	// Perform start jobs here
	if p.needsSshService() {
		p.startSshService()
	}
	p.copyAllTokens(false)
	p.fillOtherResourceInfo()
	err := p.savePodCache()
	if err != nil {
		fmt.Printf("Failed to save pod cache for pod %s: %s\n", p.Object.Name, err.Error())
		finishedStartJobs.Send(false)
		return
	}

	// TODO ingress

	finishedStartJobs.Send(true)
}

// for each pod.metadata.annotations[key]=="copyForFrontend",
// copy the token from the pod held in /tmp/key to the filesystem, ready to be served by getPods.
// If reload is true, it will only attempt each token once,
// otherwise, it will try a few times to give the pod time to create /tmp/key after starting
func (p *Pod) copyAllTokens(reload bool) {
	var toCopy []string
	for key, value := range p.Object.ObjectMeta.Annotations {
		if value == "copyForFrontend" {
			toCopy = append(toCopy, key)
		}
	}
	for _, key := range toCopy {
		var err error
		if reload {
			// if reloading tokens of pods that should already have created /tmp/key
			err = p.copyToken(key)
			if err != nil {
				fmt.Printf("Error while refreshing token %s for pod %s: %s\n", key, p.Object.Name, err.Error())
			}
		} else {
			// give a new pod up to 10s to create /tmp/key before giving up
			for i := 0; i < 10; i++ {
				err = p.copyToken(key)
				if err != nil {
					time.Sleep(1 * time.Second)
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

// Try to copy /tmp/"key" in the created pod into /tmp into p.cache.tokens
func (p *Pod) copyToken(key string) error {
	var stdout, stderr bytes.Buffer
	var err error
	stdout, stderr, err = p.Client.PodExec([]string{"cat", fmt.Sprintf("/tmp/%s", key)}, p.Object, 0)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't call pod exec for pod %s: %s", p.Object.Name, err.Error()))
	}
	if stdout.Len() == 0 {
		return errors.New(fmt.Sprintf("Empty response. Stderr: %s", stderr.String()))
	}
	// read the first tokenByteLimit bytes from the buffer
	var readBytes []byte
	if stdout.Len() < tokenByteLimit {
		readBytes = stdout.Bytes()
	} else {
		readBytes = make([]byte, tokenByteLimit)
		stdout.Read(readBytes)
	}
	p.cache.Tokens[key] = string(readBytes)
	return nil
}

// Start the ssh service required by this pod
func (p *Pod) startSshService() error {
	// TODO could add a check here to delete an orphaned service with the name that will be created here
	_, err := p.Client.CreateService(p.getTargetSshService())
	if err != nil {
		return err
	}
	return nil
}

// Get a target service object that will provide ssh port forwarding for this pod
func (p *Pod) getTargetSshService() *apiv1.Service {
	return &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-ssh", p.Object.Name),
			Labels: map[string]string{
				"createdForPod": p.Object.Name,
			},
		},
		Spec: apiv1.ServiceSpec{
			Ports: []apiv1.ServicePort{
				{
					Name:       "ssh",
					Protocol:   apiv1.ProtocolTCP,
					Port:       22,
					TargetPort: intstr.FromInt(22),
				},
			},
			Type:        apiv1.ServiceTypeLoadBalancer,
			Selector:    p.Object.ObjectMeta.Labels,
			ExternalIPs: []string{p.Client.PublicIP},
		},
	}
}
