package packet

import (
	"fmt"
	"io"

	"github.com/packethost/packngo"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog"
)

const (
	packetAuthTokenEnvVar string = "PACKET_AUTH_TOKEN"
	packetProjectIDEnvVar string = "PACKET_PROJECT_ID"
	providerName          string = "packet"
	// ConsumerToken token for packet consumer
	ConsumerToken string = "packet-ccm"
)

// cloudService an internal service that can be initialize and report a name
type cloudService interface {
	name() string
	init(sharedInformer informers.SharedInformerFactory, k8sclient kubernetes.Interface, stop <-chan struct{}) error
}

type cloudInstances interface {
	cloudprovider.Instances
	cloudService
}
type cloudLoadBalancers interface {
	cloudprovider.LoadBalancer
	cloudService
}
type cloudZones interface {
	cloudprovider.Zones
	cloudService
}

// cloud implements cloudprovider.Interface
type cloud struct {
	client       *packngo.Client
	instances    cloudInstances
	zones        cloudZones
	loadBalancer cloudLoadBalancers
	facility     string
}

// Config configuration for a provider, includes authentication token, project ID ID, and optional override URL to talk to a different packet API endpoint
type Config struct {
	AuthToken            string  `json:"apiKey"`
	ProjectID            string  `json:"projectId"`
	BaseURL              *string `json:"base-url,omitempty"`
	DisableLoadBalancer  bool    `json:"disableLoadBalancer,omitempty"`
	Facility             string  `json:"facility,omitempty"`
	LoadBalancerManifest []byte
}

func newCloud(packetConfig Config, client *packngo.Client) (cloudprovider.Interface, error) {
	return &cloud{
		client:       client,
		facility:     packetConfig.Facility,
		instances:    newInstances(client, packetConfig.ProjectID),
		zones:        newZones(client, packetConfig.ProjectID),
		loadBalancer: newLoadBalancers(client, packetConfig.ProjectID, packetConfig.Facility, packetConfig.DisableLoadBalancer, packetConfig.LoadBalancerManifest),
	}, nil
}

func InitializeProvider(packetConfig Config) error {
	// set up our client and create the cloud interface
	client := packngo.NewClientWithAuth("", packetConfig.AuthToken, nil)
	cloud, err := newCloud(packetConfig, client)
	if err != nil {
		return fmt.Errorf("failed to create new cloud handler: %v", err)
	}

	// finally, register
	cloudprovider.RegisterCloudProvider(providerName, func(config io.Reader) (cloudprovider.Interface, error) {
		// by the time we get here, there is no error, as it would have been handled earlier
		return cloud, nil
	})

	return nil
}

// get those elements that are initializable
func (c *cloud) initializable() []cloudService {
	return []cloudService{c.loadBalancer, c.instances, c.zones}
}

// Initialize provides the cloud with a kubernetes client builder and may spawn goroutines
// to perform housekeeping activities within the cloud provider.
func (c *cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	klog.V(5).Info("called Initialize")
	clientset := clientBuilder.ClientOrDie("packet-shared-informers")
	sharedInformer := informers.NewSharedInformerFactory(clientset, 0)
	// initializations of our cloud provider, including spawning any goroutines
	for _, elm := range c.initializable() {
		name := elm.name()
		klog.V(5).Infof("initializing %s", name)
		if err := elm.init(sharedInformer, clientset, stop); err != nil {
			klog.Errorf("%s cloud initialization failed: %s", name, err)
		}
		klog.V(5).Infof("initialized %s", name)
	}
}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
// TODO unimplemented
func (c *cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	klog.V(5).Info("called LoadBalancer")
	return nil, false
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) Instances() (cloudprovider.Instances, bool) {
	klog.V(5).Info("called Instances")
	return c.instances, true
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
func (c *cloud) Zones() (cloudprovider.Zones, bool) {
	klog.V(5).Info("called Zones")
	return c.zones, true
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (c *cloud) Clusters() (cloudprovider.Clusters, bool) {
	klog.V(5).Info("called Clusters")
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (c *cloud) Routes() (cloudprovider.Routes, bool) {
	klog.V(5).Info("called Routes")
	return nil, false
}

// ProviderName returns the cloud provider ID.
func (c *cloud) ProviderName() string {
	klog.V(2).Infof("called ProviderName, returning %s", providerName)
	return providerName
}

// HasClusterID returns true if a ClusterID is required and set
func (c *cloud) HasClusterID() bool {
	klog.V(5).Info("called HasClusterID")
	return false
}
