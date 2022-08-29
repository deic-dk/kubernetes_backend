package k8sclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/util"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	watch "k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Struct to wrap kubernetes client functions
type K8sClient struct {
	config        *rest.Config
	clientset     *kubernetes.Clientset
	TimeoutDelete time.Duration
	TimeoutCreate time.Duration
	Namespace     string
	TokenDir      string
	PublicIP      string
}

// initialize a new K8SClient
func NewK8sClient() *K8sClient {
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
	return &K8sClient{
		config:    config,
		clientset: clientset,
		// TODO figure out how to get the namespace automatically from within the pod where this runs
		Namespace:     "sciencedata-dev",
		TimeoutDelete: 90 * time.Second,
		TimeoutCreate: 90 * time.Second,
		TokenDir:      "/tmp/tokens",
		// TODO set this with an external config file instead of hardcoding
		PublicIP: "130.226.137.130",
	}
}

// Set up a watcher to pass to signalFunc, which should ch<-true when the desired event occurs
func (c *K8sClient) WatchFor(
	name string,
	resourceType string,
	signalFunc func(watch.Interface, *util.ReadyChannel),
	ch *util.ReadyChannel,
) {
	listOptions := metav1.ListOptions{FieldSelector: fmt.Sprintf("metadata.name=%s", name)}
	var err error
	var watcher watch.Interface
	// create a watcher for the API resource of the correct type
	switch resourceType {
	case "Pod":
		watcher, err = c.clientset.CoreV1().Pods(c.Namespace).Watch(context.TODO(), listOptions)
	case "PV":
		watcher, err = c.clientset.CoreV1().PersistentVolumes().Watch(context.TODO(), listOptions)
	case "PVC":
		watcher, err = c.clientset.CoreV1().PersistentVolumeClaims(c.Namespace).Watch(context.TODO(), listOptions)
	case "SVC":
		watcher, err = c.clientset.CoreV1().Services(c.Namespace).Watch(context.TODO(), listOptions)
	default:
		err = errors.New("Unsupported resource type for watcher")
	}
	if err != nil {
		ch.Send(false)
		fmt.Printf("Error in WatchFor: %s\n", err.Error())
		return
	}
	// In a goroutine, wait until there's a value in the channel, and then stop the watcher.
	// This will ensure that either a successful event or the timeout will terminate signalFunc
	go func() {
		ch.Receive()
		watcher.Stop()
	}()
	// In this goroutine, call the function to ch<-true when the desired event occurs
	signalFunc(watcher, ch)
}

// Push ch<-true when watcher receives an event for a ready pod
func signalPodReady(watcher watch.Interface, ch *util.ReadyChannel) {
	// Run this loop every time an event is ready in the watcher channel
	for event := range watcher.ResultChan() {
		// the event.Object is only sure to be an apiv1.Pod if the event.Type is Modified
		if event.Type == watch.Modified {
			// event.Object is a new runtime.Object with the pod in its state after the event
			eventPod := event.Object.(*apiv1.Pod)
			// Loop through the pod conditions to find the one that's "Ready"
			for _, condition := range eventPod.Status.Conditions {
				if condition.Type == apiv1.PodReady {
					// If the pod is ready, then stop watching, so the event loop will terminate
					if condition.Status == apiv1.ConditionTrue {
						ch.Send(true)
					}
					break
				}
			}
		}
	}
}

// Push ch<-true when the object watcher is watching is deleted
func signalDeleted(watcher watch.Interface, ch *util.ReadyChannel) {
	for event := range watcher.ResultChan() {
		if event.Type == watch.Deleted {
			ch.Send(true)
		}
	}
}

// Push ch<-true when the Persistent Volume is ready
func signalPVReady(watcher watch.Interface, ch *util.ReadyChannel) {
	for event := range watcher.ResultChan() {
		if event.Type == watch.Modified {
			pv := event.Object.(*apiv1.PersistentVolume)
			if pv.Status.Phase == apiv1.VolumeAvailable {
				ch.Send(true)
			}
		}
	}
}

// Push ch<-true when when Persistent Volume Claim is bound
func signalPVCReady(watcher watch.Interface, ch *util.ReadyChannel) {
	for event := range watcher.ResultChan() {
		if event.Type == watch.Modified {
			pvc := event.Object.(*apiv1.PersistentVolumeClaim)
			if pvc.Status.Phase == apiv1.ClaimBound {
				ch.Send(true)
			}
		}
	}
}

func (c *K8sClient) ListPods(opt metav1.ListOptions) (*apiv1.PodList, error) {
	return c.clientset.CoreV1().Pods(c.Namespace).List(context.TODO(), opt)
}

func (c *K8sClient) DeletePod(name string) error {
	return c.clientset.CoreV1().Pods(c.Namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (c *K8sClient) WatchDeletePod(name string, finished *util.ReadyChannel) {
	c.WatchFor(name, "Pod", signalDeleted, finished)
}

func (c *K8sClient) CreatePod(target *apiv1.Pod) (*apiv1.Pod, error) {
	return c.clientset.CoreV1().Pods(c.Namespace).Create(context.TODO(), target, metav1.CreateOptions{})
}

func (c *K8sClient) WatchCreatePod(name string, ready *util.ReadyChannel) {
	c.WatchFor(name, "Pod", signalPodReady, ready)
}

func (c *K8sClient) ListPVC(opt metav1.ListOptions) (*apiv1.PersistentVolumeClaimList, error) {
	return c.clientset.CoreV1().PersistentVolumeClaims(c.Namespace).List(context.TODO(), opt)
}

func (c *K8sClient) DeletePVC(name string) error {
	return c.clientset.CoreV1().PersistentVolumeClaims(c.Namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (c *K8sClient) WatchDeletePVC(name string, finished *util.ReadyChannel) {
	c.WatchFor(name, "PVC", signalDeleted, finished)
}

func (c *K8sClient) CreatePVC(target *apiv1.PersistentVolumeClaim) (*apiv1.PersistentVolumeClaim, error) {
	return c.clientset.CoreV1().PersistentVolumeClaims(c.Namespace).Create(context.TODO(), target, metav1.CreateOptions{})
}

func (c *K8sClient) WatchCreatePVC(name string, ready *util.ReadyChannel) {
	c.WatchFor(name, "PVC", signalPVCReady, ready)
}

func (c *K8sClient) ListPV(opt metav1.ListOptions) (*apiv1.PersistentVolumeList, error) {
	return c.clientset.CoreV1().PersistentVolumes().List(context.TODO(), opt)
}

func (c *K8sClient) DeletePV(name string) error {
	return c.clientset.CoreV1().PersistentVolumes().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (c *K8sClient) WatchDeletePV(name string, finished *util.ReadyChannel) {
	c.WatchFor(name, "PV", signalDeleted, finished)
}

func (c *K8sClient) CreatePV(target *apiv1.PersistentVolume) (*apiv1.PersistentVolume, error) {
	return c.clientset.CoreV1().PersistentVolumes().Create(context.TODO(), target, metav1.CreateOptions{})
}

func (c *K8sClient) WatchCreatePV(name string, ready *util.ReadyChannel) {
	c.WatchFor(name, "PV", signalPVReady, ready)
}

func (c *K8sClient) ListServices(opt metav1.ListOptions) (*apiv1.ServiceList, error) {
	return c.clientset.CoreV1().Services(c.Namespace).List(context.TODO(), opt)
}

func (c *K8sClient) CreateService(target *apiv1.Service) (*apiv1.Service, error) {
	return c.clientset.CoreV1().Services(c.Namespace).Create(context.TODO(), target, metav1.CreateOptions{})
}

func (c *K8sClient) DeleteService(name string) error {
	return c.clientset.CoreV1().Services(c.Namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func (c *K8sClient) WatchDeleteService(name string, finished *util.ReadyChannel) {
	c.WatchFor(name, "SVC", signalDeleted, finished)
}

// call a bash command inside of a pod, with the command given as a []string of bash words
func (c *K8sClient) PodExec(command []string, pod *apiv1.Pod, nContainer int) (bytes.Buffer, bytes.Buffer, error) {
	var stdout, stderr bytes.Buffer
	restRequest := c.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(c.Namespace).
		SubResource("exec").
		VersionedParams(
			&apiv1.PodExecOptions{
				Container: pod.Spec.Containers[nContainer].Name,
				Command:   command,
				Stdin:     false,
				Stdout:    true,
				Stderr:    true,
				TTY:       false,
			},
			scheme.ParameterCodec,
		)
	exec, err := remotecommand.NewSPDYExecutor(c.config, "POST", restRequest.URL())
	if err != nil {
		return stdout, stderr, errors.New(fmt.Sprintf("Couldn't create executor: %s", err.Error()))
	}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		return stdout, stderr, errors.New(fmt.Sprintf("Stream error: %s", err.Error()))
	}
	return stdout, stderr, nil
}
