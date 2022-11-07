# User pods kubernetes backend

This makes the api available for the user_pods app to create/delete pods and their necessary ingresses, services, and volumes.

## Overview

main.go runs inside of a pod in the kubernetes cluster.
Its pod is assigned roles with minimum permissions to create user pods and associated resources within the sciencedata namespace.
It receives traffic from sciencedata via an ingress.
It has a wildcard TLS certificate that allows it to make an ingress for each user pod with a unique subdomain prefix.
It supports pulling docker images from a local registry.
Its runtime configuration can be overwritten with environment variables, and stdout is available in kubernetes logs.
If the backend is restarted, state is recovered, i.e. cached information about the running user pods.
Tests can be run in a separate namespace without downtime.

## API

All requests should be POST with the header "Content-Type: application/json".
It is assumed that the API is only accessible from the sciencedata network and that the silos always make requests with the correct user_id. 
In the included manifest (manifests/deploy_user_pods_backend.yaml), this is accomplished with a hostname whitelist in the ingress, which requires that the kubernetes.io/nginx-ingress controller is running in the cluster.

| Request           | input data                                                                  | response           |
| ----------------- | --------------------------------------------------------------------------- | ------------------ |
| /get_pods         | {user_id: string}                                                           | [podInfo]          |
| /create_pod       | {yaml_url: string, user_id: string, settings: map[string]map[string]string} | {pod_name: string} |
| /watch_create_pod | {user_id: string, pod_name: string}                                         | {ready: bool}      |
| /delete_pod       | {user_id: string, pod_name: string}                                         | {requested: bool}  |
| /watch_delete_pod | {user_id: string, pod_name: string}                                         | {deleted: bool}    |
| /delete_all_user  | {user_id: string}                                                           | {deleted: bool}    |

#### get_pods

the [podInfo] response is a list of dicts for each pod, including 
{pod_name, container_name, image_name, pod_ip, node_ip, owner, age, status, url, tokens, k8s_pod_info}

Tokens is a dict where each key is one of the comma-separated values in metadata.annotations["sciencedata.dk/copy-token"] of the pod's manifest. 
The first container is expected to create a file named /tmp/key, and the value is the content of this file.

k8s_pod_info is a dict for information about related resources. 
For now, the nodePort of the ssh service is the only value this gets used for.

#### create_pod

Settings is a dict in the format {container0_name: {env_var: value, ...}, container1_name: {env_var: value,... }, ...}

The backend makes no assumptions about what environment variables should be there;
it only sets the environment variables from the request, overwriting existing environment variables if they already exist.

#### watch_create_pod and watch_delete_pod

The backend maintains a dict of {pod_name: {user_id, *readyChannel}} both for pods being created and pods being deleted.
*readyChannel is a pointer to a util.readyChannel object which waits for the pod to finish being created/deleted and receives a bool (success/failure) at that time.
When the client makes a watch_create_pod request, the backend checks whether there is an entry for that pod_name and if so, whether the user_id matches. If it does, it waits until the readyChannel receives a value, and then replies to the client. This way, the user can be notified right away when a pod reaches Ready state.

Because the client could manually make the request with an arbitrary pod_name, the default returned value of
watch_create_pod is false, and the defaulte returned value of watch_delete_pod is true, 
so that the watch functions cannot be abused to get information about whether other users have a pod with the given name.

When a pod is created, once it reaches ready state, the entry is removed from the backend's watch dict.
In case the user makes a watch_create_pod request after this occurs, the backend checks whether the pod exists and 
returns true if so.

#### delete_all_user

Delete's all of the users' pods, storage, and other associated resources.
Not implemented in the frontend, but often convenient for manually cleaning up.

## Deployment

The manifest in manifests/deploy_user_pods_backend.yaml contains most of the resources necessary for the backend to function.
It assumes that the following are in place already

- kubernetes.io/ingress-nginx ingress controller is installed in the cluster and accessible at a public domain name
- there is a secret in the sciencedata namespace with a wildcard tls certificate ({data: {tls.crt, tls.key}}) for "*.ingressDomain" for the config value ingressDomain, where podName.ingressDomain is the full domain name that will route to each pod. Wildcard tls certificates can be automatically generated and renewed with cert-manager if you can allow it to add DNS TXT records by API.
- pod manifests that have a container which mounts a volume called "sciencedata" will be modified to point to the user's storage PVC. If there are other volumes specified, such as read-only software for jupyter, those need to exist already in the sciencedata namespace.
- if you want to support pod manifests that pull images from a private docker registry, they should be written with spec.containers[].image: LOCALREGISTRY/imageName. Then the configuration value for localRegistryURL will replace LOCALREGISTRY in the image string. If the docker registry requires credentials, then a secret with those credentials needs to be present in the sciencedata namespace with the name equal to the localRegistrySecret config value.

## Testing

There are extensive unit tests that cover each module. The modules will be briefly explained 

### modules

- Server: api functions, wrappers for being served by an http handler, watch dicts
- Managed: rich objects to represent Users and Pods, functions like list all of the users' pods, get podInfo, run tasks after pod creation, templates for services and ingresses that rely on information about the pods, etc.
- Podcreator: object for fetching the manifest and calling for pod creation
- Poddeleter: object for pod deletion
- Util: readyChannel objects for many asynchronous tasks, configuration
- K8sclient: wrapper for kubernetes client-go packages, watch for creation/deletion, equivalent of `kubectl exec`
- Testingutil: only used in testing to make http requests to server, breaking dependency loop.

### running tests

Build the docker image using Dockerfile_testing (docker build -f).
Apply manifests/deploy_user_pods_testing.yaml with the same conditions met as for regular deployment.
The entrypoint is just sshd, so that one can rsync updated source files easily without rebuilding the image each time.
The server has to be started manually, e.g. `./main > out &`, and it only works from a shell created by `kubectl exec`
(environment variables necessary for calling the kubernetes api are set for a `kubectl exec` shell).
With the server running, then run the unit test for e.g. the server module by `cd server` `go test -v`.

**Note: The server has to be running for the unit tests to work.**
This is not the norm for golang unit tests, but is necessary for this use case because many of the components can only
be tested dynamically (with pods being created/deleted).
The dependency graph must be acyclic, so e.g. the managed module cannot import the podcreator module for testing.
Instead, it imports testingutil to make http requests to ./main running on localhost to create the pods, 
then tests functions within its scope on the running pods.

## Configuration

The default configuration is in config.yaml which is included in the docker image. 
For each variable, if there is set an environment variable in all caps prefixed with backend (e.g. ingressDomain -> BACKEND_INGRESSDOMAIN), then the value of the environment variable will be used instead of config.yaml. This allows for changing the configuration via the manifest without rebuilding the docker image.

### Values
 
- defaultRestartPolicy: must be a valid pod.spec.restartPolicy, "Always", "Never", "OnFailure", sets the default but will not overwrite if the restartPolicy is explicitly defined in a pod's manifest.
- timeoutCreate: timeout for pod creation, in the format of time.Duration (e.g. "90s" or "1h2m3s"). If the timeout is reached before the pod reaches Ready state, then the pod and associated resources will be deleted. Note that if there is a new version of the docker image, it needs to be pulled within the timeout. As long neither the timeout nor Ready state has been reached, watch_create_pod will not get a response.
- timeoutDelete: timeout to wait for pod deletion before giving up. If the timeout is reached, then deletion jobs like cleaning up related resources won't be performed.
- namespace: the namespace where pods and other resources should be created. Needs to match the namespace where the backend's serviceAccount has permissions and where necessary secrets exist.
- podCacheDir: directory in the backend's local filesystem where podcaches should be stored. The directory needs to exist.
- whitelistManifestRegex: a regex that the yaml_url in a create_pod request must match in order to be used. Because users could manually create a request with an arbitrary yaml_url, this should be used to restrict to manifests controlled by the operators.
- tokenByteLimit: maximum number of bytes that will be copied for tokens. It is possible for a user to modify the files from which tokens are copied, and this prevents a simple type DoS attack by filling up a file with lots of data.
- nfsStorageRoot: the path prefix before the user_id that should be used in creating nfs persistent volumes.
- testingHost: IP address where nfs storage is available for testing. Normally, the server gets the silo IP address from the http request, but when testing, the request comes from localhost.
- localRegistryURL: string to replace "LOCALREGISTRY" in pod.spec.containers[].image of the pod manifest
- localRegistrySecret: name of the secret in the namespace that contains auth credentials to pull from the local docker registry if needed
- ingressDomain: domain suffix for pods. For example, if "pods.sciencedata.dk", then ingresses will be created for "podName.pods.sciencedata.dk", and a wildcard tls cert needs to be available for "*.pods.sciencedata.dk"
- ingressWildCardSecret: name of the kubernetes secret in the sciencedata namespace that contains the wildcard tls cert.
