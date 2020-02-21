package packet

import (
	"context"
	"strings"

	"github.com/packethost/packngo"
	"github.com/packethost/packngo/metadata"
	"github.com/pkg/errors"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog"
)

type instances struct {
	client  *packngo.Client
	project string
}

func newInstances(client *packngo.Client, projectID string) *instances {
	return &instances{client, projectID}
}

// cloudService implementation
func (i *instances) name() string {
	return "instances"
}
func (i *instances) init(sharedInformer informers.SharedInformerFactory, k8sclient kubernetes.Interface, stop <-chan struct{}) error {
	return nil
}

// cloudprovider.Instances interface implementation

// NodeAddresses returns the addresses of the specified instance.
func (i *instances) NodeAddresses(_ context.Context, name types.NodeName) ([]v1.NodeAddress, error) {
	klog.V(2).Infof("called NodeAddresses with node name %s", name)
	device, err := deviceByName(i.client, i.project, name)
	if err != nil {
		return nil, err
	}

	return nodeAddresses(device)
}

// NodeAddressesByProviderID returns the addresses of the specified instance.
// The instance is specified using the providerID of the node. The
// ProviderID is a unique identifier of the node. This will not be called
// from the node whose nodeaddresses are being queried. i.e. local metadata
// services cannot be used in this method to obtain nodeaddresses
func (i *instances) NodeAddressesByProviderID(_ context.Context, providerID string) ([]v1.NodeAddress, error) {
	klog.V(2).Infof("called NodeAddressesByProviderID with providerID %s", providerID)
	device, err := i.deviceFromProviderID(providerID)
	if err != nil {
		return nil, err
	}

	return nodeAddresses(device)
}

func nodeAddresses(device *packngo.Device) ([]v1.NodeAddress, error) {
	var addresses []v1.NodeAddress
	addresses = append(addresses, v1.NodeAddress{Type: v1.NodeHostName, Address: device.Hostname})

	var privateIP, publicIP string
	for _, address := range device.Network {
		if address.AddressFamily == int(metadata.IPv4) {
			if address.Public {
				publicIP = address.Address
			} else {
				privateIP = address.Address
			}
		}
	}

	if privateIP == "" {
		return nil, errors.New("could not get private ip")
	}
	addresses = append(addresses, v1.NodeAddress{Type: v1.NodeInternalIP, Address: privateIP})

	if publicIP == "" {
		return nil, errors.New("could not get public ip")
	}
	addresses = append(addresses, v1.NodeAddress{Type: v1.NodeExternalIP, Address: publicIP})

	return addresses, nil
}

// InstanceID returns the cloud provider ID of the node with the specified NodeName.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
func (i *instances) InstanceID(_ context.Context, nodeName types.NodeName) (string, error) {
	klog.V(2).Infof("called InstanceID with node name %s", nodeName)
	device, err := deviceByName(i.client, i.project, nodeName)
	if err != nil {
		return "", err
	}

	klog.V(2).Infof("InstanceID for %s: %s", nodeName, device.ID)
	return device.ID, nil
}

// InstanceType returns the type of the specified instance.
func (i *instances) InstanceType(_ context.Context, nodeName types.NodeName) (string, error) {
	klog.V(2).Infof("called InstanceType with node name %s", nodeName)
	device, err := deviceByName(i.client, i.project, nodeName)
	if err != nil {
		return "", err
	}

	return device.Plan.Slug, nil
}

// InstanceTypeByProviderID returns the type of the specified instance.
func (i *instances) InstanceTypeByProviderID(_ context.Context, providerID string) (string, error) {
	klog.V(2).Infof("called InstanceTypeByProviderID with providerID %s", providerID)
	device, err := i.deviceFromProviderID(providerID)
	if err != nil {
		return "", err
	}

	return device.Plan.Slug, nil
}

// AddSSHKeyToAllInstances adds an SSH public key as a legal identity for all instances
// expected format for the key is standard ssh-keygen format: <protocol> <blob>
func (i *instances) AddSSHKeyToAllInstances(_ context.Context, user string, keyData []byte) error {
	klog.V(2).Info("called AddSSHKeyToAllInstances")
	return cloudprovider.NotImplemented
}

// CurrentNodeName returns the name of the node we are currently running on
// On most clouds (e.g. GCE) this is the hostname, so we provide the hostname
func (i *instances) CurrentNodeName(_ context.Context, nodeName string) (types.NodeName, error) {
	klog.V(2).Infof("called CurrentNodeName with nodeName %s", nodeName)
	return types.NodeName(nodeName), nil
}

// InstanceExistsByProviderID returns true if the instance for the given provider id still is running.
// If false is returned with no error, the instance will be immediately deleted by the cloud controller manager.
func (i *instances) InstanceExistsByProviderID(_ context.Context, providerID string) (bool, error) {
	klog.V(2).Infof("called InstanceExistsByProviderID with providerID %s", providerID)
	_, err := i.deviceFromProviderID(providerID)
	switch {
	case err != nil && err == cloudprovider.InstanceNotFound:
		return false, nil
	case err != nil:
		return false, err
	}

	return true, nil
}

// InstanceShutdownByProviderID returns true if the instance is shutdown in cloudprovider
func (i *instances) InstanceShutdownByProviderID(_ context.Context, providerID string) (bool, error) {
	klog.V(2).Infof("called InstanceShutdownByProviderID with providerID %s", providerID)
	device, err := i.deviceFromProviderID(providerID)
	if err != nil {
		return false, err
	}

	return device.State == "inactive", nil
}

func deviceByID(client *packngo.Client, id string) (*packngo.Device, error) {
	klog.V(2).Infof("called deviceByID with ID %s", id)
	device, _, err := client.Devices.Get(id, nil)
	if isNotFound(err) {
		return nil, cloudprovider.InstanceNotFound
	}
	return device, err
}

// deviceByName returns an instance thats hostname matches the kubernetes node.Name
func deviceByName(client *packngo.Client, projectID string, nodeName types.NodeName) (*packngo.Device, error) {
	klog.V(2).Infof("called deviceByName with projectID %s nodeName %s", projectID, nodeName)
	if string(nodeName) == "" {
		return nil, errors.New("node name cannot be empty string")
	}
	devices, _, err := client.Devices.List(projectID, nil)
	if err != nil {
		return nil, err
	}

	for _, device := range devices {
		if device.Hostname == string(nodeName) {
			klog.V(2).Infof("Found device for nodeName %s", nodeName)
			klog.V(3).Infof("%#v", device)
			return &device, nil
		}
	}

	return nil, cloudprovider.InstanceNotFound
}

// deviceIDFromProviderID returns a device's ID from providerID.
//
// The providerID spec should be retrievable from the Kubernetes
// node object. The expected format is: packet://device-id
func deviceIDFromProviderID(providerID string) (string, error) {
	klog.V(2).Infof("called deviceIDFromProviderID with providerID %s", providerID)
	if providerID == "" {
		return "", errors.New("providerID cannot be empty string")
	}

	split := strings.Split(providerID, "://")
	if len(split) != 2 {
		return "", errors.Errorf("unexpected providerID format: %s, format should be: packet://device-id", providerID)
	}

	if split[0] != providerName {
		return "", errors.Errorf("provider name from providerID should be packet: %s", providerID)
	}

	return split[1], nil
}

// deviceFromProviderID uses providerID to get the device id and return the device
func (i *instances) deviceFromProviderID(providerID string) (*packngo.Device, error) {
	klog.V(2).Infof("called deviceFromProviderID with providerID %s", providerID)
	id, err := deviceIDFromProviderID(providerID)
	if err != nil {
		return nil, err
	}

	return deviceByID(i.client, id)
}
