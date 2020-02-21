# Kubernetes Cloud Controller Manager for Packet

`packet-ccm` is the Kubernetes CCM implementation for Packet. Read more about the CCM in [the official Kubernetes documentation](https://kubernetes.io/docs/tasks/administer-cluster/running-cloud-controller/).

## Requirements

At the current state of Kubernetes, running the CCM requires a few things.
Please read through the requirements carefully as they are critical to running the CCM on a Kubernetes cluster.

### Version
Recommended versions of Packet CCM based on your Kubernetes version:
* Packet CCM version v0.0.4 supports Kubernetes version >=v1.10
* Packet CCM version v1.0.0 support Kubernetes version >=1.15.0

## Deployment

**TL;DR**

1. Set kubernetes binary arguments correctly
1. Get your Packet project and secret API token
1. Deploy your Packet project and secret API token to your cluster
1. Deploy the CCM

### Kubernetes Binary Arguments

Control plane binaries in your cluster must start with the correct flags:

* `kubelet`: All kubelets in your cluster **MUST** set the flag `--cloud-provider=external`. This must be done for _every_ kubelet. Note that [k3s](https://k3s.io) sets its own CCM by default. If you want to use the CCM with k3s, you must disable the k3s CCM and enable this one, as `--disable-cloud-controller --kubelet-arg cloud-provider=external`.
* `kube-apiserver` and `kube-controller-manager` must **NOT** set the flag `--cloud-provider`. They then will use no cloud provider natively, leaving room for the Packet CCM.

**WARNING**: setting the kubelet flag `--cloud-provider=external` will taint all nodes in a cluster with `node.cloudprovider.kubernetes.io/uninitialized`.
The CCM itself will untaint those nodes when it initializes them.
Any pod that does not tolerate that taint will be unscheduled until the CCM is running.

#### Kubernetes node names must match the device name

By default, the kubelet will name nodes based on the node's hostname.
Packet's device hostnames are set based on the name of the device.
It is important that the Kubernetes node name matches the device name.

### Get Packet Project ID and API Token

To run `packet-ccm`, you need your Packet project ID and secret API key ID that your cluster is running in.
If you are already logged into the [Packet portal](https://app.packet.net), you can create one by clicking on your
profile in the upper right then "API keys".
To get your project ID click into the project that your cluster is under and select "project settings" from the header.
Under General you will see "Project ID". Once you have this information you will be able to fill in the config needed for the CCM.

#### Deploy Project and API
Copy [deploy/template/secret.yaml](./deploy/template/secret.yaml) to someplace useful:

```bash
cp deploy/template/secret.yaml /tmp/secret.yaml
```

Replace the placeholder in the copy with your token. When you're done, the `yaml` should look something like this:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: packet-cloud-config
  namespace: kube-system
stringData:
  cloud-sa.json: |
    {
    "apiKey": "abc123abc123abc123",
    "projectID": "abc123abc123abc123",
    }  
```


Then apply the file via `kubectl`, e.g.:

```bash
kubectl apply -f /tmp/secret.yaml`
```

You can confirm that the secret was created with the following:

````bash
$ kubectl -n kube-system get secrets packet-cloud-config
NAME                  TYPE                                  DATA      AGE
packet-cloud-config   Opaque                                1         2m
````

##

You can apply the rest of the CCM by using `kubectl` to apply `deploy/releases/<version>/deployment.yaml`, e.g:

```bash
kubectl apply -f deploy/releases/v1.0.0/deployment.yaml
```

### Logging

By default, ccm does minimal logging, relying on the supporting infrastructure from kubernetes. However, it does support
optional additional logging levels via the `--v=<level>` flag. In general:

* `--v=2`: log most function calls for devices and facilities, when relevant logging the returned values
* `--v=3`: log additional data when logging returned values, usually entire go structs
* `--v=5`: log every function call, including those called very frequently
## How It Works

The Kubernetes CCM for Packet deploys as a `Deployment` into your cluster with a replica of `1`. It provides the following services:

* lists available zones, returning Packet regions
* lists and retrieves instances by ID, returning Packet servers
* manages load balancers

### Facility

The Packet CCM works in one facility at a time. You can control which facility it works with as follows:

1. If the environment variable `PACKET_FACILITY_NAME` is set, use that value. Else..
1. If the config file has a field named `facility`, use that value. Else..
1. Read the facility from the Packet metadata of the host where the CCM is running. Else..
1. Fail.

The overrides of environment variable and config file are provided so that you can run the CCM
on a node in a different facility, or even outside of Packet entirely.

### Load Balancers

Packet does not offer managed load balancers like [AWS ELB](https://aws.amazon.com/elasticloadbalancing/) or [GCP Load Balancers](https://cloud.google.com/load-balancing/).
Instead, Packet uses BGP and [metallb](https://metallb.universe.tf) to provide the _equivalence_ of load balancing, without requiring an additional
managed service (or hop).

By default, the load balancer is deployed. You can disable the load balancer when
deploying the CCM, which will prevent the load balancer from running. To do so,
you use one of these two options:

* set the environment variable `PACKET_DISABLE_LB=true` before running the CCM
* set the `packet-cloud-config` secret property "disable-loadbalancer" to `true`

The Packet CCM, when the load balancer feature is not disabled, does the following.

Upon start, CCM does the following:

* Ensure MetalLB is deployed to the cluster, using an ASN of `65000`, while the top-of-rack (ToR) routers are `65530`
* Ensure the project has BGP enabled
* List each node in the cluster using a kubernetes node lister; for each:
  * ensure it has BGP enabled
  * add it to the metallb `ConfigMap` with the top-of-the-rack (ToR) router peer IP address for that node, and a kubernetes selector ensuring that the peer is only for this node.
* Start a kubernetes informer for node changes, responding to node addition and removals
  * Node addition: ensure the node has BGP enabled, and is in the metallb `ConfigMap` as above
  * Node deletion: remove the node from the metallb `ConfigMap`
* List all of the services in the cluster using a kubernetes service lister; for each:
  * If the service is not of `type=LoadBalancer`, ignore it
  * If a facility-specific `/32` IP address reservation tagged with `usage="packet-ccm-auto"` and `service="<service-hash>"` exists, and it already has that IP address affiliated with it, it is ready; ignore
  * If the service does not have that IP affiliated with it, add it to the [service spec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#servicespec-v1-core) and ensure it is in the pools of the metallb `ConfigMap` with `auto-assign: false`
* Start a kubernetes informer for service changes, responding to service addition and removals
  * Service addition: create a facility-specific `/32` IP address reservation tagged with `usage="packet-ccm-auto"` and `service="<service-hash>"`; if it is ready immediately, add it to the [service spec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#servicespec-v1-core) and ensure is in the pools of the metallb `ConfigMap` with `auto-assign: false`
  * Service deletion: find the `/32` IP address on the service spec and remove it; remove from the `ConfigMap`
* Start an independent loop that checks every 30 seconds (configurable) for IP address reservations that are tagged with `usage="packet-ccm-auto"` and `service="<service-hash>"` but not on any services. If it finds one:
  * If a service exists that matches the `<service-hash>`, that is an indication that an IP address reservation request was made, not completed at request time, and now is available. Add the IP to the [service spec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#servicespec-v1-core) and ensure is in the pools of the metallb `ConfigMap` with `auto-assign: false`
  * If no service exists that is missing an IP, or none with a matching hash, delete the IP reservation

In all cases of tagging the IP address reservation, we tag the IP reservation with `usage="packet-ccm-auto"` and `service="<service-hash>"` where `<service-hash>` is the sha256 hash of `<namespace>.<service-name>`. We do this so that the name of the service does not leak out to Packet itself.

IP addresses always are created `/32`, and are tagged as `"packet-ccm-auto"`

### Language

In order to ease understanding, we use several different terms for an IP address:

* Requested: A dedicated `/32` IP address has been requested for the service from Packet. It may be returned immediately, or it may need to wait for Packet intervention.
* Reserved: A dedicated `/32` IP address has been reserved for the service from Packet.
* Assigned: The dedicated IP address has been marked on the service as `Service.Spec.LoadBalancerIP` as assigned.
* Mapped: The dedicated IP address has been added to the metallb `ConfigMap` as available.

From Packet's perspective, the IP reservation is either Requested or Reserved, but not both. For the
load balancer to work, the IP address needs to be all of: Reserved, Assigned, Mapped.
