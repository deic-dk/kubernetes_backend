package managed

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	apiv1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// User

type User struct {
	UserID       string
	Name         string
	Domain       string
	Client       k8sclient.K8sClient
	GlobalConfig util.GlobalConfig
}

func NewUser(userID string, client k8sclient.K8sClient, globalConfig util.GlobalConfig) User {
	if userID == "" {
		panic("UserID cannot be empty when creating user")
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
	opt.LabelSelector = fmt.Sprintf("user=%s,domain=%s", u.Name, u.Domain)
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

// Pod

const ingressPortAnnotation = "sciencedata.dk/ingress-port"

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
	SshUrl            string            `json:"ssh_url"`
	Tokens            map[string]string `json:"tokens"`
	OtherResourceInfo map[string]string `json:"k8s_pod_info"`
}

type Pod struct {
	Object       *apiv1.Pod
	Owner        User
	Client       k8sclient.K8sClient
	GlobalConfig util.GlobalConfig
	ingressPort  int32
}

func NewPod(existingPod *apiv1.Pod, client k8sclient.K8sClient, globalConfig util.GlobalConfig) Pod {
	userID := util.GetUserIDFromLabels(existingPod.ObjectMeta.Labels)
	var owner User
	if userID != "" {
		owner = NewUser(userID, client, globalConfig)
	}
	return Pod{
		Object:       existingPod,
		Client:       client,
		Owner:        owner,
		GlobalConfig: globalConfig,
	}
}

func (p *Pod) GetCacheFilename() string {
	return fmt.Sprintf("%s/%s", p.GlobalConfig.PodCacheDir, p.Object.Name)
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

	if p.NeedsIngress() {
		podInfo.Url = fmt.Sprintf("https://%s", p.getIngressHost())
	}

	cache, err := p.loadPodCache()
	if err == nil {
		podInfo.Tokens = cache.Tokens
		podInfo.OtherResourceInfo = cache.OtherResourceInfo
		sshPort, hasSshPort := cache.OtherResourceInfo["sshPort"]
		if hasSshPort {
			podInfo.SshUrl = p.getSshUrl(sshPort)
		}
	}

	return podInfo
}

// Generate the url by which a user can access the pod via ssh
// Note that `port` must be an integer represented as a string,
// which is assumed to be the case following from Pod.getSshPort
func (p *Pod) getSshUrl(port string) string {
	// Could get username from an env variable set in the manifest. For now only root ubuntu pods use ssh.
	return fmt.Sprintf("ssh://root@%s:%s", p.GlobalConfig.IngressDomain, port)
}

func (p *Pod) NeedsSshService() bool {
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

func (p *Pod) labelSelectOptions() metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: fmt.Sprintf("createdForPod=%s", p.Object.Name),
	}
}

func (p *Pod) ListServices() (*apiv1.ServiceList, error) {
	return p.Client.ListServices(p.labelSelectOptions())
}

func (p *Pod) ListIngresses() (*netv1.IngressList, error) {
	return p.Client.ListIngresses(p.labelSelectOptions())
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
	if p.NeedsSshService() {
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
	err = os.Remove(p.GetCacheFilename())
	if err != nil {
		// if there was an error other than that the file didn't exist
		if !os.IsNotExist(err) {
			return err
		}
	}

	// save the buffer
	err = ioutil.WriteFile(p.GetCacheFilename(), b.Bytes(), 0600)
	if err != nil {
		return err
	}
	return nil
}

func (p *Pod) loadPodCache() (podCache, error) {
	var cache podCache
	// create an io.Reader for the file
	file, err := os.Open(p.GetCacheFilename())
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

func (p *Pod) DeleteAllServices(finished *util.ReadyChannel) error {
	serviceList, err := p.ListServices()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list services: %s", err.Error()))
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

func (p *Pod) DeleteAllIngresses() error {
	ingressList, err := p.ListIngresses()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list ingresses: %s", err.Error()))
	}
	for _, ing := range ingressList.Items {
		err = p.Client.DeleteIngress(ing.Name)
		if err != nil {
			return errors.New(fmt.Sprintf("Failed to delete ingress: %s", err.Error()))
		}
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
	err := os.Remove(p.GetCacheFilename())
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

	err = p.DeleteAllIngresses()
	if err != nil {
		fmt.Printf("Error deleting ingresses: %s", err.Error())
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

	// Ensure no orphaned services or ingresses for deleted pods with this pod's name
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
	err = p.DeleteAllIngresses()
	if err != nil {
		fmt.Printf("Error cleaning up orphaned ingresses %s", err.Error())
	}

	// Perform start jobs here

	if p.NeedsSshService() {
		p.startSshService()
	}

	if p.NeedsIngress() {
		p.createIngress()
	}

	err = p.CreateAndSavePodCache(false)
	if err != nil {
		fmt.Printf("Failed to save pod cache for pod %s: %s\n", p.Object.Name, err.Error())
		finishedStartJobs.Send(false)
		return
	}

	finishedStartJobs.Send(true)
}

func (p *Pod) CreateAndSavePodCache(reload bool) error {
	tokens := p.getAllTokens(reload)
	otherResourceInfo := p.getOtherResourceInfo()
	return p.savePodCache(
		podCache{
			Tokens:            tokens,
			OtherResourceInfo: otherResourceInfo,
		},
	)
}

// for each comma-separated token key in pod.metadata.annotations["sciencedata.dk/copy-token"],
// copy the token from the pod held in /tmp/key to the filesystem, ready to be served by getPods.
// If reload is true, it will only attempt each token once,
// otherwise, it will try a few times to give the pod time to create /tmp/key after starting
func (p *Pod) getAllTokens(reload bool) map[string]string {
	tokenMap := make(map[string]string)
	keys, has := p.Object.ObjectMeta.Annotations["sciencedata.dk/copy-token"]
	// If the copy-token annotiation doesn't exist
	if !has {
		return tokenMap
	}
	// Get a list of tokens to attempt to copy
	toCopy := strings.Split(keys, ",")

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
			Type:     apiv1.ServiceTypeNodePort,
			Selector: p.Object.ObjectMeta.Labels,
		},
	}
}

// Checks whether an ingress should be created for this pod based on the annotation in its manifest
func (p *Pod) NeedsIngress() bool {
	portStr, hasKey := p.Object.ObjectMeta.Annotations[ingressPortAnnotation]
	if hasKey {
		portInt, err := strconv.ParseInt(portStr, 10, 32)
		if err != nil {
			fmt.Printf("Warning: Couldn't parse ingress-port annotation for pod %s, skipping ingress", p.Object.Name)
			return false
		}
		p.ingressPort = int32(portInt)
	}
	return hasKey
}

func (p *Pod) createIngress() error {
	// First create the service that the ingress should route to
	targetService := p.getTargetHttpService()
	_, err := p.Client.CreateService(targetService)
	if err != nil {
		return err
	}
	fmt.Printf("Created SVC %s\n", targetService.Name)

	// Then create the ingress
	targetIngress := p.getTargetIngress()
	_, err = p.Client.CreateIngress(targetIngress)
	if err != nil {
		fmt.Printf("Ingress error: %s\n", err.Error())
		return err
	}
	fmt.Printf("Created ING %s\n", targetIngress.Name)

	return nil
}

// Get a target service object that will forward http traffic to this pod
// An ingress will route traffic to it
func (p *Pod) getTargetHttpService() *apiv1.Service {
	return &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-http", p.Object.Name),
			Labels: map[string]string{
				"createdForPod": p.Object.Name,
			},
		},
		Spec: apiv1.ServiceSpec{
			Ports: []apiv1.ServicePort{
				{
					Name:       "http",
					Protocol:   apiv1.ProtocolTCP,
					Port:       p.ingressPort,
					TargetPort: intstr.FromInt(int(p.ingressPort)),
				},
			},
			Type:     apiv1.ServiceTypeClusterIP,
			Selector: p.Object.ObjectMeta.Labels,
		},
	}
}

// Get a target ingress object to route http traffic to this pod
func (p *Pod) getTargetIngress() *netv1.Ingress {
	pathType := netv1.PathTypePrefix
	return &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-ingress", p.Object.Name),
			Labels: map[string]string{
				"createdForPod": p.Object.Name,
			},
		},
		Spec: netv1.IngressSpec{
			TLS: []netv1.IngressTLS{
				{
					Hosts:      []string{p.getIngressHost()},
					SecretName: p.GlobalConfig.IngressWildcardSecret,
				},
			},
			Rules: []netv1.IngressRule{
				{
					Host: p.getIngressHost(),
					IngressRuleValue: netv1.IngressRuleValue{
						HTTP: &netv1.HTTPIngressRuleValue{
							Paths: []netv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: netv1.IngressBackend{
										Service: &netv1.IngressServiceBackend{
											Name: fmt.Sprintf("%s-http", p.Object.Name),
											Port: netv1.ServiceBackendPort{
												Number: p.ingressPort,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// Function for deriving the URL for routing traffic to the pod.
// For now, we can just use the pod name since it is url-compatible,
// unique, and specific to the user, but we could decide to define
// it differently
func (p *Pod) getIngressHost() string {
	return fmt.Sprintf("%s.%s", p.Object.Name, p.GlobalConfig.IngressDomain)
}
