package ovn

import (
	"fmt"
	"net"
	"strings"
	"time"

	goovn "github.com/ebay/go-ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// Builds the logical switch port name for a given pod.
func podLogicalPortName(pod *kapi.Pod) string {
	return pod.Namespace + "_" + pod.Name
}

func (oc *Controller) syncPods(pods []interface{}) {
	// get the list of logical switch ports (equivalent to pods)
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			klog.Errorf("Spurious object in syncPods: %v", podInterface)
			continue
		}
		annotations, err := util.UnmarshalPodAnnotation(pod.Annotations)
		if util.PodScheduled(pod) && util.PodWantsNetwork(pod) && err == nil {
			logicalPort := podLogicalPortName(pod)
			expectedLogicalPorts[logicalPort] = true
			if err = oc.lsManager.AllocateIPs(pod.Spec.NodeName, annotations.IPs); err != nil {
				klog.Errorf("Couldn't allocate IPs: %s for pod: %s on node: %s"+
					" error: %v", util.JoinIPNetIPs(annotations.IPs, " "), logicalPort,
					pod.Spec.NodeName, err)
			}
		}
	}

	existingLogicalPorts := make([]string, 0)
	// get the list of logical ports from OVN
	nodes, err := oc.watchFactory.GetNodes()
	if err != nil {
		klog.Errorf("Failed to get nodes")
		return
	}
	for _, n := range nodes {
		nodeSwitchPorts, err := oc.ovnNBClient.LSPList(n.Name)
		if err != nil {
			klog.Errorf("Failed to list lsp for switch %s: error %v", n.Name, err)
			continue
		}
		for _, port := range nodeSwitchPorts {
			if port.ExternalID["pod"] == "true" {
				existingLogicalPorts = append(existingLogicalPorts, port.Name)
			}
		}
	}

	for _, existingPort := range existingLogicalPorts {
		if _, ok := expectedLogicalPorts[existingPort]; !ok {
			// not found, delete this logical port
			klog.Infof("Stale logical port found: %s. This logical port will be deleted.", existingPort)
			cmd, err := oc.ovnNBClient.LSPDel(existingPort)
			if err != nil {
				klog.Errorf("Error in getting the cmd to delete pod's logical port %s %v", existingPort, err)
				continue
			}
			err = oc.ovnNBClient.Execute(cmd)
			if err != nil {
				klog.Errorf("Error deleting pod's logical port %s %v", existingPort, err)
				continue
			}
		}
	}
}

func (oc *Controller) deleteLogicalPort(pod *kapi.Pod) {
	oc.deletePodExternalGW(pod)
	if pod.Spec.HostNetwork {
		return
	}
	start := time.Now()
	var ovnExecuteTime time.Duration
	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod: %s", podDesc)
	defer func() {
		klog.Infof("[%s/%s] deleteLogicalPort took %v, OVN Execute time %v", pod.Namespace, pod.Name, time.Since(start), ovnExecuteTime)
	}()

	logicalPort := podLogicalPortName(pod)
	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
		start1 := time.Now()
		// If ovnkube-master restarts, it is also possible the Pod's logical switch port
		// is not readded into the cache. Delete logical switch port anyway.
		err = util.OvnNBLSPDel(oc.ovnNBClient, logicalPort)
		ovnExecuteTime = time.Since(start1)
		if err != nil {
			klog.Errorf(err.Error())
		}

		// Even if the port is not in the cache, IPs annotated in the Pod annotation may already be allocated,
		// need to release them to avoid leakage.
		logicalSwitch := pod.Spec.NodeName
		if logicalSwitch != "" {
			annotation, err := util.UnmarshalPodAnnotation(pod.Annotations)
			if err == nil {
				podIfAddrs := annotation.IPs
				_ = oc.lsManager.ReleaseIPs(logicalSwitch, podIfAddrs)
			}
		}
		return
	}

	// FIXME: if any of these steps fails we need to stop and try again later...

	var cmds []*goovn.OvnCommand
	addrSetCmds, err := oc.deletePodFromNamespace(pod.Namespace, portInfo.name, portInfo.uuid, portInfo.ips)
	if err != nil {
		klog.Errorf(err.Error())
	} else {
		cmds = append(cmds, addrSetCmds...)
	}

	cmd, err := oc.ovnNBClient.LSPDel(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
	} else {
		cmds = append(cmds, cmd)
	}
	start1 := time.Now()
	// execute all the commands together.
	err = oc.ovnNBClient.Execute(cmds...)
	ovnExecuteTime = time.Since(start1)
	if err != nil {
		klog.Errorf("Error deleting logical port %s: %v", portInfo.name, err)
	}

	if err := oc.lsManager.ReleaseIPs(portInfo.logicalSwitch, portInfo.ips); err != nil {
		klog.Errorf(err.Error())
	}

	if config.Gateway.DisableSNATMultipleGWs {
		oc.deletePerPodGRSNAT(pod.Spec.NodeName, portInfo.ips)
	}
	podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	oc.deleteGWRoutesForPod(podNsName, portInfo.ips)

	oc.logicalPortCache.remove(logicalPort)
}

func (oc *Controller) waitForNodeLogicalSwitch(nodeName string) (string, error) {
	// Wait for the node logical switch to be created by the ClusterController.
	// The node switch will be created when the node's logical network infrastructure
	// is created by the node watch.
	var uuid string
	var subnets []*net.IPNet
	if err := wait.PollImmediate(30*time.Millisecond, 30*time.Second, func() (bool, error) {
		subnets, uuid = oc.lsManager.GetSwitchSubnetsAndUUID(nodeName)
		return subnets != nil, nil
	}); err != nil {
		return "", fmt.Errorf("timed out waiting for logical switch %q subnet: %v", nodeName, err)
	}
	return uuid, nil
}

func (oc *Controller) addRoutesGatewayIP(pod *kapi.Pod, podAnnotation *util.PodAnnotation, nodeSubnets []*net.IPNet,
	routingExternalGWs *gatewayInfo, routingPodGWs map[string]*gatewayInfo, hybridOverlayExternalGW net.IP) error {
	// if there are other network attachments for the pod, then check if those network-attachment's
	// annotation has default-route key. If present, then we need to skip adding default route for
	// OVN interface
	networks, err := util.GetPodNetSelAnnotation(pod, util.NetworkAttachmentAnnotation)
	if err != nil {
		return fmt.Errorf("error while getting network attachment definition for [%s/%s]: %v",
			pod.Namespace, pod.Name, err)
	}
	otherDefaultRouteV4 := false
	otherDefaultRouteV6 := false
	for _, network := range networks {
		for _, gatewayRequest := range network.GatewayRequest {
			if utilnet.IsIPv6(gatewayRequest) {
				otherDefaultRouteV6 = true
			} else {
				otherDefaultRouteV4 = true
			}
		}
	}

	for _, podIfAddr := range podAnnotation.IPs {
		isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
		nodeSubnet, err := util.MatchIPNetFamily(isIPv6, nodeSubnets)
		if err != nil {
			return err
		}
		// DUALSTACK FIXME: hybridOverlayExternalGW is not Dualstack
		// When oc.getHybridOverlayExternalGwAnnotation() supports dualstack, return error if no match.
		// If external gateway mode is configured, need to use it for all outgoing traffic, so don't want
		// to fall back to the default gateway here
		if hybridOverlayExternalGW != nil && utilnet.IsIPv6(hybridOverlayExternalGW) != isIPv6 {
			klog.Warningf("Pod %s/%s has no external gateway for %s", pod.Namespace, pod.Name, util.IPFamilyName(isIPv6))
			continue
		}

		gatewayIPnet := util.GetNodeGatewayIfAddr(nodeSubnet)

		otherDefaultRoute := otherDefaultRouteV4
		if isIPv6 {
			otherDefaultRoute = otherDefaultRouteV6
		}
		var gatewayIP net.IP
		hasRoutingExternalGWs := len(routingExternalGWs.gws) > 0
		hasPodRoutingGWs := len(routingPodGWs) > 0
		if otherDefaultRoute || (hybridOverlayExternalGW != nil && !hasRoutingExternalGWs && !hasPodRoutingGWs) {
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				if isIPv6 == utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    clusterSubnet.CIDR,
						NextHop: gatewayIPnet.IP,
					})
				}
			}
			for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
				if isIPv6 == utilnet.IsIPv6CIDR(serviceSubnet) {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    serviceSubnet,
						NextHop: gatewayIPnet.IP,
					})
				}
			}
			if hybridOverlayExternalGW != nil {
				gatewayIP = util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
			}
		} else {
			gatewayIP = gatewayIPnet.IP
		}

		if len(config.HybridOverlay.ClusterSubnets) > 0 {
			// Add a route for each hybrid overlay subnet via the hybrid
			// overlay port on the pod's logical switch.
			nextHop := util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
			for _, clusterSubnet := range config.HybridOverlay.ClusterSubnets {
				if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) == isIPv6 {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    clusterSubnet.CIDR,
						NextHop: nextHop,
					})
				}
			}
		}
		if gatewayIP != nil {
			podAnnotation.Gateways = append(podAnnotation.Gateways, gatewayIP)
		}
	}
	return nil
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) (err error) {
	// If a node does node have an assigned hostsubnet don't wait for the logical switch to appear
	if oc.lsManager.IsNonHostSubnetSwitch(pod.Spec.NodeName) {
		return nil
	}

	var ovnExecuteTime time.Duration
	var podAnnoTime time.Duration
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort took %v, OVN Execute time %v, pod Annotation time: %v",
			pod.Namespace, pod.Name, time.Since(start), ovnExecuteTime, podAnnoTime)
	}()

	logicalSwitch := pod.Spec.NodeName
	lsUUID, err := oc.waitForNodeLogicalSwitch(logicalSwitch)
	if err != nil {
		return err
	}

	portName := podLogicalPortName(pod)
	klog.V(5).Infof("Creating logical port for %s on switch %s [%s]", portName, logicalSwitch, lsUUID)

	var podMac net.HardwareAddr
	var podIfAddrs []*net.IPNet
	var cmds []*goovn.OvnCommand
	var addresses []string
	var cmd *goovn.OvnCommand
	var podCmd *goovn.OvnCommand
	var releaseIPs bool

	opts := make(map[string]string)

	// Check if the pod's logical switch port already exists. If it
	// does don't re-add the port to OVN as this will change its
	// UUID and and the port cache, address sets, and port groups
	// will still have the old UUID.
	lsp, err := oc.ovnNBClient.LSPGet(portName)
	if err != nil {
		if err != goovn.ErrorNotFound && err != goovn.ErrorSchema {
			return fmt.Errorf("unable to get the lsp: %s from the nbdb: %s", portName, err)
		}
	} else {
		// Preserve existing port options
		for k, v := range lsp.Options {
			key, keyOk := k.(string)
			value, valueOk := v.(string)
			if keyOk && valueOk {
				opts[key] = value
			}
		}
	}

	// Bind the port to the node's chassis; prevents ping-ponging between
	// chassis if ovnkube-node isn't running correctly and hasn't cleared
	// out iface-id for an old instance of this pod, and the pod got
	// rescheduled.
	opts["requested-chassis"] = pod.Spec.NodeName

	if lsp == nil {
		podCmd, err = oc.ovnNBClient.LSPAdd(logicalSwitch, lsUUID, portName)
		if err != nil {
			return fmt.Errorf("unable to create the LSPAdd command for port: %s from the nbdb: %v", portName, err)
		}
		// Unique identifier to distinguish interfaces for recreated pods, also set by ovnkube-node
		// ovn-controller will claim the OVS interface only if external_ids:iface-id
		// matches with the Port_Binding.logical_port and external_ids:iface-id-ver matches
		// with the Port_Binding.options:iface-id-ver. This is not mandatory.
		// If Port_binding.options:iface-id-ver is not set, then OVS
		// Interface.external_ids:iface-id-ver if set is ignored.
		// Only set for new LSP for correct ovn-kube upgrade, because for old OVS Interfaces
		// iface-id-ver is not set => ovn-controller won't bind OVS Interface
		opts["iface-id-ver"] = string(pod.UID)
	} else {
		klog.Infof("LSP already exists for port: %s", portName)
	}

	cmd, err = oc.ovnNBClient.LSPSetOptions(portName, opts)
	if err != nil {
		return fmt.Errorf("unable to create the LSPSetOptions command for port: %s from the nbdb: %v", portName, err)
	}
	if podCmd != nil {
		podCmd.Operations[0].Row["options"] = cmd.Operations[0].Row["options"]
	} else {
		podCmd = cmd
	}
	cmds = append(cmds, podCmd)

	// the IPs we allocate in this function need to be released back to the
	// IPAM pool if there is some error in any step of addLogicalPort past
	// the point the IPs were assigned via the IPAM manager.
	// this needs to be done only when releaseIPs is set to true (the case where
	// we truly have assigned podIPs in this call) AND when there is no error in
	// the rest of the functionality of addLogicalPort. It is important to use a
	// named return variable for defer to work correctly.

	defer func() {
		if releaseIPs && err != nil {
			if relErr := oc.lsManager.ReleaseIPs(logicalSwitch, podIfAddrs); relErr != nil {
				klog.Errorf("Error when releasing IPs for node: %s, err: %q",
					logicalSwitch, relErr)
			} else {
				klog.Infof("Released IPs: %s for node: %s", util.JoinIPNetIPs(podIfAddrs, " "), logicalSwitch)
			}
			if addrSetCmds, nsErr := oc.deletePodFromNamespace(pod.Namespace, portName, "", podIfAddrs); nsErr != nil {
				klog.Errorf("Error when deleting pod: %s from namespace: %v", pod.Name, err)
			} else {
				if addrErr := oc.ovnNBClient.Execute(addrSetCmds...); addrErr != nil {
					klog.Errorf("Error removing pod %s IPs from namespace address set: %v", portName, addrErr)
				}
			}
		}
	}()

	needsIP := true
	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations)
	if err == nil {
		podMac = annotation.MAC
		podIfAddrs = annotation.IPs

		// If the pod already has annotations use the existing static
		// IP/MAC from the annotation.
		podCmd.Operations[0].Row["dynamic_addresses"] = ""

		// ensure we have reserved the IPs in the annotation
		if err = oc.lsManager.AllocateIPs(logicalSwitch, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			return fmt.Errorf("unable to ensure IPs allocated for already annotated pod: %s, IPs: %s, error: %v",
				pod.Name, util.JoinIPNetIPs(podIfAddrs, " "), err)
		} else {
			needsIP = false
		}
	}

	if needsIP {
		// try to get the IP from existing port in OVN first
		if lsp != nil {
			podMac, podIfAddrs, err = oc.getPortAddresses(logicalSwitch, lsp)
			if err != nil {
				return fmt.Errorf("failed to get pod addresses for pod %s on node: %s, err: %v",
					portName, logicalSwitch, err)
			}
		}
		needsNewAllocation := false
		// ensure we have reserved the IPs found in OVN
		if len(podIfAddrs) == 0 {
			needsNewAllocation = true
		} else if err = oc.lsManager.AllocateIPs(logicalSwitch, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			klog.Warningf("Unable to allocate IPs found on existing OVN port: %s, for pod %s on node: %s"+
				" error: %v", util.JoinIPNetIPs(podIfAddrs, " "), portName, logicalSwitch, err)

			needsNewAllocation = true
		}
		if needsNewAllocation {
			// Previous attempts to use already configured IPs failed, need to assign new
			podMac, podIfAddrs, err = oc.assignPodAddresses(logicalSwitch)
			if err != nil {
				return fmt.Errorf("failed to assign pod addresses for pod %s on node: %s, err: %v",
					portName, logicalSwitch, err)
			}
		}

		releaseIPs = true
	}

	// Ensure the namespace/nsInfo exists
	routingExternalGWs, routingPodGWs, hybridOverlayExternalGW, addrSetCmds, err := oc.addPodToNamespace(pod.Namespace, podIfAddrs)
	if err != nil {
		return err
	}
	cmds = append(cmds, addrSetCmds...)

	if needsIP {
		var networks []*types.NetworkSelectionElement

		networks, err = util.GetPodNetSelAnnotation(pod, util.DefNetworkAnnotation)
		// handle error cases separately first to ensure binding to err, otherwise the
		// defer will fail
		if err != nil {
			return fmt.Errorf("error while getting custom MAC config for port %q from "+
				"default-network's network-attachment: %v", portName, err)
		} else if networks != nil && len(networks) != 1 {
			err = fmt.Errorf("invalid network annotation size while getting custom MAC config"+
				" for port %q", portName)
			return err
		}

		if networks != nil && networks[0].MacRequest != "" {
			klog.V(5).Infof("Pod %s/%s requested custom MAC: %s", pod.Namespace, pod.Name, networks[0].MacRequest)
			podMac, err = net.ParseMAC(networks[0].MacRequest)
			if err != nil {
				return fmt.Errorf("failed to parse mac %s requested in annotation for pod %s: Error %v",
					networks[0].MacRequest, pod.Name, err)
			}
		}
		podAnnotation := util.PodAnnotation{
			IPs: podIfAddrs,
			MAC: podMac,
		}
		var nodeSubnets []*net.IPNet
		if nodeSubnets, _ = oc.lsManager.GetSwitchSubnetsAndUUID(logicalSwitch); nodeSubnets == nil {
			return fmt.Errorf("cannot retrieve subnet for assigning gateway routes for pod %s, node: %s",
				pod.Name, logicalSwitch)
		}
		err = oc.addRoutesGatewayIP(pod, &podAnnotation, nodeSubnets, routingExternalGWs, routingPodGWs, hybridOverlayExternalGW)
		if err != nil {
			return err
		}
		var marshalledAnnotation map[string]string
		marshalledAnnotation, err = util.MarshalPodAnnotation(&podAnnotation)
		if err != nil {
			return fmt.Errorf("error creating pod network annotation: %v", err)
		}

		klog.V(5).Infof("Annotation values: ip=%v ; mac=%s ; gw=%s\nAnnotation=%s",
			podIfAddrs, podMac, podAnnotation.Gateways, marshalledAnnotation)
		annoStart := time.Now()
		err = oc.kube.SetAnnotationsOnPod(pod.Namespace, pod.Name, marshalledAnnotation)
		podAnnoTime = time.Since(annoStart)
		if err != nil {
			return fmt.Errorf("failed to set annotation on pod %s: %v", pod.Name, err)
		}
		releaseIPs = false
	}

	// if we have any external or pod Gateways, add routes
	gateways := make([]*gatewayInfo, 0)

	if len(routingExternalGWs.gws) > 0 {
		gateways = append(gateways, routingExternalGWs)
	}
	for _, gw := range routingPodGWs {
		if len(gw.gws) > 0 {
			gateways = append(gateways, gw)
		} else {
			klog.Warningf("Found routingPodGW with no gateways ip set for namespace %s", pod.Namespace)
		}
	}

	if len(gateways) > 0 {
		podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		err = oc.addGWRoutesForPod(gateways, podIfAddrs, podNsName, pod.Spec.NodeName)
		if err != nil {
			return err
		}
	} else if config.Gateway.DisableSNATMultipleGWs {
		// Add NAT rules to pods if disable SNAT is set and does not have
		// namespace annotations to go through external egress router
		if err = oc.addPerPodGRSNAT(pod, podIfAddrs); err != nil {
			return err
		}
	}

	// check if this pod is serving as an external GW
	err = oc.addPodExternalGW(pod)
	if err != nil {
		return fmt.Errorf("failed to handle external GW check: %v", err)
	}

	// set addresses on the port
	addresses = make([]string, len(podIfAddrs)+1)
	addresses[0] = podMac.String()
	for idx, podIfAddr := range podIfAddrs {
		addresses[idx+1] = podIfAddr.IP.String()
	}

	// LSP addresses in OVN are a single space-separated value
	lspAddrs := strings.Join(addresses, " ")
	cmd, err = oc.ovnNBClient.LSPSetAddress(portName, lspAddrs)
	if err != nil {
		return fmt.Errorf("unable to create LSPSetAddress command for port: %s", portName)
	}
	podCmd.Operations[0].Row["addresses"] = cmd.Operations[0].Row["addresses"]

	// add external ids
	extIds := map[string]string{"namespace": pod.Namespace, "pod": "true"}
	cmd, err = oc.ovnNBClient.LSPSetExternalIds(portName, extIds)
	if err != nil {
		return fmt.Errorf("unable to create LSPSetExternalIds command for port: %s", portName)
	}
	podCmd.Operations[0].Row["external_ids"] = cmd.Operations[0].Row["external_ids"]

	// CNI depends on the flows from port security, delay setting it until end
	psAddrs := strings.Join(addresses, " ")
	cmd, err = oc.ovnNBClient.LSPSetPortSecurity(portName, psAddrs)
	if err != nil {
		return fmt.Errorf("unable to create LSPSetPortSecurity command for port: %s", portName)
	}
	podCmd.Operations[0].Row["port_security"] = cmd.Operations[0].Row["port_security"]

	start1 := time.Now()
	// execute all the commands together. If a single operation fails, all commands will roll back =>
	// for new Pod no LSP will be created
	var r []string
	r, err = oc.ovnNBClient.ExecuteR(cmds...)
	ovnExecuteTime = time.Since(start1)
	if err != nil {
		return fmt.Errorf("error while creating logical port %s error: %v",
			portName, err)
	}

	if lsp == nil {
		// Grab the LSP's UUID from the creation response
		if len(r) != 1 {
			return fmt.Errorf("unexpected logical switch port %q create response length %v", portName, r)
		}
		lsp, err = oc.ovnNBClient.LSPGetUUID(r[0])
		if err != nil {
			return fmt.Errorf("failed to get the logical switch port: %s from the ovn client, error: %s", portName, err)
		}
		// Sanity check
		if lsp.Name != portName {
			return fmt.Errorf("unexpected logical switch port name %q for uuid %s (expected %q)", lsp.Name, lsp.UUID, portName)
		}
	}

	// Add the pod's logical switch port to the port cache
	portInfo := oc.logicalPortCache.add(logicalSwitch, portName, lsp.UUID, podMac, podIfAddrs)

	// If multicast is allowed and enabled for the namespace, add the port to the allow policy.
	// FIXME: there's a race here with the Namespace multicastUpdateNamespace() handler, but
	// it's rare and easily worked around for now.
	ns, err := oc.watchFactory.GetNamespace(pod.Namespace)
	if err != nil {
		return err
	}
	if oc.multicastSupport && isNamespaceMulticastEnabled(ns.Annotations) {
		if err := podAddAllowMulticastPolicy(oc.ovnNBClient, pod.Namespace, portInfo); err != nil {
			return err
		}
	}
	// observe the pod creation latency metric.
	metrics.RecordPodCreated(pod)
	return nil
}

// Given a node, gets the next set of addresses (from the IPAM) for each of the node's
// subnets to assign to the new pod
func (oc *Controller) assignPodAddresses(nodeName string) (net.HardwareAddr, []*net.IPNet, error) {
	var (
		podMAC   net.HardwareAddr
		podCIDRs []*net.IPNet
		err      error
	)
	podCIDRs, err = oc.lsManager.AllocateNextIPs(nodeName)
	if err != nil {
		return nil, nil, err
	}
	if len(podCIDRs) > 0 {
		podMAC = util.IPAddrToHWAddr(podCIDRs[0].IP)
	}
	return podMAC, podCIDRs, nil
}

// Given a pod and the node on which it is scheduled, get all addresses currently assigned
// to it from the nbdb.
func (oc *Controller) getPortAddresses(nodeName string, lsp *goovn.LogicalSwitchPort) (net.HardwareAddr, []*net.IPNet, error) {
	podMac, podIPs, err := util.ParsePortAddresses(lsp)
	if err != nil {
		return nil, nil, err
	}

	if podMac == nil || len(podIPs) == 0 {
		return nil, nil, nil
	}

	var podIPNets []*net.IPNet

	nodeSubnets, _ := oc.lsManager.GetSwitchSubnetsAndUUID(nodeName)

	for _, ip := range podIPs {
		for _, subnet := range nodeSubnets {
			if subnet.Contains(ip) {
				podIPNets = append(podIPNets,
					&net.IPNet{
						IP:   ip,
						Mask: subnet.Mask,
					})
				break
			}
		}
	}
	return podMac, podIPNets, nil
}
