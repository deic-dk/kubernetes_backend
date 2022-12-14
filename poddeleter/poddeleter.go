package poddeleter

import (
	"errors"
	"fmt"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PodDeleter struct {
	podName      string
	Pod          managed.Pod
	userID       string
	client       k8sclient.K8sClient
	globalConfig util.GlobalConfig
	initialized  bool
}

func NewPodDeleter(podName string, userID string, client k8sclient.K8sClient, globalConfig util.GlobalConfig) (PodDeleter, error) {
	deleter := PodDeleter{podName: podName, userID: userID, client: client, globalConfig: globalConfig, initialized: false}
	err := deleter.initPodObject()
	if err != nil {
		return deleter, err
	}
	return deleter, nil
}

func NewFromPod(pod managed.Pod) PodDeleter {
	return PodDeleter{podName: pod.Object.Name, userID: pod.Owner.UserID, client: pod.Client, Pod: pod, globalConfig: pod.GlobalConfig, initialized: true}
}

func (pd *PodDeleter) initPodObject() error {
	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", pd.podName),
	}
	podList, err := pd.client.ListPods(listOptions)
	if err != nil {
		return err
	}
	if len(podList.Items) < 1 {
		return errors.New(fmt.Sprintf("Didn't find pod by name %s", pd.podName))
	}
	pod := managed.NewPod(&podList.Items[0], pd.client, pd.globalConfig)
	if pod.Owner.UserID != pd.userID {
		return errors.New(fmt.Sprintf("Pod %s not owned by user %s", pd.podName, pd.userID))
	}
	pd.Pod = pod
	pd.initialized = true
	return nil
}

func (pd *PodDeleter) DeletePod(finished *util.ReadyChannel) error {
	if !pd.initialized {
		return errors.New("PodDeleter can't DeletePod, not initialized with a pod object")
	}
	podDeleted := util.NewReadyChannel(pd.globalConfig.TimeoutDelete)
	go func() {
		pd.client.WatchDeletePod(pd.podName, podDeleted)
		if podDeleted.Receive() {
			fmt.Printf("Deleted pod %s\n", pd.podName)
		} else {
			fmt.Printf("Warning: failed to delete pod %s\n", pd.podName)
		}
	}()
	err := pd.client.DeletePod(pd.podName)
	if err != nil {
		return err
	}
	go pd.Pod.RunDeleteJobsWhenReady(podDeleted, finished)
	return nil
}
