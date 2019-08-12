package packet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/packethost/packet-ccm/packet/metallb"
	"github.com/packethost/packngo"

	v1 "k8s.io/api/core/v1"

	//"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"

	//"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

const (
	asn                   = 65000
	bufferSize            = 4096
	checkLoopTimerSeconds = 60
)

type loadBalancers struct {
	client    *packngo.Client
	k8sclient kubernetes.Interface
	project   string
	disabled  bool
	facility  string
	manifest  []byte
}

func newLoadBalancers(client *packngo.Client, projectID, facility string, disabled bool, manifest []byte) *loadBalancers {
	return &loadBalancers{client, nil, projectID, disabled, facility, manifest}
}

func (l *loadBalancers) name() string {
	return "loadbalancer"
}
func (l *loadBalancers) init(sharedInformer informers.SharedInformerFactory, k8sclient kubernetes.Interface, stop <-chan struct{}) error {
	klog.V(2).Info("loadBalancers.init(): started")
	l.k8sclient = k8sclient
	if l.disabled {
		klog.V(2).Info("loadBalancers disabled, not initializing")
		return nil
	}
	// enable BGP
	klog.V(2).Info("loadBalancers.init(): enabling BGP on project")
	if err := l.enableBGP(); err != nil {
		return fmt.Errorf("failed to enable BGP on project %s: %v", l.project, err)
	}
	// deploy metallb
	klog.V(2).Info("loadBalancers.init(): deploying metallb")
	if err := l.deployMetalLB(); err != nil {
		return fmt.Errorf("failed to deploy metallb: %v", err)
	}
	// start a goroutine to watch for nodes added/removed and update them
	klog.V(2).Info("loadBalancers.init(): starting node watcher")
	if err := l.startNodesWatcher(sharedInformer, stop); err != nil {
		return fmt.Errorf("failed to start nodes watcher: %v", err)
	}
	// start a goroutine to watch for service changes
	klog.V(2).Info("loadBalancers.init(): starting services watcher")
	if err := l.startServicesWatcher(sharedInformer, stop); err != nil {
		return fmt.Errorf("failed to start services watcher: %v", err)
	}
	return nil
}

// implementation of cloudprovider.LoadBalancer
// we do this via metallb, not directly, so none of this works... for now.

func (l *loadBalancers) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	return nil, false, nil
}
func (l *loadBalancers) GetLoadBalancerName(ctx context.Context, clusterName string, service *v1.Service) string {
	return ""
}
func (l *loadBalancers) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	return nil, nil
}
func (l *loadBalancers) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	return nil
}
func (l *loadBalancers) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
	return nil
}

// utility funcs

// enableBGP enable bgp on the project
func (l *loadBalancers) enableBGP() error {
	req := packngo.CreateBGPConfigRequest{
		Asn:            asn,
		DeploymentType: "local",
		UseCase:        "kubernetes-load-balancer",
	}
	_, err := l.client.BGPConfig.Create(l.project, req)
	return err
}

// deployMetalLB deploy the metallb config to the kubernetes cluster
func (l *loadBalancers) deployMetalLB() error {
	// read each item in the manifest and deploy it
	// we could use gopkg.in/yaml.v2 or even k8s.io/apimachinery/pkg/util/yaml
	// but we do not need to marshal these into objects, as the server handles it.
	// We just want to split them via --- per https://yaml.org/spec/1.2/spec.html

	// save to k8s
	manifests := bytes.Split(l.manifest, []byte("---"))
	for i, m := range manifests {
		klog.V(2).Infof("applying manifest %s", m)
		_, err := l.k8sclient.CoreV1().RESTClient().Patch(k8stypes.ApplyPatchType).
			Namespace(metalLBNamespace).
			Resource(configMapResource).
			Name(metalLBConfigMapName).
			Body(m).
			Do().
			Get()
		if err != nil {
			return fmt.Errorf("error applying document %d: %v", i, err)
		}
	}
	klog.V(2).Info("all manifests applied")
	return nil
}

// startNodesWatcher start a goroutine that watches k8s for nodes and updates the bgp config
func (l *loadBalancers) startNodesWatcher(informer informers.SharedInformerFactory, stop <-chan struct{}) error {
	nodes := informer.Core().V1().Nodes()
	cmLister := informer.Core().V1().ConfigMaps().Lister().ConfigMaps(metalLBNamespace)
	nodesLister := nodes.Lister()

	// next make sure existing nodes have it set
	nodeList, err := nodesLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v", err)
	}
	if err := updateSyncNodes(nodeList, cmLister, l.client, l.k8sclient, false); err != nil {
		klog.Errorf("failed to update and sync nodes: %v", err)
	}

	nodesInformer := nodes.Informer()
	nodesInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			n := obj.(*v1.Node)
			if err := updateSyncNodes([]*v1.Node{n}, cmLister, l.client, l.k8sclient, false); err != nil {
				klog.Errorf("failed to update and sync node for add %s: %v", n.Name, err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			n := obj.(*v1.Node)
			if err := updateSyncNodes([]*v1.Node{n}, cmLister, l.client, l.k8sclient, true); err != nil {
				klog.Errorf("failed to update and sync node for remove %s: %v", n.Name, err)
			}
		},
	})
	nodesInformer.Run(stop)

	return nil
}

// startServicesWatcher start a goroutine that watches k8s for services and creates/deletes
// IP reservations
func (l *loadBalancers) startServicesWatcher(informer informers.SharedInformerFactory, stop <-chan struct{}) error {
	services := informer.Core().V1().Services()
	cmLister := informer.Core().V1().ConfigMaps().Lister().ConfigMaps(metalLBNamespace)
	servicesLister := services.Lister()

	// next make sure existing nodes have it set
	servicesList, err := servicesLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v", err)
	}

	if err := updateSyncServices(servicesList, cmLister, l.client, l.k8sclient, l.project, l.facility, false); err != nil {
		klog.Errorf("failed to update and sync services: %v", err)
		return err
	}

	// register to capture all new services
	servicesInformer := services.Informer()
	servicesInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*v1.Service)
			if err := updateSyncServices([]*v1.Service{svc}, cmLister, l.client, l.k8sclient, l.project, l.facility, false); err != nil {
				klog.Errorf("failed to update and sync service for add %s/%s: %v", svc.Namespace, svc.Name, err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*v1.Service)
			if err := updateSyncServices([]*v1.Service{svc}, cmLister, l.client, l.k8sclient, l.project, l.facility, true); err != nil {
				klog.Errorf("failed to update and sync service for remove %s/%s: %v", svc.Namespace, svc.Name, err)
			}
		},
	})
	servicesInformer.Run(stop)

	go func() {
		for {
			select {
			case <-time.After(checkLoopTimerSeconds * time.Second):
				servicesList, err := servicesLister.List(labels.Everything())
				if err != nil {
					klog.Errorf("timed reservations watcher: failed to list nodes: %v", err)
				}

				if err := updateSyncServices(servicesList, cmLister, l.client, l.k8sclient, l.project, l.facility, false); err != nil {
					klog.Errorf("timed reservations watcher: failed to update and sync services: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()

	return nil
}

// ensureNodeBGPEnabled check if the node has bgp enabled, and set it if it does not
func ensureNodeBGPEnabled(id string, client *packngo.Client) error {
	// if we are rnning ccm properly, then the provider ID will be on the node object
	id, err := deviceIDFromProviderID(id)
	if err != nil {
		return err
	}
	// fortunately, this is idempotent, so just create
	req := packngo.CreateBGPSessionRequest{
		AddressFamily: "ipv4",
	}
	_, _, err = client.BGPSessions.Create(id, req)
	return err
}

// getNodePeerAddress get the BGP peer address for a specific node
func getNodePeerAddress(device string, client *packngo.Client) (address string, err error) {
	ips, _, err := client.DeviceIPs.List(device, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get device IPs for device %s: %v", device, err)
	}
	// we need to get the ip address that is all of:
	// - AddressFamily == 4
	// - Public == false
	// - Management == true
	var addr string
	for _, ip := range ips {
		if ip.AddressFamily == 4 && !ip.Public && ip.Management {
			addr = ip.Network
			break
		}
	}
	if addr == "" {
		return addr, errors.New("no matching IP address found that is private+ipv4+management")
	}
	return addr, nil
}

// addNodePeer update the configmap to ensure that the given node has the given peer
func addNodePeer(config *metallb.ConfigFile, nodeName string, peer string) *metallb.ConfigFile {
	ns := metallb.NodeSelector{
		MatchLabels: map[string]string{
			hostnameKey: nodeName,
		},
	}
	p := metallb.Peer{
		MyASN:         localASN,
		ASN:           peerASN,
		Addr:          peer,
		NodeSelectors: []metallb.NodeSelector{ns},
	}
	config.AddPeer(&p)
	return config
}

func saveUpdatedConfigMap(cfg *metallb.ConfigFile, client kubernetes.Interface) error {
	b, err := cfg.Bytes()
	if err != nil {
		return fmt.Errorf("error converting configmap to bytes: %v", err)
	}

	// save to k8s
	_, err = client.CoreV1().RESTClient().Patch(k8stypes.ApplyPatchType).
		Namespace(metalLBNamespace).
		Resource(configMapResource).
		Name(metalLBConfigMapName).
		Body(b).
		Do().
		Get()

	return err
}

// updateSyncServices for one service, ensure that an IP address reservation exists and is added to the
// service, or at least a request exists for it. Update the metallb configmap as well.
func updateSyncServices(svcs []*v1.Service, lister listerv1.ConfigMapNamespaceLister, client *packngo.Client, k8sclient kubernetes.Interface, project, facility string, remove bool) error {
	var err error
	// get IP address reservations and check if they any exists for this svc
	ips, _, err := client.ProjectIPs.List(project)
	if err != nil {
		return fmt.Errorf("unable to retrieve IP reservations for project %s: %v", project, err)
	}
	// get the configmap
	config, err := getMetalConfigMap(lister)
	if err != nil {
		return fmt.Errorf("unable to retrieve metallb config map: %v", err)
	}

	for _, svc := range svcs {
		// filter on type: only take those that are of type=LoadBalancer
		if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
			continue
		}

		svcName := serviceRep(svc)
		svcTag := serviceTag(svc)
		svcIP := svc.Spec.LoadBalancerIP
		ipReservation := ipReservationByTags([]string{svcTag, packetTag}, ips)

		klog.V(2).Infof("processing %s with existing IP assignment %s", svcName, svcIP)

		if remove {
			// REMOVAL
			// get the IPs and see if there is anything to clean up
			if ipReservation == nil {
				klog.V(2).Infof("no IP reservation found for %s, nothing to delete", svcName)
				return nil
			}
			// delete the reservation
			_, err = client.ProjectIPs.Remove(ipReservation.ID)
			if err != nil {
				return fmt.Errorf("failed to remove IP address reservation %s from project: %v", ipReservation.String(), err)
			}
			// remove it from the configmap
			if err = unmapIP(config, ipReservation.String(), k8sclient); err != nil {
				return fmt.Errorf("error mapping IP %s: %v", svcName, err)
			}
		} else {
			// ADDITION
			// if it already has an IP, no need to get it one
			if svcIP == "" {
				klog.V(2).Infof("no IP assigned for service %s; searching reservations", svcName)

				// if no IP found, request a new one
				if ipReservation == nil {

					// if we did not find an IP reserved, create a request
					klog.V(2).Infof("no IP assignment found for %s, requesting", svcName)
					// create a request
					req := packngo.IPReservationRequest{
						Type:        "public_ipv4",
						Quantity:    1,
						Description: ccmIPDescription,
						Facility:    &facility,
						Tags: []string{
							packetTag,
							svcTag,
						},
					}

					ipReservation, _, err = client.ProjectIPs.Request(project, &req)
					if err != nil {
						return fmt.Errorf("failed to request an IP for the load balancer: %v", err)
					}
				}

				// if we have no IP from existing or a new reservation, log it and return
				if ipReservation == nil {
					klog.V(2).Infof("no IP to assign to service %s, will need to wait until it is allocated", svcName)
					return nil
				}

				// we have an IP, either found from existing reservations or a new reservation.
				// map and assign it
				svcIP = ipReservation.String()
				klog.V(2).Infof("service %s has reserved IP %s", svcName, svcIP)

				// assign the IP and save it
				klog.V(2).Infof("assigning IP %s to %s", svcIP, svcName)
				svc.Spec.LoadBalancerIP = svcIP
				_, err = k8sclient.CoreV1().Services(svc.Namespace).Update(svc)
				if err != nil {
					klog.V(2).Infof("failed to update service %s: %v", svcName, err)
					return fmt.Errorf("failed to update service %s: %v", svcName, err)
				}
				klog.V(2).Infof("successfully assigned %s update service %s", svcIP, svcName)
			}
			// Update the service and configmap and save them
			if err = mapIP(config, svcIP, svcName, k8sclient); err != nil {
				return fmt.Errorf("error mapping IP %s: %v", svcName, err)
			}
		}
	}
	return nil
}

func updateSyncNodes(nodes []*v1.Node, lister listerv1.ConfigMapNamespaceLister, client *packngo.Client, k8sclient kubernetes.Interface, remove bool) error {
	var (
		peer string
		err  error
	)
	// get the configmap
	config, err := getMetalConfigMap(lister)
	if err != nil {
		return fmt.Errorf("failed to get metallb config map: %v", err)
	}
	for _, node := range nodes {
		// are we adding or removing the node?
		if remove {
			// go through the peers and see if we have one with our hostname.
			selector := metallb.NodeSelector{
				MatchLabels: map[string]string{
					hostnameKey: node.Name,
				},
			}
			config.RemovePeerBySelector(&selector)
		} else {
			// get the node provider ID
			id := node.Spec.ProviderID
			if id == "" {
				return fmt.Errorf("no provider ID given")
			}
			// ensure BGP is enabled for the node
			if err := ensureNodeBGPEnabled(id, client); err != nil {
				klog.Errorf("could not ensure BGP enabled for node %s: %v", node.Name, err)
			}
			if peer, err = getNodePeerAddress(id, client); err != nil {
				klog.Errorf("could not add metallb node peer address for node %s: %v", node.Name, err)
			}
			config = addNodePeer(config, node.Name, peer)
			if config == nil {
				klog.V(2).Info("config unchanged, not updating")
				return nil
			}
		}
		klog.V(2).Info("config changed, updating")
		err = saveUpdatedConfigMap(config, k8sclient)
	}
	return nil
}

func serviceRep(svc *v1.Service) string {
	if svc == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)
}

func serviceTag(svc *v1.Service) string {
	if svc == nil {
		return ""
	}
	hash := sha256.Sum256([]byte(serviceRep(svc)))
	return fmt.Sprintf("service=%s", hash)
}

func getMetalConfigMap(lister listerv1.ConfigMapNamespaceLister) (*metallb.ConfigFile, error) {
	cm, err := lister.Get(metalLBConfigMapName)
	if err != nil {
		return nil, fmt.Errorf("unable to get metallb configmap %s: %v", metalLBConfigMapName, err)
	}
	var (
		configData []byte
		ok         bool
	)
	if configData, ok = cm.BinaryData["config"]; !ok {
		return nil, errors.New("configmap data has no property 'config'")
	}
	return metallb.ParseConfig(configData)
}

// unmapIP remove a given IP address from the metalllb config map
func unmapIP(config *metallb.ConfigFile, addr string, k8sclient kubernetes.Interface) error {
	klog.V(2).Infof("unmapping IP %s", addr)
	return updateMapIP(config, addr, "", k8sclient, false)
}

// mapIP add a given ip address to the metallb configmap
func mapIP(config *metallb.ConfigFile, addr, svcName string, k8sclient kubernetes.Interface) error {
	klog.V(2).Infof("mapping IP %s", addr)
	return updateMapIP(config, addr, svcName, k8sclient, true)
}
func updateMapIP(config *metallb.ConfigFile, addr, svcName string, k8sclient kubernetes.Interface, add bool) error {
	// update the configmap and save it
	if add {
		autoAssign := false
		config.AddAddressPool(&metallb.AddressPool{
			Protocol:   "bgp",
			Name:       svcName,
			Addresses:  []string{addr},
			AutoAssign: &autoAssign,
		})
	} else {
		config.RemoveAddressPoolByAddress(addr)
	}
	if config == nil {
		klog.V(2).Info("config unchanged, not updating")
		return nil
	}
	klog.V(2).Info("config changed, updating")
	if err := saveUpdatedConfigMap(config, k8sclient); err != nil {
		klog.V(2).Infof("error updating configmap: %v", err)
		return fmt.Errorf("failed to update configmap: %v", err)
	}
	return nil
}
