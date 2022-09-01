package managed

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testUser = "registeredtest7"
	remoteIP = "10.0.0.20"
)

// Test user functions
func TestNewUser(t *testing.T) {
	userIDs := []string{
		"foo@bar",
		"foo",
		"foo@bar.baz",
		"foo.bar@baz",
	}
	for _, uid := range userIDs {
		u := NewUser(uid, *k8sclient.NewK8sClient())
		labels := map[string]string{"user": u.Name, "domain": u.Domain}
		if u.UserID != util.GetUserIDFromLabels(labels) {
			t.Fatalf("User contstructed incorrectly with userID %s", uid)
		}
	}
}

func TestListOptions(t *testing.T) {
	c := *k8sclient.NewK8sClient()
	tests := []struct {
		input User
		want  metav1.ListOptions
	}{
		{NewUser("foo", c), metav1.ListOptions{LabelSelector: "user=foo,domain="}},
		{NewUser("foo@bar", c), metav1.ListOptions{LabelSelector: "user=foo,domain=bar"}},
		{NewUser("foo@bar.baz", c), metav1.ListOptions{LabelSelector: "user=foo,domain=bar.baz"}},
	}
	for _, test := range tests {
		if test.input.GetListOptions() != test.want {
			t.Fatalf("Bad list options for userID %s", test.input.UserID)
		}
	}
}

func TestListPods(t *testing.T) {
	u := NewUser(testUser, *k8sclient.NewK8sClient())
	// Use u.ListPods
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods")
	}

	// Then use a manual list from the k8sclient
	manualPodList, err := u.Client.ListPods(u.GetListOptions())
	// For each of the manually listed pods,
	for _, existingPod := range manualPodList.Items {
		// Look through ListPods and make sure it's there
		inPodList := false
		for _, listedPod := range podList {
			if listedPod.Object.Name == existingPod.Name {
				inPodList = true
				break
			}
		}
		if !inPodList {
			t.Fatalf("Pod %s wasn't listed in User.ListPods", existingPod.Name)
		}
	}
	if len(podList) != len(manualPodList.Items) {
		t.Fatalf("Mismatched number of user pods listed")
	}
}

func TestOwnership(t *testing.T) {
	u := NewUser(testUser, *k8sclient.NewK8sClient())
	// Use u.ListPods
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf("Couldn't list user pods")
	}
	if len(podList) == 0 {
		t.Fatalf("Need to have at least one pod running for this test")
	}
	for _, pod := range podList {
		owns, err := u.OwnsPod(pod.Object.Name)
		if err != nil {
			t.Fatalf(err.Error())
		}
		if !owns {
			t.Fatalf("User thinks they don't own a pod that they do")
		}
	}
	tryPodNames := []string{"foobar-pod", "user-pods-backend", "user-pods-backend-testing"}
	for _, name := range tryPodNames {
		owns, err := u.OwnsPod(name)
		if err != nil {
			t.Fatalf(err.Error())
		}
		if owns {
			t.Fatalf("User thinks they own pod %s, but they don't", name)
		}
	}
}

func TestUserString(t *testing.T) {
	c := *k8sclient.NewK8sClient()
	tests := []struct {
		input User
		want  string
	}{
		{NewUser("foo", c), "foo"},
		{NewUser("foo@bar", c), "foo-bar"},
		{NewUser("Foo@Bar", c), "Foo-Bar"},
		{NewUser("foo@bar.baz", c), "foo-bar-baz"},
		{NewUser("foo@bar.baz-baz", c), "foo-bar-baz-baz"},
		{NewUser("foo.bar@bar.baz", c), "foo-bar-bar-baz"},
	}
	for _, test := range tests {
		if test.input.GetUserString() != test.want {
			t.Fatalf("Bad list options for userID %s. Got %s, wanted %s", test.input.UserID, test.input.GetUserString(), test.want)
		}
	}
}

func TestCleanNonexistentUserStorage(t *testing.T) {
	u := NewUser("foo@bar", *k8sclient.NewK8sClient())
	finished := util.NewReadyChannel(time.Second)
	err := u.CleanUserStorage(finished)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !finished.Receive() {
		t.Fatal("Received false for deletion of user storage")
	}
}

func ensureUserHasJupyterPod() error {
	u := NewUser(testUser, *k8sclient.NewK8sClient())
	userPodList, err := u.ListPods()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list user pods %s", err.Error()))
	}
	hasJupyterPod := false
	for _, pod := range userPodList {
		if strings.Contains(pod.Object.Name, "jupyter") {
			hasJupyterPod = true
			break
		}
	}
	if !hasJupyterPod {
		return errors.New("User doesn't have an existing jupyter pod")
	}
	return nil
}

func ensureUserHasUbuntuPod() error {
	u := NewUser(testUser, *k8sclient.NewK8sClient())
	userPodList, err := u.ListPods()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list user pods %s", err.Error()))
	}
	hasJupyterPod := false
	for _, pod := range userPodList {
		if strings.Contains(pod.Object.Name, "ubuntu") {
			hasJupyterPod = true
			break
		}
	}
	if !hasJupyterPod {
		return errors.New("User doesn't have an existing ubuntu pod")
	}
	return nil
}

func TestPodData(t *testing.T) {
	err := ensureUserHasJupyterPod()
	if err != nil {
		t.Fatalf(err.Error())
	}
	u := NewUser(testUser, *k8sclient.NewK8sClient())
	podList, err := u.ListPods()
	if err != nil {
		t.Fatalf(err.Error())
	}

	// For each of the user's pods,
	for _, pod := range podList {
		// double check that the saved cache matches environment variables
		cache, err := pod.loadPodCache()
		if err != nil {
			t.Fatalf("Couldn't load pod cache for pod %s", pod.Object.Name)
		}
		info := pod.GetPodInfo()
		object := pod.Object
		if info.PodName != object.Name {
			t.Fatalf("Pod %s wrong name", object.Name)
		}
		if info.Owner != u.UserID {
			t.Fatalf("Pod %s wrong owner", object.Name)
		}
		for key, value := range info.Tokens {
			// try to re-copy the token from the running pod
			pod.copyToken(key)
			if value != cache.Tokens[key] {
				t.Fatalf("Cached token %s had value %s, but in the running pod %s is currently %s", key, value, object.Name, cache.Tokens[key])
			}
		}
		sshPort, exists := info.OtherResourceInfo["sshPort"]
		if exists && !pod.needsSshService() {
			t.Fatalf("PodInfo has a listed Ssh port, but pod %s doesn't listen for ssh", object.Name)
		}
		if exists {
			newlyRetreivedSshPort, err := pod.getSshPort()
			if err != nil {
				t.Fatalf(err.Error())
			}
			if newlyRetreivedSshPort != sshPort {
				t.Fatalf("Pod %s has ssh service on port %s, but cached sshport %s", object.Name, newlyRetreivedSshPort, sshPort)
			}
		}
	}
}
