package podcreator

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/managed"
	"github.com/deic.dk/user_pods_k8s_backend/util"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
)

// TODO move this to a config file
const whitelistYamlRegex = "https:\\/\\/raw[.]githubusercontent[.]com\\/deic-dk\\/pod_manifests"

type PodCreator struct {
	targetPod        *apiv1.Pod
	yamlURL          string
	user             managed.User
	siloIP           string
	containerEnvVars map[string]map[string]string
	client           k8sclient.K8sClient
}

// Initialization functions
// PodCreator must be initialized with a valid targetPod, which can then be
// created by calling CreatePod()

// Initialize a PodCreator with the data it will need to make a pod
// Return without error if it is ready to call CreatePod()
func NewPodCreator(
	yamlURL string,
	user managed.User,
	siloIP string,
	containerEnvVars map[string]map[string]string,
	client k8sclient.K8sClient,
) (PodCreator, error) {
	creator := PodCreator{
		yamlURL:          yamlURL,
		user:             user,
		siloIP:           siloIP,
		containerEnvVars: containerEnvVars,
		client:           client,
		targetPod:        nil,
	}
	err := creator.initTargetPod()
	if err != nil {
		return creator, errors.New(fmt.Sprintf("Couldn't initialize PodCreator with a valid targetPod: %s", err.Error()))
	}
	return creator, nil
}

// Return the user's siloIP in the subnet where data can be accessed by the pods.
func (pc *PodCreator) getSiloIPDataNet() string {
	return strings.Replace(pc.siloIP, "10.0.", "10.2.", 1)
}

// Return the map of environment variables that should be set in each container of
// the target pod, so that pods can know how to reach the user's data
func (pc *PodCreator) getMandatoryEnvVars() map[string]string {
	mandatoryEnvVars := make(map[string]string)
	mandatoryEnvVars["HOME_SERVER"] = pc.getSiloIPDataNet()
	mandatoryEnvVars["SD_UID"] = pc.user.UserID
	return mandatoryEnvVars
}

// Retrieve the yaml manifest and parse it into a pod API object to attempt to create
func (pc *PodCreator) initTargetPod() error {
	if pc.targetPod != nil {
		return errors.New("PodCreator already initialized with a targetPod")
	}
	var targetPod apiv1.Pod

	// Get the manifest
	yaml, err := pc.getYaml()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't get manifest: %s", err.Error()))
	}

	// Convert it from []byte -> runtime.Object -> unstructured -> apiv1.Pod
	deserializer := scheme.Codecs.UniversalDeserializer()
	object, _, err := deserializer.Decode([]byte(yaml), nil, nil)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't deserialize manifest: %s", err.Error()))
	}
	unstructuredPod, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't convert runtime.Object: %s", err.Error()))
	}
	// Fill out targetPodObject with the data from the manifest
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredPod, &targetPod)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't parse manifest as apiv1.Pod: %s", err.Error()))
	}

	// Fill in values in targetPodObject according to the request
	pc.applyCreatePodSettings(&targetPod)
	// Find and set a unique podName in the format pod.metadata.name-user-domain-x
	err = pc.applyCreatePodName(&targetPod)
	if err != nil {
		return err
	}
	err = pc.applyCreatePodVolumes(&targetPod)
	if err != nil {
		return err
	}

	pc.targetPod = &targetPod
	return nil
}

// Retrieve the yaml manifest from a URL matching the whitelist
func (pc *PodCreator) getYaml() (string, error) {
	allowed, err := regexp.MatchString(whitelistYamlRegex, pc.yamlURL)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", errors.New(fmt.Sprintf("YamlURL %s not matched to whitelist", pc.yamlURL))
	}
	response, err := http.Get(pc.yamlURL)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Could not fetch manifest from given url: %s", pc.yamlURL))
	}
	defer response.Body.Close()

	// if the GET status isn't "200 OK"
	if response.StatusCode != 200 {
		return "", errors.New(fmt.Sprintf("Didn't find a file at the given url: %s", pc.yamlURL))
	}

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Could not parse manifest from given url: %s", pc.yamlURL))
	}

	return string(body), nil
}

func (pc *PodCreator) applyCreatePodSettings(targetPodObject *apiv1.Pod) {
	for i, container := range targetPodObject.Spec.Containers {
		envVars, exist := pc.containerEnvVars[container.Name]
		// if there are settings for this container (if container.Name is a key in request.ContainerEnvVars)
		if exist {
			// then for each setting,
			for name, value := range envVars {
				// find the env entry with a matching name, and set the value
				for ii, env := range container.Env {
					if env.Name == name {
						targetPodObject.Spec.Containers[i].Env[ii].Value = value
					}
				}
			}
		}
		// for each envvar that should be set in every container,
		for name, value := range pc.getMandatoryEnvVars() {
			overwrite := false
			// try to overwrite the value if the var already exists
			for ii, env := range targetPodObject.Spec.Containers[i].Env {
				if env.Name == name {
					targetPodObject.Spec.Containers[i].Env[ii].Value = value
					overwrite = true
				}
			}
			// otherwise, append the var
			if !overwrite {
				targetPodObject.Spec.Containers[i].Env = append(targetPodObject.Spec.Containers[i].Env, apiv1.EnvVar{
					Name:  name,
					Value: value,
				})
			}
		}
	}
}

func (pc *PodCreator) applyCreatePodName(targetPodObject *apiv1.Pod) error {
	basePodName := fmt.Sprintf("%s-%s", targetPodObject.Name, pc.user.GetUserString())
	existingPodList, err := pc.user.ListPods()
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't list pods to find a unique pod name: %s", err.Error()))
	}
	podName := basePodName
	var nameInUse bool
	for i := 1; i < 11; i++ {
		nameInUse = false
		for _, existingPod := range existingPodList {
			if existingPod.Object.Name == podName {
				nameInUse = true
				break
			}
		}
		// if a pod with the name podName doesn't exist yet
		if !nameInUse {
			// then set the target pod's name and labels, then finish
			targetPodObject.Name = podName
			targetPodObject.ObjectMeta.Labels = map[string]string{
				"user":    pc.user.Name,
				"domain":  pc.user.Domain,
				"podName": podName,
			}
			return nil
		}
		// otherwise try again with the next name
		podName = fmt.Sprintf("%s-%d", basePodName, i)
	}
	// if all 10 names are in use,
	return errors.New(fmt.Sprintf("Couldn't find a unique name for %s-(1-9), all are in use", basePodName))
}

// Dynamically generate the pod.Spec.Volume entry for an unsatisfied pod.Spec.Container[].VolumeMount
func (pc *PodCreator) getCreatePodSpecVolume(volumeMount apiv1.VolumeMount) (apiv1.Volume, error) {
	switch volumeMount.Name {
	case "local":
		return apiv1.Volume{
			Name: "local",
			VolumeSource: apiv1.VolumeSource{
				PersistentVolumeClaim: &apiv1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("local-claim-%s", strings.ReplaceAll(volumeMount.MountPath, "/", "-")),
				},
			},
		}, nil
	case "sciencedata":
		return apiv1.Volume{
			Name: "sciencedata",
			VolumeSource: apiv1.VolumeSource{
				PersistentVolumeClaim: &apiv1.PersistentVolumeClaimVolumeSource{
					ClaimName: pc.user.GetStoragePVName(),
				},
			},
		}, nil
	default:
		return apiv1.Volume{}, errors.New(
			fmt.Sprintf("Not known how to dynamically create an entry for this volume mount %+v", volumeMount),
		)
	}
}

// Make sure that any VolumeMounts that aren't specified in Spec.Volumes get added.
// This should be used for e.g. the user's storage, which should be generated at runtime
// for the given user.
func (pc *PodCreator) applyCreatePodVolumes(targetPodObject *apiv1.Pod) error {
	for _, container := range targetPodObject.Spec.Containers {
		for _, volumeMount := range container.VolumeMounts {
			// For each volume mount, first check whether the volume is specified in pod.Spec.Volumes
			satisfied := false
			for _, volume := range targetPodObject.Spec.Volumes {
				if volume.Name == volumeMount.Name {
					satisfied = true
					break
				}
			}
			if !satisfied {
				targetVolumeSpec, err := pc.getCreatePodSpecVolume(volumeMount)
				if err != nil {
					return err
				}
				targetPodObject.Spec.Volumes = append(targetPodObject.Spec.Volumes, targetVolumeSpec)
			}
		}
	}
	return nil
}

// Functions for pod creation

// Call the kubernetes API for creation of the PodCreator's targetPod
// Create and return a managed.Pod object corresponding to the created pod
// Use the ready channel to let the parent know when the pod's start jobs are complete
func (pc *PodCreator) CreatePod(ready *util.ReadyChannel) (managed.Pod, error) {
	var pod managed.Pod
	if pc.targetPod == nil {
		return pod, errors.New("PodCreater wasn't initialized with a targetPod, cannot create empty target.")
	}

	storageReady := util.NewReadyChannel(pc.client.TimeoutCreate)
	if pc.requiresUserStorage() {
		pc.ensureUserStorageExists(storageReady)
	} else {
		storageReady.Send(true)
	}

	podReady := util.NewReadyChannel(pc.client.TimeoutCreate)
	go func() {
		pc.client.WatchCreatePod(pc.targetPod.Name, podReady)
		if podReady.Receive() {
			fmt.Printf("Ready pod %s\n", pc.targetPod.Name)
		} else {
			fmt.Printf("Warning: pod %s didn't reach ready state\n", pc.targetPod.Name)
		}
	}()

	createdPod, err := pc.client.CreatePod(pc.targetPod)
	if err != nil {
		return pod, errors.New(fmt.Sprintf("Call to create pod %s failed: %s", pc.targetPod.Name, err.Error()))
	}
	pod = managed.NewPod(createdPod, pc.client)

	startJobWaitChans := make([]*util.ReadyChannel, 2)
	startJobWaitChans[0] = storageReady
	startJobWaitChans[1] = podReady

	go pod.RunStartJobsWhenReady(startJobWaitChans, ready)
	return pod, nil
}

// if the targetPod requires a PV and PVC for the user, return true
func (pc *PodCreator) requiresUserStorage() bool {
	req := false
	for _, volume := range pc.targetPod.Spec.Volumes {
		if volume.Name == "sciencedata" {
			req = true
		}
	}
	return req
}

// Check that the PV and PVC for the user's /tank/storage directory exist if required
func (pc *PodCreator) ensureUserStorageExists(ready *util.ReadyChannel) error {
	listOptions := pc.user.GetStorageListOptions()
	PVready := util.NewReadyChannel(pc.client.TimeoutCreate)
	PVCready := util.NewReadyChannel(pc.client.TimeoutCreate)
	PVList, err := pc.client.ListPV(listOptions)
	if err != nil {
		return err
	}
	if len(PVList.Items) == 0 {
		targetPV := pc.user.GetTargetStoragePV(pc.siloIP)
		go func() {
			pc.client.WatchCreatePV(targetPV.Name, PVready)
			if PVready.Receive() {
				fmt.Printf("Ready PV %s\n", targetPV.Name)
			} else {
				fmt.Printf("Warning PV %s didn't reach ready state\n", targetPV.Name)
			}
		}()
		_, err := pc.client.CreatePV(targetPV)
		if err != nil {
			return err
		}
	} else {
		PVready.Send(true)
	}

	PVCList, err := pc.client.ListPVC(listOptions)
	if err != nil {
		return err
	}
	if len(PVCList.Items) == 0 {
		targetPVC := pc.user.GetTargetStoragePVC(pc.siloIP)
		go func() {
			pc.client.WatchCreatePVC(targetPVC.Name, PVCready)
			if PVCready.Receive() {
				fmt.Printf("Ready PVC %s\n", targetPVC.Name)
			} else {
				fmt.Printf("Warning PVC %s didn't reach ready state\n", targetPVC.Name)
			}
		}()
		_, err := pc.client.CreatePVC(targetPVC)
		if err != nil {
			return err
		}
	} else {
		PVCready.Send(true)
	}
	go util.CombineReadyChannels([]*util.ReadyChannel{PVready, PVCready}, ready)
	return nil
}
