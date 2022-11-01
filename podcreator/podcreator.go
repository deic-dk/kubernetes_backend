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

type PodCreator struct {
	targetPod        *apiv1.Pod
	yamlURL          string
	user             managed.User
	siloIP           string
	containerEnvVars map[string]map[string]string
	client           k8sclient.K8sClient
	globalConfig     util.GlobalConfig
}

// Initialization functions
// PodCreator must be initialized with a valid targetPod, which can then be
// created by calling CreatePod()

// Initialize a PodCreator with the data it will need to make a pod
// Return without error if it is ready to call CreatePod()
func NewPodCreator(
	yamlURL string,
	userID string,
	siloIP string,
	containerEnvVars map[string]map[string]string,
	client k8sclient.K8sClient,
	globalConfig util.GlobalConfig,
) (PodCreator, error) {
	creator := PodCreator{
		yamlURL:          yamlURL,
		user:             managed.NewUser(userID, client, globalConfig),
		siloIP:           siloIP,
		containerEnvVars: containerEnvVars,
		client:           client,
		globalConfig:     globalConfig,
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
	pc.targetPod = &targetPod

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
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredPod, pc.targetPod)
	if err != nil {
		return errors.New(fmt.Sprintf("Couldn't parse manifest as apiv1.Pod: %s", err.Error()))
	}

	// Fill in values in targetPodObject according to the request
	pc.applyCreatePodSettings()
	// Fill in values in targetPodObject that are independent of the request
	pc.applyMandatorySettings()
	// Fill in the correct settings to pull the image from a local repository if necessary
	pc.applyRegistrySettings()
	// Find and set a unique podName in the format pod.metadata.name-user-domain-x
	err = pc.applyCreatePodName()
	if err != nil {
		return err
	}
	err = pc.applyCreatePodVolumes()
	if err != nil {
		return err
	}

	return nil
}

// Retrieve the yaml manifest from a URL matching the whitelist
func (pc *PodCreator) getYaml() (string, error) {
	allowed, err := regexp.MatchString(pc.globalConfig.WhitelistManifestRegex, pc.yamlURL)
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

// Apply all settings that are mandatory for each pod, independent of the request or manifest
func (pc *PodCreator) applyMandatorySettings() {
	// Set the restart policy from the global config
	pc.targetPod.Spec.RestartPolicy = pc.globalConfig.RestartPolicy

	// Set environment variables in each container
	for i, _ := range pc.targetPod.Spec.Containers {
		for name, value := range pc.getMandatoryEnvVars() {
			overwrite := false
			// try to overwrite the value if the var already exists
			for ii, env := range pc.targetPod.Spec.Containers[i].Env {
				if env.Name == name {
					pc.targetPod.Spec.Containers[i].Env[ii].Value = value
					overwrite = true
				}
			}
			// otherwise, append the var
			if !overwrite {
				pc.targetPod.Spec.Containers[i].Env = append(pc.targetPod.Spec.Containers[i].Env, apiv1.EnvVar{
					Name:  name,
					Value: value,
				})
			}
		}
	}
}

func (pc *PodCreator) applyCreatePodSettings() {
	for i, container := range pc.targetPod.Spec.Containers {
		envVars, exist := pc.containerEnvVars[container.Name]
		// if there are settings for this container (if container.Name is a key in request.ContainerEnvVars)
		if exist {
			// then for each setting,
			for name, value := range envVars {
				// find the env entry with a matching name, and set the value
				for ii, env := range container.Env {
					if env.Name == name {
						pc.targetPod.Spec.Containers[i].Env[ii].Value = value
					}
				}
			}
		}
	}
}

// If any containers in the pod manifest get their image from a local repository,
// then write in the URL where the image should be pulled from with the value in the config.
// If the config specifies the name of a secret with auth credentials to pull from the
// local repository, then fill it in the pod spec.
func (pc *PodCreator) applyRegistrySettings() {
	requires_local_registry := false
	for ii, container := range pc.targetPod.Spec.Containers {
		if strings.Contains(container.Image, "LOCALREGISTRY") {
			requires_local_registry = true
			pc.targetPod.Spec.Containers[ii].Image = strings.Replace(container.Image, "LOCALREGISTRY", pc.globalConfig.LocalRegistryURL, 1)
		}
	}
	if requires_local_registry && len(pc.globalConfig.LocalRegistrySecret) > 0 {
		pc.targetPod.Spec.ImagePullSecrets = []apiv1.LocalObjectReference{
			{Name: pc.globalConfig.LocalRegistrySecret},
		}
	}
}

func (pc *PodCreator) applyCreatePodName() error {
	basePodName := fmt.Sprintf("%s-%s", pc.targetPod.Name, pc.user.GetUserString())
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
			pc.targetPod.Name = podName
			pc.targetPod.ObjectMeta.Labels = map[string]string{
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
func (pc *PodCreator) applyCreatePodVolumes() error {
	for _, container := range pc.targetPod.Spec.Containers {
		for _, volumeMount := range container.VolumeMounts {
			// For each volume mount, first check whether the volume is specified in pod.Spec.Volumes
			satisfied := false
			for _, volume := range pc.targetPod.Spec.Volumes {
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
				pc.targetPod.Spec.Volumes = append(pc.targetPod.Spec.Volumes, targetVolumeSpec)
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

	storageReady := util.NewReadyChannel(pc.globalConfig.TimeoutCreate)
	if pc.requiresUserStorage() {
		pc.user.CreateUserStorageIfNotExist(storageReady, pc.siloIP)
	} else {
		storageReady.Send(true)
	}

	podReady := util.NewReadyChannel(pc.globalConfig.TimeoutCreate)
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
	pod = managed.NewPod(createdPod, pc.client, pc.globalConfig)

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
