// +build linux

package node

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"k8s.io/klog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	kubeutilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/v1/helper"
	kubeletapi "k8s.io/kubernetes/pkg/kubelet/apis/cri"
	kruntimeapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
	ktypes "k8s.io/kubernetes/pkg/kubelet/types"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	kexec "k8s.io/utils/exec"

	networkapi "github.com/openshift/api/network/v1"
	networkclient "github.com/openshift/client-go/network/clientset/versioned"
	networkinformers "github.com/openshift/client-go/network/informers/externalversions"
	"github.com/openshift/library-go/pkg/network/networkutils"
	"github.com/openshift/origin/pkg/network/common"
	"github.com/openshift/origin/pkg/network/node/cniserver"
	"github.com/openshift/origin/pkg/network/node/ovs"
)

const hostLocalDataDir = "/var/lib/cni/networks"
const cniBinDir = "/opt/cni/bin"

type osdnPolicy interface {
	Name() string
	Start(node *OsdnNode) error
	SupportsVNIDs() bool

	AddNetNamespace(netns *networkapi.NetNamespace)
	UpdateNetNamespace(netns *networkapi.NetNamespace, oldNetID uint32)
	DeleteNetNamespace(netns *networkapi.NetNamespace)

	GetVNID(namespace string) (uint32, error)
	GetNamespaces(vnid uint32) []string
	GetMulticastEnabled(vnid uint32) bool

	EnsureVNIDRules(vnid uint32)
	SyncVNIDRules()
}

type OsdnNodeConfig struct {
	PluginName string
	Hostname   string
	SelfIP     string
	MTU        uint32

	NetworkClient networkclient.Interface
	KClient       kubernetes.Interface
	Recorder      record.EventRecorder

	KubeInformers    informers.SharedInformerFactory
	NetworkInformers networkinformers.SharedInformerFactory

	IPTablesSyncPeriod time.Duration
	ProxyMode          kubeproxyconfig.ProxyMode
	MasqueradeBit      *int32
}

type OsdnNode struct {
	policy             osdnPolicy
	kClient            kubernetes.Interface
	networkClient      networkclient.Interface
	recorder           record.EventRecorder
	oc                 *ovsController
	networkInfo        *common.NetworkInfo
	podManager         *podManager
	localSubnetCIDR    string
	localIP            string
	hostName           string
	useConnTrack       bool
	iptablesSyncPeriod time.Duration
	mtu                uint32

	// Synchronizes operations on egressPolicies
	egressPoliciesLock sync.Mutex
	egressPolicies     map[uint32][]networkapi.EgressNetworkPolicy
	egressDNS          *common.EgressDNS

	kubeInformers    informers.SharedInformerFactory
	networkInformers networkinformers.SharedInformerFactory

	// Holds runtime endpoint shim to make SDN <-> runtime communication
	runtimeService kubeletapi.RuntimeService

	egressIP *egressIPWatcher
}

// Called by higher layers to create the plugin SDN node instance
func New(c *OsdnNodeConfig) (*OsdnNode, error) {
	var policy osdnPolicy
	var pluginId int
	var minOvsVersion string
	var useConnTrack bool
	switch strings.ToLower(c.PluginName) {
	case networkutils.SingleTenantPluginName:
		policy = NewSingleTenantPlugin()
		pluginId = 0
	case networkutils.MultiTenantPluginName:
		policy = NewMultiTenantPlugin()
		pluginId = 1
	case networkutils.NetworkPolicyPluginName:
		policy = NewNetworkPolicyPlugin()
		pluginId = 2
		minOvsVersion = "2.6.0"
		useConnTrack = true
	default:
		// Not an OpenShift plugin
		return nil, nil
	}
	klog.Infof("Initializing SDN node of type %q with configured hostname %q (IP %q), iptables sync period %q", c.PluginName, c.Hostname, c.SelfIP, c.IPTablesSyncPeriod.String())

	if useConnTrack && c.ProxyMode != kubeproxyconfig.ProxyModeIPTables {
		return nil, fmt.Errorf("%q plugin is not compatible with proxy-mode %q", c.PluginName, c.ProxyMode)
	}

	if err := c.setNodeIP(); err != nil {
		return nil, err
	}

	ovsif, err := ovs.New(kexec.New(), Br0, minOvsVersion)
	if err != nil {
		return nil, err
	}
	oc := NewOVSController(ovsif, pluginId, useConnTrack, c.SelfIP)

	plugin := &OsdnNode{
		policy:             policy,
		kClient:            c.KClient,
		networkClient:      c.NetworkClient,
		recorder:           c.Recorder,
		oc:                 oc,
		podManager:         newPodManager(c.KClient, policy, c.MTU, oc),
		localIP:            c.SelfIP,
		hostName:           c.Hostname,
		useConnTrack:       useConnTrack,
		iptablesSyncPeriod: c.IPTablesSyncPeriod,
		mtu:                c.MTU,
		egressPolicies:     make(map[uint32][]networkapi.EgressNetworkPolicy),
		egressDNS:          common.NewEgressDNS(),
		kubeInformers:      c.KubeInformers,
		networkInformers:   c.NetworkInformers,
		egressIP:           newEgressIPWatcher(oc, c.SelfIP, c.MasqueradeBit),
	}

	RegisterMetrics()

	return plugin, nil
}

// Set node IP if required
func (c *OsdnNodeConfig) setNodeIP() error {
	if len(c.Hostname) == 0 {
		output, err := kexec.New().Command("uname", "-n").CombinedOutput()
		if err != nil {
			return err
		}
		c.Hostname = strings.TrimSpace(string(output))
		klog.Infof("Resolved hostname to %q", c.Hostname)
	}

	if len(c.SelfIP) == 0 {
		var err error
		c.SelfIP, err = GetNodeIP(c.Hostname)
		if err != nil {
			klog.V(5).Infof("Failed to determine node address from hostname %s; using default interface (%v)", c.Hostname, err)
			var defaultIP net.IP
			defaultIP, err = kubeutilnet.ChooseHostInterface()
			if err != nil {
				return err
			}
			c.SelfIP = defaultIP.String()
			klog.Infof("Resolved IP address to %q", c.SelfIP)
		}
	}

	if _, _, err := GetLinkDetails(c.SelfIP); err != nil {
		if err == ErrorNetworkInterfaceNotFound {
			err = fmt.Errorf("node IP %q is not a local/private address (hostname %q)", c.SelfIP, c.Hostname)
		}
		utilruntime.HandleError(fmt.Errorf("Unable to find network interface for node IP; some features will not work! (%v)", err))
	}

	return nil
}

func GetNodeIP(nodeName string) (string, error) {
	ip := net.ParseIP(nodeName)
	if ip == nil {
		addrs, err := net.LookupIP(nodeName)
		if err != nil {
			return "", fmt.Errorf("Failed to lookup IP address for node %s: %v", nodeName, err)
		}
		for _, addr := range addrs {
			// Skip loopback and non IPv4 addrs
			if addr.IsLoopback() || addr.To4() == nil {
				klog.V(5).Infof("Skipping loopback/non-IPv4 addr: %q for node %s", addr.String(), nodeName)
				continue
			}
			ip = addr
			break
		}
	} else if ip.IsLoopback() || ip.To4() == nil {
		klog.V(5).Infof("Skipping loopback/non-IPv4 addr: %q for node %s", ip.String(), nodeName)
		ip = nil
	}

	if ip == nil || len(ip.String()) == 0 {
		return "", fmt.Errorf("Failed to obtain IP address from node name: %s", nodeName)
	}
	return ip.String(), nil
}

var (
	ErrorNetworkInterfaceNotFound = fmt.Errorf("could not find network interface")
)

func GetLinkDetails(ip string) (netlink.Link, *net.IPNet, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, nil, err
	}

	for _, link := range links {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			klog.Warningf("Could not get addresses of interface %q: %v", link.Attrs().Name, err)
			continue
		}

		for _, addr := range addrs {
			if addr.IP.String() == ip {
				_, ipNet, err := net.ParseCIDR(addr.IPNet.String())
				if err != nil {
					return nil, nil, fmt.Errorf("could not parse CIDR network from address %q: %v", ip, err)
				}
				return link, ipNet, nil
			}
		}
	}

	return nil, nil, ErrorNetworkInterfaceNotFound
}

func (node *OsdnNode) Start() error {
	klog.V(2).Infof("Starting openshift-sdn network plugin")

	if err := validateNetworkPluginName(node.networkClient, node.policy.Name()); err != nil {
		return fmt.Errorf("failed to validate network configuration: %v", err)
	}

	var err error
	node.networkInfo, err = common.GetNetworkInfo(node.networkClient)
	if err != nil {
		return fmt.Errorf("failed to get network information: %v", err)
	}

	hostIPNets, _, err := common.GetHostIPNetworks([]string{Tun0})
	if err != nil {
		return fmt.Errorf("failed to get host network information: %v", err)
	}
	if err := node.networkInfo.CheckHostNetworks(hostIPNets); err != nil {
		// checkHostNetworks() errors *should* be fatal, but we didn't used to check this, and we can't break (mostly-)working nodes on upgrade.
		utilruntime.HandleError(fmt.Errorf("Local networks conflict with SDN; this will eventually cause problems: %v", err))
	}

	node.localSubnetCIDR, err = node.getLocalSubnet()
	if err != nil {
		return err
	}

	var cidrList []string
	for _, cn := range node.networkInfo.ClusterNetworks {
		cidrList = append(cidrList, cn.ClusterCIDR.String())
	}
	nodeIPTables := newNodeIPTables(cidrList, node.iptablesSyncPeriod, !node.useConnTrack, node.networkInfo.VXLANPort)

	if err = nodeIPTables.Setup(); err != nil {
		return fmt.Errorf("failed to set up iptables: %v", err)
	}

	networkChanged, existingPods, err := node.SetupSDN()
	if err != nil {
		return fmt.Errorf("node SDN setup failed: %v", err)
	}

	hsw := newHostSubnetWatcher(node.oc, node.localIP, node.networkInfo)
	hsw.Start(node.networkInformers)

	if err = node.policy.Start(node); err != nil {
		return err
	}
	if node.policy.SupportsVNIDs() {
		if err := node.SetupEgressNetworkPolicy(); err != nil {
			return err
		}
		if err := node.egressIP.Start(node.networkInformers, nodeIPTables); err != nil {
			return err
		}
	}
	if !node.useConnTrack {
		node.watchServices()
	}

	klog.V(2).Infof("Starting openshift-sdn pod manager")
	if err := node.podManager.Start(cniserver.CNIServerRunDir, node.localSubnetCIDR,
		node.networkInfo.ClusterNetworks, node.networkInfo.ServiceNetwork.String()); err != nil {
		return err
	}

	if networkChanged && len(existingPods) > 0 {
		err := node.reattachPods(existingPods)
		if err != nil {
			return err
		}
	}

	go kwait.Forever(node.policy.SyncVNIDRules, time.Hour)
	go kwait.Forever(func() {
		gatherPeriodicMetrics(node.oc.ovs)
	}, time.Minute*2)

	return nil
}

// reattachPods takes an array containing the information about pods that had been
// attached to the OVS bridge before restart, and either reattaches or kills each of the
// corresponding pods.
func (node *OsdnNode) reattachPods(existingPods map[string]podNetworkInfo) error {
	sandboxes, err := node.getPodSandboxes()
	if err != nil {
		return err
	}

	failed := []*kruntimeapi.PodSandbox{}
	for sandboxID, podInfo := range existingPods {
		sandbox, ok := sandboxes[sandboxID]
		if !ok {
			klog.V(5).Infof("Sandbox for pod with IP %s no longer exists", podInfo.ip)
			continue
		}
		if _, err := netlink.LinkByName(podInfo.vethName); err != nil {
			klog.Infof("Interface %s for pod '%s/%s' no longer exists", podInfo.vethName, sandbox.Metadata.Namespace, sandbox.Metadata.Name)
			failed = append(failed, sandbox)
			continue
		}

		req := &cniserver.PodRequest{
			Command:      cniserver.CNI_ADD,
			PodNamespace: sandbox.Metadata.Namespace,
			PodName:      sandbox.Metadata.Name,
			SandboxID:    sandboxID,
			HostVeth:     podInfo.vethName,
			AssignedIP:   podInfo.ip,
			Result:       make(chan *cniserver.PodResult),
		}
		klog.Infof("Reattaching pod '%s/%s' to SDN", req.PodNamespace, req.PodName)
		// NB: we don't need to worry about locking here because the cniserver
		// isn't running for real yet.
		if _, err := node.podManager.handleCNIRequest(req); err != nil {
			klog.Warningf("Could not reattach pod '%s/%s' to SDN: %v", req.PodNamespace, req.PodName, err)
			failed = append(failed, sandbox)
		}
	}

	// Kill any remaining pods in another thread, after letting SDN startup proceed
	go node.killFailedPods(failed)

	return nil
}

func (node *OsdnNode) killFailedPods(failed []*kruntimeapi.PodSandbox) {
	// Kill pods we couldn't recover; they will get restarted and then
	// we'll be able to set them up correctly
	for _, sandbox := range failed {
		podRef := &corev1.ObjectReference{Kind: "Pod", Name: sandbox.Metadata.Name, Namespace: sandbox.Metadata.Namespace, UID: types.UID(sandbox.Metadata.Uid)}
		node.recorder.Eventf(podRef, corev1.EventTypeWarning, "NetworkFailed", "The pod's network interface has been lost and the pod will be stopped.")

		klog.V(5).Infof("Killing pod '%s/%s' sandbox", podRef.Namespace, podRef.Name)
		if err := node.runtimeService.StopPodSandbox(sandbox.Id); err != nil {
			klog.Warningf("Failed to kill pod '%s/%s' sandbox: %v", podRef.Namespace, podRef.Name, err)
		}
	}
}

// FIXME: this should eventually go into kubelet via a CNI UPDATE/CHANGE action
// See https://github.com/containernetworking/cni/issues/89
func (node *OsdnNode) UpdatePod(pod corev1.Pod) error {
	filter := &kruntimeapi.PodSandboxFilter{
		LabelSelector: map[string]string{ktypes.KubernetesPodUIDLabel: string(pod.UID)},
	}
	sandboxID, err := node.getPodSandboxID(filter)
	if err != nil {
		return err
	}

	req := &cniserver.PodRequest{
		Command:      cniserver.CNI_UPDATE,
		PodNamespace: pod.Namespace,
		PodName:      pod.Name,
		SandboxID:    sandboxID,
		Result:       make(chan *cniserver.PodResult),
	}

	// Send request and wait for the result
	_, err = node.podManager.handleCNIRequest(req)
	return err
}

func (node *OsdnNode) GetLocalPods(namespace string) ([]corev1.Pod, error) {
	fieldSelector := fields.Set{"spec.nodeName": node.hostName}.AsSelector()
	opts := metav1.ListOptions{
		LabelSelector: labels.Everything().String(),
		FieldSelector: fieldSelector.String(),
	}
	podList, err := node.kClient.CoreV1().Pods(namespace).List(opts)
	if err != nil {
		return nil, err
	}

	// Filter running pods
	pods := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			pods = append(pods, pod)
		}
	}
	return pods, nil
}

func isServiceChanged(oldsvc, newsvc *corev1.Service) bool {
	if len(oldsvc.Spec.Ports) == len(newsvc.Spec.Ports) {
		for i := range oldsvc.Spec.Ports {
			if oldsvc.Spec.Ports[i].Protocol != newsvc.Spec.Ports[i].Protocol ||
				oldsvc.Spec.Ports[i].Port != newsvc.Spec.Ports[i].Port {
				return true
			}
		}
		return false
	}
	return true
}

func (node *OsdnNode) watchServices() {
	funcs := common.InformerFuncs(&kapi.Service{}, node.handleAddOrUpdateService, node.handleDeleteService)
	node.kubeInformers.Core().V1().Services().Informer().AddEventHandler(funcs)
}

func (node *OsdnNode) handleAddOrUpdateService(obj, oldObj interface{}, eventType watch.EventType) {
	serv := obj.(*corev1.Service)
	// Ignore headless/external services
	if !helper.IsServiceIPSet(serv) {
		return
	}

	klog.V(5).Infof("Watch %s event for Service %q", eventType, serv.Name)
	oldServ, exists := oldObj.(*corev1.Service)
	if exists {
		if !isServiceChanged(oldServ, serv) {
			return
		}
		node.DeleteServiceRules(oldServ)
	}

	netid, err := node.policy.GetVNID(serv.Namespace)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Skipped adding service rules for serviceEvent: %v, Error: %v", eventType, err))
		return
	}

	node.AddServiceRules(serv, netid)
	node.policy.EnsureVNIDRules(netid)
}

func (node *OsdnNode) handleDeleteService(obj interface{}) {
	serv := obj.(*corev1.Service)
	// Ignore headless/external services
	if !helper.IsServiceIPSet(serv) {
		return
	}

	klog.V(5).Infof("Watch %s event for Service %q", watch.Deleted, serv.Name)
	node.DeleteServiceRules(serv)
}

func validateNetworkPluginName(networkClient networkclient.Interface, pluginName string) error {
	// Detect any plugin mismatches between node and master
	clusterNetwork, err := networkClient.NetworkV1().ClusterNetworks().Get(networkapi.ClusterNetworkDefault, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		return fmt.Errorf("master has not created a default cluster network, network plugin %q can not start", pluginName)
	case err != nil:
		return fmt.Errorf("cannot fetch %q cluster network: %v", networkapi.ClusterNetworkDefault, err)
	}
	if clusterNetwork.PluginName != strings.ToLower(pluginName) {
		return fmt.Errorf("detected network plugin mismatch between OpenShift node(%q) and master(%q)", pluginName, clusterNetwork.PluginName)
	}
	return nil
}
