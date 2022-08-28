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
	podName string
	Pod managed.Pod
	userID string
	client k8sclient.K8sClient
}

func NewPodDeleter(podName string, userID string, client k8sclient.K8sClient) (PodDeleter, error) {
	deleter := PodDeleter{podName: podName, userID: userID, client: client}
	err := deleter.initPodObject()
	if err != nil {
		return deleter, err
	}
	return deleter, nil
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
	pod := managed.NewPod(&podList.Items[0], pd.client)
	if pod.Owner.UserID != pd.userID {
		return errors.New(fmt.Sprintf("Pod %s not owned by user %s", pd.podName, pd.userID))
	}
	pd.Pod = pod
	return nil
}

func (pd *PodDeleter) DeletePod(finished *util.ReadyChannel) error {
	podDeleted := util.NewReadyChannel(pd.client.TimeoutDelete)
	go pd.client.WatchDeletePod(pd.podName, podDeleted)
	err := pd.client.DeletePod(pd.podName)
	if err != nil {
		return err
	}
	go pd.Pod.RunDeleteJobsWhenReady(podDeleted, finished)
	return nil
}
