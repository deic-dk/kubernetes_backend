package managed

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type User struct {
	UserID       string
	Name         string
	Domain       string
	Client       k8sclient.K8sClient
	GlobalConfig util.GlobalConfig
}

func NewUser(userID string, client k8sclient.K8sClient, globalConfig util.GlobalConfig) User {
	if userID == "" {
		return User{Client: client}
	}

	name, domain, _ := strings.Cut(userID, "@")
	return User{
		UserID:       userID,
		Name:         name,
		Domain:       domain,
		Client:       client,
		GlobalConfig: globalConfig,
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
	for i := 0; i < len(podList.Items); i++ {
		pod := NewPod(&podList.Items[i], u.Client, u.GlobalConfig)
		if pod.Owner.UserID == u.UserID {
			pods[i] = pod
		} else {
			fmt.Printf("Warning: pod %s has labels matching userID %s, but userID %s was expected\n", pod.Object.Name, pod.Owner.UserID, u.UserID)
		}
	}
	return pods, nil
}

func (u *User) OwnsPod(podName string) (bool, error) {
	opt := u.GetListOptions()
	opt.FieldSelector = fmt.Sprintf("metadata.name=%s", podName)
	podList, err := u.Client.ListPods(opt)
	if err != nil {
		return false, err
	}
	return (len(podList.Items) > 0), nil
}

// Make a unique string to identify userID in api objects
func (u *User) GetUserString() string {
	userString := strings.Replace(u.UserID, "@", "-", -1)
	userString = strings.Replace(userString, ".", "-", -1)
	return userString
}

// Return a unique name for the user's nfs storage PV and PVC (same name used for both)
func (u *User) GetStoragePVName() string {
	return fmt.Sprintf("user-storage-%s", u.GetUserString())
}

// Return list options for finding the user's PV and PVC (since they have the same name)
func (u *User) GetStorageListOptions() metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: fmt.Sprintf("name=%s", u.GetStoragePVName())}
}

func (u *User) getNfsStoragePath() string {
	return fmt.Sprintf("%s/%s", u.GlobalConfig.NfsStorageRoot, u.UserID)
}

// Generate an api object for the PV to attempt to create for the user's nfs storage
func (u *User) GetTargetStoragePV(nfsIP string) *apiv1.PersistentVolume {
	return &apiv1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: u.GetStoragePVName(),
			Labels: map[string]string{
				"name":   u.GetStoragePVName(),
				"user":   u.Name,
				"domain": u.Domain,
				"server": nfsIP,
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
					Server: nfsIP,
					Path:   u.getNfsStoragePath(),
				},
			},
			ClaimRef: &apiv1.ObjectReference{
				Namespace: u.GlobalConfig.Namespace,
				Name:      u.GetStoragePVName(),
				Kind:      "PersistentVolumeClaim",
			},
			Capacity: apiv1.ResourceList{
				apiv1.ResourceStorage: resource.MustParse("10Gi"),
			},
		},
	}
}

// Generate an api object for the PVC to attempt to create for the user's nfs storage
func (u *User) GetTargetStoragePVC(nfsIP string) *apiv1.PersistentVolumeClaim {
	return &apiv1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: u.GlobalConfig.Namespace,
			Name:      u.GetStoragePVName(),
			Labels: map[string]string{
				"name":   u.GetStoragePVName(),
				"user":   u.Name,
				"domain": u.Domain,
				"server": nfsIP,
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

// Delete the user's storage PV and PVC
func (u *User) DeleteUserStorage(finished *util.ReadyChannel) error {
	pvName := u.GetStoragePVName()
	// Start a watcher for PV deletion,
	pvChan := util.NewReadyChannel(u.GlobalConfig.TimeoutDelete)
	// Then try to delete the PV.
	err := u.Client.DeletePV(pvName)
	// If there is an error,
	if err != nil {
		// If the error message is that the PV is not found, that's okay. Signal that the PV is in the desired state.
		if regexp.MustCompile(fmt.Sprintf("\"%s\" not found", pvName)).MatchString(err.Error()) {
			pvChan.Send(true)
		} else { // If the error message is something else, there's a problem that should be handled.
			return err
		}
	} else { // if the delete request was issued successfully, then listen log the result
		go func() {
			u.Client.WatchDeletePV(pvName, pvChan)
			if pvChan.Receive() {
				fmt.Printf("Deleted PV %s\n", pvName)
			} else {
				fmt.Printf("Warning: failed to delete PV %s\n", pvName)
			}
		}()
	}

	// Repeat for the PVC
	pvcChan := util.NewReadyChannel(u.GlobalConfig.TimeoutDelete)
	err = u.Client.DeletePVC(pvName)
	if err != nil {
		if regexp.MustCompile(fmt.Sprintf("\"%s\" not found", pvName)).MatchString(err.Error()) {
			pvcChan.Send(true)
		} else {
			return err
		}
	} else {
		go func() {
			u.Client.WatchDeletePVC(pvName, pvcChan)
			if pvcChan.Receive() {
				fmt.Printf("Deleted PVC %s\n", pvName)
			} else {
				fmt.Printf("Warning: failed to delete PVC %s\n", pvName)
			}
		}()
	}

	// Then combine the channels so `finished` will see when both PV and PVC are deleted
	util.CombineReadyChannels([]*util.ReadyChannel{pvChan, pvcChan}, finished)
	return nil
}

// Check that the PV and PVC for the user's nfs storage exist and create them if not
func (u *User) CreateUserStorageIfNotExist(ready *util.ReadyChannel, nfsIP string) error {
	listOptions := u.GetStorageListOptions()
	PVready := util.NewReadyChannel(u.GlobalConfig.TimeoutCreate)
	PVCready := util.NewReadyChannel(u.GlobalConfig.TimeoutCreate)
	PVList, err := u.Client.ListPV(listOptions)
	if err != nil {
		return err
	}
	if len(PVList.Items) == 0 {
		targetPV := u.GetTargetStoragePV(nfsIP)
		go func() {
			u.Client.WatchCreatePV(targetPV.Name, PVready)
			if PVready.Receive() {
				fmt.Printf("Ready PV %s\n", targetPV.Name)
			} else {
				fmt.Printf("Warning PV %s didn't reach ready state\n", targetPV.Name)
			}
		}()
		_, err := u.Client.CreatePV(targetPV)
		if err != nil {
			return err
		}
	} else {
		PVready.Send(true)
	}

	PVCList, err := u.Client.ListPVC(listOptions)
	if err != nil {
		return err
	}
	if len(PVCList.Items) == 0 {
		targetPVC := u.GetTargetStoragePVC(nfsIP)
		go func() {
			u.Client.WatchCreatePVC(targetPVC.Name, PVCready)
			if PVCready.Receive() {
				fmt.Printf("Ready PVC %s\n", targetPVC.Name)
			} else {
				fmt.Printf("Warning PVC %s didn't reach ready state\n", targetPVC.Name)
			}
		}()
		_, err := u.Client.CreatePVC(targetPVC)
		if err != nil {
			return err
		}
	} else {
		PVCready.Send(true)
	}
	go util.CombineReadyChannels([]*util.ReadyChannel{PVready, PVCready}, ready)
	return nil
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
	Object       *apiv1.Pod
	Owner        User
	Client       k8sclient.K8sClient
	GlobalConfig util.GlobalConfig
}

func NewPod(existingPod *apiv1.Pod, client k8sclient.K8sClient, globalConfig util.GlobalConfig) Pod {
	userID := util.GetUserIDFromLabels(existingPod.ObjectMeta.Labels)
	owner := NewUser(userID, client, globalConfig)
	return Pod{
		Object:       existingPod,
		Client:       client,
		Owner:        owner,
		GlobalConfig: globalConfig,
	}
}

func (p *Pod) getCacheFilename() string {
	return fmt.Sprintf("%s/%s", p.GlobalConfig.TokenDir, p.Object.Name)
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

	cache, err := p.loadPodCache()
	if err == nil {
		podInfo.Tokens = cache.Tokens
		podInfo.OtherResourceInfo = cache.OtherResourceInfo
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
func (p *Pod) getOtherResourceInfo() map[string]string {
	otherResourceInfo := make(map[string]string)
	if p.needsSshService() {
		sshPort, err := p.getSshPort()
		if err != nil {
			fmt.Printf("Error while copying ssh port for pod %s: %s\n", p.Object.Name, err.Error())
		} else {
			otherResourceInfo["sshPort"] = sshPort
		}
	}
	// other information about related resources that should be cached for inclusion in GetPodInfo
	// should be included here

	return otherResourceInfo
}

func (p *Pod) savePodCache(cache podCache) error {
	b := new(bytes.Buffer)
	e := gob.NewEncoder(b)
	// encode pod cache into the bytes buffer
	err := e.Encode(cache)
	if err != nil {
		return err
	}

	// if the token file exists, delete it
	err = os.Remove(p.getCacheFilename())
	if err != nil {
		// if there was an error other than that the file didn't exist
		if !os.IsNotExist(err) {
			return err
		}
	}

	// save the buffer
	err = ioutil.WriteFile(p.getCacheFilename(), b.Bytes(), 0600)
	if err != nil {
		return err
	}
	return nil
}

func (p *Pod) loadPodCache() (podCache, error) {
	var cache podCache
	// create an io.Reader for the file
	file, err := os.Open(p.getCacheFilename())
	if err != nil {
		return cache, err
	}

	d := gob.NewDecoder(file)
	// decode the file's contents into `cache`
	err = d.Decode(&cache)
	if err != nil {
		return cache, err
	}

	return cache, nil
}

func (p *Pod) CheckState() {
	// reload with client.ListPods and check p.Object.Status
	// if needs user storage, check that it's present
	// if needsssh, check that it's present
	// check that cache is filled
}

func (p *Pod) DeleteAllServices(finished *util.ReadyChannel) error {
	serviceList, err := p.ListServices()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list services for pod %s", err.Error()))
	}
	if len(serviceList.Items) > 0 {
		deleteChannels := make([]*util.ReadyChannel, len(serviceList.Items))
		// For each service, call for deletion and add a watcher channel to the list of deleteChannels
		for i, service := range serviceList.Items {
			ch := util.NewReadyChannel(p.GlobalConfig.TimeoutDelete)
			deleteChannels[i] = ch
			go func() {
				p.Client.WatchDeleteService(service.Name, ch)
				if ch.Receive() {
					fmt.Printf("Deleted SVC %s\n", service.Name)
				} else {
					fmt.Printf("Warning: failed to delete SVC %s\n", service.Name)
				}
			}()
			p.Client.DeleteService(service.Name)
		}
		// Then only signal finished when each service has been deleted successfully
		util.CombineReadyChannels(deleteChannels, finished)
	} else {
		finished.Send(true)
	}
	return nil
}

func (p *Pod) RunDeleteJobsWhenReady(ready *util.ReadyChannel, finished *util.ReadyChannel) {
	// wait for the signal that delete jobs can begin
	// If ready.Receive() is false (due to timeout or failure),
	// then signal false on finished channel, and do not attempt delete jobs
	if !ready.Receive() {
		finished.Send(false)
		return
	}

	// Delete the cache file if it exists
	err := os.Remove(p.getCacheFilename())
	if err != nil {
		// if there was an error other than that the file didn't exist, log it
		if !os.IsNotExist(err) {
			fmt.Printf("Error while deleting cache for pod %s: %s\n", p.Object.Name, err.Error())
		}
	}

	// Delete all of the pod's related services
	err = p.DeleteAllServices(finished)
	if err != nil {
		fmt.Printf("Error deleting services: %s", err.Error())
		finished.Send(false)
	}
}

// Wait until each channel in requiredToStartJobs has an input,
// then if each input is true, attempt to perform all start jobs.
// send true into finishedStartJobs when all jobs finish successfully,
// or send false if any step fails
func (p *Pod) RunStartJobsWhenReady(requiredToStartJobs []*util.ReadyChannel, finishedStartJobs *util.ReadyChannel) {
	// block this function until a result is read from each channel in requiredToStartJobs
	ready := util.ReceiveReadyChannels(requiredToStartJobs)
	if !ready {
		fmt.Printf("Warning: Pod %s and/or user storage didn't reach ready state. Start jobs not attempted.\n", p.Object.Name)
		finishedStartJobs.Send(false)
		return
	}

	// Ensure no orphaned services for deleted pods with this pod's name
	cleanedOrphanedServices := util.NewReadyChannel(p.GlobalConfig.TimeoutDelete)
	err := p.DeleteAllServices(cleanedOrphanedServices)
	if err != nil {
		fmt.Printf("Error cleaning up orphaned services %s", err.Error())
		finishedStartJobs.Send(false)
		return
	}
	if !cleanedOrphanedServices.Receive() {
		fmt.Printf("Couldn't ensure orphaned services were removed for pod %s, didn't continue start jobs", p.Object.Name)
		finishedStartJobs.Send(false)
		return
	}

	// Perform start jobs here

	if p.needsSshService() {
		p.startSshService()
	}
	tokens := p.getAllTokens(false)
	otherResourceInfo := p.getOtherResourceInfo()
	err = p.savePodCache(
		podCache{
			Tokens:            tokens,
			OtherResourceInfo: otherResourceInfo,
		},
	)
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
func (p *Pod) getAllTokens(reload bool) map[string]string {
	tokenMap := make(map[string]string)
	var toCopy []string
	for key, value := range p.Object.ObjectMeta.Annotations {
		if value == "copyForFrontend" {
			toCopy = append(toCopy, key)
		}
	}
	for _, key := range toCopy {
		var err error
		var token string
		if reload {
			// if reloading tokens of pods that should already have created /tmp/key
			token, err = p.GetToken(key)
			if err != nil {
				fmt.Printf("Error while refreshing token %s for pod %s: %s\n", key, p.Object.Name, err.Error())
			}
		} else {
			// give a new pod up to 10s to create /tmp/key before giving up
			for i := 0; i < 10; i++ {
				token, err = p.GetToken(key)
				if err != nil {
					time.Sleep(1 * time.Second)
				} else {
					break
				}
			}
		}
		// if it never succeeded, log the last error message
		if err != nil {
			fmt.Printf("Error while copying token %s for pod %s: %s\n", key, p.Object.Name, err.Error())
		} else {
			// If it got the token successfully, add it to the tokenMap
			tokenMap[key] = token
		}
	}
	return tokenMap
}

// Try to copy /tmp/"key" in the created pod into /tmp into p.cache.tokens
func (p *Pod) GetToken(key string) (string, error) {
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
	if stdout.Len() < p.GlobalConfig.TokenByteLimit {
		readBytes = stdout.Bytes()
	} else {
		readBytes = make([]byte, p.GlobalConfig.TokenByteLimit)
		stdout.Read(readBytes)
	}
	return string(readBytes), nil
}

// Start the ssh service required by this pod
func (p *Pod) startSshService() error {
	// TODO could add a check here to delete an orphaned service with the name that will be created here
	targetService := p.getTargetSshService()
	_, err := p.Client.CreateService(targetService)
	if err != nil {
		return err
	}
	fmt.Printf("Created SVC %s\n", targetService.Name)
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
			ExternalIPs: []string{p.GlobalConfig.PublicIP},
		},
	}
}
