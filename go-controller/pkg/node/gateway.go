package node

import (
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// Gateway responds to Service and Endpoint K8s events
// and programs OVN gateway functionality.
// It may also spawn threads to ensure the flow tables
// are kept in sync
type Gateway interface {
	informer.ServiceAndEndpointsEventHandler
	Init(factory.NodeWatchFactory) error
	Run(<-chan struct{}, *sync.WaitGroup)
	GetGatewayBridgeIface() string
}

type gateway struct {
	// loadBalancerHealthChecker is a health check server for load-balancer type services
	loadBalancerHealthChecker informer.ServiceAndEndpointsEventHandler
	// portClaimWatcher is for reserving ports for virtual IPs allocated by the cluster on the host
	portClaimWatcher informer.ServiceEventHandler
	// nodePortWatcher is used in Shared GW mode to handle nodePort flows in shared OVS bridge
	nodePortWatcher informer.ServiceAndEndpointsEventHandler
	// localPortWatcher is used in Local GW mode to handle iptables rules and routes for services
	localPortWatcher informer.ServiceEventHandler
	openflowManager  *openflowManager
	nodeIPManager    *addressManager
	initFunc         func() error
	readyFunc        func() (bool, error)
}

func (g *gateway) AddService(svc *kapi.Service) {
	if g.portClaimWatcher != nil {
		g.portClaimWatcher.AddService(svc)
	}
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.AddService(svc)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.AddService(svc)
	}
	if g.localPortWatcher != nil {
		g.localPortWatcher.AddService(svc)
	}
}

func (g *gateway) UpdateService(old, new *kapi.Service) {
	if g.portClaimWatcher != nil {
		g.portClaimWatcher.UpdateService(old, new)
	}
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.UpdateService(old, new)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.UpdateService(old, new)
	}
	if g.localPortWatcher != nil {
		g.localPortWatcher.UpdateService(old, new)
	}
}

func (g *gateway) DeleteService(svc *kapi.Service) {
	if g.portClaimWatcher != nil {
		g.portClaimWatcher.DeleteService(svc)
	}
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.DeleteService(svc)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.DeleteService(svc)
	}
	if g.localPortWatcher != nil {
		g.localPortWatcher.DeleteService(svc)
	}
}

func (g *gateway) SyncServices(objs []interface{}) {
	if g.portClaimWatcher != nil {
		g.portClaimWatcher.SyncServices(objs)
	}
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.SyncServices(objs)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.SyncServices(objs)
	}
	if g.localPortWatcher != nil {
		g.localPortWatcher.SyncServices(objs)
	}
}

func (g *gateway) AddEndpoints(ep *kapi.Endpoints) {
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.AddEndpoints(ep)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.AddEndpoints(ep)
	}
}

func (g *gateway) UpdateEndpoints(old, new *kapi.Endpoints) {
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.UpdateEndpoints(old, new)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.UpdateEndpoints(old, new)
	}
}

func (g *gateway) DeleteEndpoints(ep *kapi.Endpoints) {
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.DeleteEndpoints(ep)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.DeleteEndpoints(ep)
	}
}

func (g *gateway) Init(wf factory.NodeWatchFactory) error {
	err := g.initFunc()
	if err != nil {
		return err
	}
	wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			g.AddService(svc)
		},
		UpdateFunc: func(old, new interface{}) {
			oldSvc := old.(*kapi.Service)
			newSvc := new.(*kapi.Service)
			g.UpdateService(oldSvc, newSvc)
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			g.DeleteService(svc)
		},
	}, g.SyncServices)

	wf.AddEndpointsHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			g.AddEndpoints(ep)
		},
		UpdateFunc: func(old, new interface{}) {
			oldEp := old.(*kapi.Endpoints)
			newEp := new.(*kapi.Endpoints)
			g.UpdateEndpoints(oldEp, newEp)
		},
		DeleteFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			g.DeleteEndpoints(ep)
		},
	}, nil)
	return nil
}

func (g *gateway) Run(stopChan <-chan struct{}, wg *sync.WaitGroup) {
	if g.nodeIPManager != nil {
		g.nodeIPManager.Run(stopChan)
	}

	if g.openflowManager != nil {
		klog.Info("Spawning Conntrack Rule Check Thread")
		wg.Add(1)
		defer wg.Done()
		g.openflowManager.Run(stopChan)
	}
}

func gatewayInitInternal(nodeName, gwIntf, egressGatewayIntf string, subnets []*net.IPNet, gwNextHops []net.IP, nodeAnnotator kube.Annotator) (
	*bridgeConfiguration, *bridgeConfiguration, error) {

	gatewayBridge, err := bridgeForInterface(gwIntf, nodeName, types.PhysicalNetworkName)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Bridge for interface failed for %s", gwIntf)
	}
	var egressGWBridge *bridgeConfiguration
	if egressGatewayIntf != "" {
		egressGWBridge, err = bridgeForInterface(egressGatewayIntf, nodeName, types.PhysicalNetworkExGwName)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "Bridge for interface failed for %s", egressGatewayIntf)
		}
	}

	if config.Gateway.Mode == config.GatewayModeLocal {
		err = setupLocalNodeAccessBridge(nodeName, subnets)
		if err != nil {
			return nil, nil, err
		}
	}
	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return nil, nil, err
	}

	if !config.Gateway.DisablePacketMTUCheck {
		chkPktLengthSupported, err := util.DetectCheckPktLengthSupport(gatewayBridge.bridgeName)
		if err != nil {
			return nil, nil, err
		}

		if !chkPktLengthSupported {
			klog.Warningf("OVS on this node does not support check packet length action in kernel datapath. This "+
				"will cause incoming packets destined to OVN and larger than pod MTU: %d to the node, being dropped "+
				"without sending fragmentation needed", config.Default.MTU)
			config.Gateway.DisablePacketMTUCheck = true
		}
	}

	config := util.L3GatewayConfig{
		Mode:           config.GatewayModeShared,
		ChassisID:      chassisID,
		InterfaceID:    gatewayBridge.interfaceID,
		MACAddress:     gatewayBridge.macAddress,
		IPAddresses:    gatewayBridge.ips,
		NextHops:       gwNextHops,
		NodePortEnable: config.Gateway.NodeportEnable,
		VLANID:         &config.Gateway.VLANID,
	}
	if egressGWBridge != nil {
		config.EgressGWInterfaceID = egressGWBridge.interfaceID
		config.EgressGWMACAddress = egressGWBridge.macAddress
		config.EgressGWIPAddresses = egressGWBridge.ips
	}

	err = util.SetL3GatewayConfig(nodeAnnotator, &config)
	return gatewayBridge, egressGWBridge, err
}

func gatewayReady(patchPort string) (bool, error) {
	// Get ofport of patchPort
	_, _, err := util.GetOVSOfPort("--if-exists", "get", "interface", patchPort, "ofport")
	if err != nil {
		return false, nil
	}
	klog.Info("Gateway is ready")
	return true, nil
}

func (g *gateway) GetGatewayBridgeIface() string {
	return g.openflowManager.defaultBridge.bridgeName
}

// getMaxFrameLength returns the maximum frame size (ignoring VLAN header) that a gateway can handle
func getMaxFrameLength() int {
	return config.Default.MTU + 14
}

type bridgeConfiguration struct {
	bridgeName  string
	uplinkName  string
	ips         []*net.IPNet
	interfaceID string
	macAddress  net.HardwareAddr
	patchPort   string
	ofPortPatch string
	ofPortPhys  string
}

func bridgeForInterface(intfName, nodeName, physicalNetworkName string) (*bridgeConfiguration, error) {
	res := bridgeConfiguration{}
	gwIntf := intfName
	bridgeCreated := false

	if bridgeName, _, err := util.RunOVSVsctl("port-to-br", intfName); err == nil {
		// This is an OVS bridge's internal port
		uplinkName, err := util.GetNicName(bridgeName)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to find nic name for bridge %s", bridgeName)
		}
		res.bridgeName = bridgeName
		res.uplinkName = uplinkName
	} else if _, _, err := util.RunOVSVsctl("br-exists", intfName); err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		bridgeName, err := util.NicToBridge(intfName)
		if err != nil {
			return nil, errors.Wrapf(err, "NicToBridge failed for %s", intfName)
		}
		res.bridgeName = bridgeName
		res.uplinkName = intfName
		gwIntf = bridgeName
		bridgeCreated = true
	} else {
		// gateway interface is an OVS bridge
		uplinkName, err := getIntfName(intfName)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to find intfName for %s", intfName)
		}
		res.bridgeName = intfName
		res.uplinkName = uplinkName
	}
	var err error
	// Now, we get IP addresses from OVS bridge. If IP does not exist,
	// error out.
	res.ips, err = getNetworkInterfaceIPAddresses(gwIntf)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get interface details for %s", gwIntf)
	}

	res.interfaceID, res.macAddress, err = bridgedGatewayNodeSetup(nodeName, res.bridgeName, gwIntf,
		physicalNetworkName, bridgeCreated)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	res.patchPort = "patch-" + res.bridgeName + "_" + nodeName + "-to-br-int"
	return &res, nil
}
