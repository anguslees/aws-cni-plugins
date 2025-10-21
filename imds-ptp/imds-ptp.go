// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"
	cniversion "github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/coreos/go-iptables/iptables"
	"github.com/j-keck/arping"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/anguslees/aws-cni-plugins/internal/metadata"
	"github.com/anguslees/aws-cni-plugins/internal/procsys"
)

var version string

const trace = false

const (
	// Order matters
	rulePriorityLocalPods   = 30000
	rulePriorityMasq        = 30010
	rulePriorityOutgoingENI = 30020

	masqMark = 0x80

	routeTablePod      = 9
	routeTableENIStart = 10
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// NetConf is our CNI config structure
type NetConf struct {
	types.NetConf

	MTU int `json:"mtu"`
}

func loadConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{}

	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, err
	}

	return n, nil
}

// Mostly based on standard ptp CNI plugin
func setupContainerVeth(netns ns.NetNS, ifName string, mtu int, pr *cniv1.Result) (*cniv1.Interface, *cniv1.Interface, error) {
	// The IPAM result will be something like IP=192.168.3.5/24, GW=192.168.3.1.
	// What we want is really a point-to-point link but veth does not support IFF_POINTTOPOINT.
	// Next best thing would be to let it ARP but set interface to 192.168.3.5/32 and
	// add a route like "192.168.3.0/24 via 192.168.3.1 dev $ifName".
	// Unfortunately that won't work as the GW will be outside the interface's subnet.

	// Our solution is to configure the interface with 192.168.3.5/24, then delete the
	// "192.168.3.0/24 dev $ifName" route that was automatically added. Then we add
	// "192.168.3.1/32 dev $ifName" and "192.168.3.0/24 via 192.168.3.1 dev $ifName".
	// In other words we force all traffic to ARP via the gateway except for GW itself.

	hostInterface := &cniv1.Interface{}
	containerInterface := &cniv1.Interface{}

	err := netns.Do(func(hostNS ns.NetNS) error {
		hostVeth, contVeth0, err := ip.SetupVeth(ifName, mtu, "", hostNS)
		if err != nil {
			return err
		}
		hostInterface.Name = hostVeth.Name
		hostInterface.Mac = hostVeth.HardwareAddr.String()
		containerInterface.Name = contVeth0.Name
		containerInterface.Mac = contVeth0.HardwareAddr.String()
		containerInterface.Sandbox = netns.Path()

		for _, ipc := range pr.IPs {
			// All addresses apply to the container veth interface
			ipc.Interface = cniv1.Int(1)
		}

		pr.Interfaces = []*cniv1.Interface{hostInterface, containerInterface}

		if err = ipam.ConfigureIface(ifName, pr); err != nil {
			return err
		}

		contVeth, err := net.InterfaceByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to look up %q: %v", ifName, err)
		}

		for _, ipc := range pr.IPs {
			// Delete the route that was automatically added
			route := netlink.Route{
				LinkIndex: contVeth.Index,
				Dst: &net.IPNet{
					IP:   ipc.Address.IP.Mask(ipc.Address.Mask),
					Mask: ipc.Address.Mask,
				},
				Scope: netlink.SCOPE_NOWHERE,
			}

			if err := netlink.RouteDel(&route); err != nil {
				return fmt.Errorf("failed to delete route %v: %v", route, err)
			}

			addrBits := 128
			if ipc.Address.IP.To4() != nil {
				addrBits = 32
			}

			for _, r := range []netlink.Route{
				{
					LinkIndex: contVeth.Index,
					Dst: &net.IPNet{
						IP:   ipc.Gateway,
						Mask: net.CIDRMask(addrBits, addrBits),
					},
					Scope: netlink.SCOPE_LINK,
					Src:   ipc.Address.IP,
				},
				{
					LinkIndex: contVeth.Index,
					Dst: &net.IPNet{
						IP:   ipc.Address.IP.Mask(ipc.Address.Mask),
						Mask: ipc.Address.Mask,
					},
					Scope: netlink.SCOPE_UNIVERSE,
					Gw:    ipc.Gateway,
					Src:   ipc.Address.IP,
				},
			} {
				if err := netlink.RouteAdd(&r); err != nil {
					return fmt.Errorf("failed to add route %v: %v", r, err)
				}
			}
		}

		// Send a gratuitous arp for all v4 addresses
		for _, ipc := range pr.IPs {
			if ipc.Address.IP.To4() != nil {
				_ = arping.GratuitousArpOverIface(ipc.Address.IP, *contVeth)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return hostInterface, containerInterface, nil
}

func setupHostVeth(vethName string, result *cniv1.Result) error {
	// hostVeth moved namespaces and may have a new ifindex
	veth, err := netlink.LinkByName(vethName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", vethName, err)
	}

	for _, ipc := range result.IPs {
		maskLen := 128
		if ipc.Address.IP.To4() != nil {
			maskLen = 32
		}

		// NB: this is modified from standard ptp plugin.

		ipn := &net.IPNet{
			IP:   ipc.Gateway,
			Mask: net.CIDRMask(maskLen, maskLen),
		}
		addr := &netlink.Addr{
			IPNet: ipn,
			Scope: int(netlink.SCOPE_LINK), // <- ptp uses SCOPE_UNIVERSE here
		}
		if err = netlink.AddrAdd(veth, addr); err != nil {
			return fmt.Errorf("failed to add IP addr (%#v) to veth: %v", ipn, err)
		}

		ipn = &net.IPNet{
			IP:   ipc.Address.IP,
			Mask: net.CIDRMask(maskLen, maskLen),
		}
		err := netlink.RouteAdd(&netlink.Route{
			LinkIndex: veth.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Dst:       ipn,
		})
		if err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to add route on host: %v", err)
		}
	}

	return nil
}

// Setup ENI interface and primary IP policy route.
func setupHostEniIface(ec2Metadata metadata.EC2MetadataIface, procSys procsys.ProcSys, mtu int, vethName string, eniMAC string, ipVersion int) error {
	ctx := context.TODO()

	imds := metadata.NewTypedIMDS(ec2Metadata)

	getIPs := imds.GetLocalIPv4s
	getSubnet := imds.GetSubnetIPv4CIDRBlock
	defaultRoute := net.IPNet{
		IP:   net.IPv4zero,
		Mask: net.CIDRMask(0, 32),
	}
	family := unix.AF_INET
	maskLen := 32
	iptProto := iptables.ProtocolIPv4
	if ipVersion == 6 {
		getIPs = imds.GetIPv6s
		getSubnet = imds.GetSubnetIPv6CIDRBlocks
		defaultRoute = net.IPNet{
			IP:   net.IPv6zero,
			Mask: net.CIDRMask(0, 128),
		}
		family = unix.AF_INET6
		maskLen = 128
		iptProto = iptables.ProtocolIPv6
	}

	ipt, err := iptables.NewWithProtocol(iptProto)
	if err != nil {
		return err
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	interfaceByMAC := make(map[string]*net.Interface, len(ifaces))
	for i := range ifaces {
		iface := ifaces[i]
		interfaceByMAC[iface.HardwareAddr.String()] = &iface
	}

	primaryMAC, err := imds.GetMAC(ctx)
	if err != nil {
		return err
	}
	primaryIface := interfaceByMAC[primaryMAC]
	if primaryIface == nil {
		return fmt.Errorf("failed to find interface for MAC %s", primaryMAC)
	}

	rule := netlink.NewRule()
	rule.Priority = rulePriorityLocalPods
	rule.Family = family
	rule.Table = routeTablePod
	if err := netlink.RuleAdd(rule); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("failed to add rule: %v", err)
		}
	}

	// kube-proxy DNATs+MASQUERADEs traffic destined to
	// nodeports. The problem is that this rewrites everything to
	// be to/from primaryIP (eth0) *after* policy routing has
	// already chosen some other interface - rp_filter freaks out,
	// packet goes out wrong interface, etc, etc.
	// Solution: mark packets that look like nodeports in iptables
	// (before routing), and ensure the routing chooses eth0.
	// Sigh. :(
	iptRules := [][]string{
		{
			"-m", "comment", "--comment", "AWS, primary ENI",
			"-i", primaryIface.Name,
			"-m", "addrtype", "--dst-type", "LOCAL", "--limit-iface-in",
			"-j", "CONNMARK", "--set-mark", fmt.Sprintf("%#x/%#x", masqMark, masqMark),
		},
		{
			"-m", "comment", "--comment", "AWS, container return",
			"-i", vethName, "-j", "CONNMARK", "--restore-mark", "--mask", fmt.Sprintf("%#x", masqMark),
		},
	}
	for _, iptRule := range iptRules {
		if err := ipt.AppendUnique("mangle", "PREROUTING", iptRule...); err != nil {
			return err
		}
	}
	rule = netlink.NewRule()
	rule.Priority = rulePriorityMasq
	mask := uint32(masqMark)
	rule.Mark = mask
	rule.Mask = &mask
	rule.Family = family
	rule.Table = unix.RT_TABLE_MAIN
	if err := netlink.RuleAdd(rule); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("failed to add rule: %v", err)
		}
	}

	eniIface, ok := interfaceByMAC[eniMAC]
	if !ok {
		return fmt.Errorf("failed to find existing interface with MAC %s", eniMAC)
	}

	subnet, err := getSubnet(ctx, eniMAC)
	if err != nil {
		return err
	}

	deviceNumber, err := imds.GetDeviceNumber(ctx, eniMAC)
	if err != nil {
		return err
	}

	tableIdx := routeTableENIStart + deviceNumber

	eniLink, err := netlink.LinkByIndex(eniIface.Index)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetMTU(eniLink, mtu); err != nil {
		return err
	}

	if err := netlink.LinkSetUp(eniLink); err != nil {
		return err
	}

	ips, err := getIPs(ctx, eniMAC)
	if err != nil {
		return err
	}
	eniPrimaryIP := ips[0] // Reserve 'primary' (first) IP address for hostns

	ipn := &net.IPNet{
		IP:   eniPrimaryIP,
		Mask: subnet.Mask,
	}
	addr := &netlink.Addr{
		IPNet: ipn,
		Scope: int(netlink.SCOPE_UNIVERSE),
	}
	if err = netlink.AddrAdd(eniLink, addr); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("failed to add IP addr (%s) to ENI: %v", ipn, err)
		}
	}

	// Nodeport kube-proxy routing games requires "loose"
	// rp_filter.
	if ipVersion == 4 {
		if err := procSys.Set(fmt.Sprintf("net/ipv4/conf/%s/rp_filter", primaryIface.Name), "2"); err != nil {
			return err
		}
	}

	var gwIP net.IP
	switch ipVersion {
	case 4:
		// Router address isn't given in IMDS, but is
		// defined to be subnet .1
		gwIP = subnet.IP.Mask(subnet.Mask)
		gwIP[len(gwIP)-1] |= 1

	case 6:
		// Router address isn't given in IMDS, so we
		// have to wait to observe an RA, and then
		// copy across from the main route
		// table. (TODO: yuck!)
		//
		// The 'good' news is that this is only slow
		// the first time per ENI.

		// "2" == Accept RA, even with forwarding=1
		if err := procSys.Set(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", eniIface.Name), "2"); err != nil {
			return err
		}

	routelist:
		for {
			routes, err := netlink.RouteListFiltered(family, &netlink.Route{
				Table:     unix.RT_TABLE_MAIN,
				LinkIndex: eniLink.Attrs().Index,
				Dst:       &defaultRoute,
			}, netlink.RT_FILTER_OIF)
			if err != nil {
				return fmt.Errorf("failed to list routes on %s: %v", eniLink.Attrs().Name, err)
			}

			for _, r := range routes {
				if isDefaultRoute(r.Dst) && !r.Gw.IsUnspecified() {
					// found a defaultroute!
					gwIP = r.Gw
					break routelist
				}
			}

			log.Printf("Waiting for IPv6 router advertisement")
			time.Sleep(1 * time.Second) // RAs happen every ~10s
		}
	}

	routes := []netlink.Route{
		// per-ENI subnet route
		{
			Table:     tableIdx,
			LinkIndex: eniIface.Index,
			Dst:       &subnet,
			Scope:     netlink.SCOPE_LINK,
		},
		// per-ENI default route
		{
			Table:     tableIdx,
			LinkIndex: eniIface.Index,
			Dst:       &defaultRoute,
			Gw:        gwIP,
			Scope:     netlink.SCOPE_UNIVERSE,
		},
	}
	for _, r := range routes {
		if err := netlink.RouteReplace(&r); err != nil {
			if !os.IsExist(err) {
				return fmt.Errorf("failed to add route (%s): %v", r, err)
			}
		}
	}

	// Force ENI 'primary' IP out desired ENI
	rule = netlink.NewRule()
	rule.Priority = rulePriorityOutgoingENI
	rule.Table = tableIdx
	rule.Src = &net.IPNet{
		IP:   eniPrimaryIP,
		Mask: net.CIDRMask(maskLen, maskLen),
	}

	if err := netlink.RuleAdd(rule); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("failed to add rule (%s): %v", rule, err)
		}
	}

	return nil
}

// Setup pod IP policy routes. Traffic from pod IP has to go out the
// correct ENI to satisfy the AWS src/dst check.
func setupHostEniPodRoute(ec2Metadata metadata.EC2MetadataIface, vethName string, eniMAC string, ipc *cniv1.IPConfig) error {
	ctx := context.TODO()

	imds := metadata.NewTypedIMDS(ec2Metadata)

	maskLen := 128
	if ipc.Address.IP.To4() != nil {
		maskLen = 32
	}

	veth, err := netlink.LinkByName(vethName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", vethName, err)
	}

	// per-pod local pod route
	route := netlink.Route{
		Table:     routeTablePod,
		LinkIndex: veth.Attrs().Index,
		Dst: &net.IPNet{
			IP:   ipc.Address.IP,
			Mask: net.CIDRMask(maskLen, maskLen),
		},
		Scope: netlink.SCOPE_UNIVERSE,
		// TODO: Src: primaryIP?
	}
	if err := netlink.RouteReplace(&route); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("failed to add route (%s): %v", route, err)
		}
	}

	deviceNumber, err := imds.GetDeviceNumber(ctx, eniMAC)
	if err != nil {
		return err
	}

	tableIdx := routeTableENIStart + deviceNumber

	// Force pod IP out desired ENI
	rule := netlink.NewRule()
	rule.Priority = rulePriorityOutgoingENI
	rule.Table = tableIdx
	rule.Src = &net.IPNet{
		IP:   ipc.Address.IP,
		Mask: net.CIDRMask(maskLen, maskLen),
	}
	if err := netlink.RuleAdd(rule); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("failed to add rule (%s): %v", rule, err)
		}
	}

	return nil
}

func setupHostEni(ec2Metadata metadata.EC2MetadataIface, procSys procsys.ProcSys, mtu int, vethName string, result *cniv1.Result) error {
	ctx := context.TODO()

	imds := metadata.NewTypedIMDS(ec2Metadata)

	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	interfaceByMAC := make(map[string]*net.Interface, len(ifaces))
	for i := range ifaces {
		iface := ifaces[i]
		interfaceByMAC[iface.HardwareAddr.String()] = &iface
	}

	primaryMAC, err := imds.GetMAC(ctx)
	if err != nil {
		return err
	}
	primaryIface := interfaceByMAC[primaryMAC]
	if primaryIface == nil {
		return fmt.Errorf("failed to find interface for MAC %s", primaryMAC)
	}

	for _, ipc := range result.IPs {

		getIPs := imds.GetIPv6s
		ipVersion := 6
		if ipc.Address.IP.To4() != nil {
			getIPs = imds.GetLocalIPv4s
			ipVersion = 4
		}

		// Find related ENI
		macs, err := imds.GetMACs(ctx)
		if err != nil {
			return err
		}

		var eniMAC string
	macloop:
		for _, mac := range macs {
			ips, err := getIPs(ctx, mac)
			if err != nil {
				return err
			}

			for _, ip := range ips {
				if ip.Equal(ipc.Address.IP) {
					eniMAC = mac
					break macloop
				}
			}
		}
		if eniMAC == "" {
			return fmt.Errorf("failed to find ENI for %s", ipc.Address)
		}

		if err := setupHostEniIface(ec2Metadata, procSys, mtu, vethName, eniMAC, ipVersion); err != nil {
			return err
		}

		if err := setupHostEniPodRoute(ec2Metadata, vethName, eniMAC, ipc); err != nil {
			return err
		}

		// Always configure primary ENI (for kubelet itself)
		if eniMAC != primaryMAC {
			if err := setupHostEniIface(ec2Metadata, procSys, mtu, vethName, primaryMAC, ipVersion); err != nil {
				return err
			}
		}
	}

	return nil
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("CNI imds-ptp: ")
	log.SetOutput(os.Stderr) // NB: ends up in kubelet syslog

	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, cniversion.All, fmt.Sprintf("imds-ptp CNI plugin %s", version))
}

func cmdCheck(args *skel.CmdArgs) error {
	netConf, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	if trace {
		log.Printf("CHECK: %v", args)
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	// run the IPAM plugin and get back the config to apply
	err = ipam.ExecCheck(netConf.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}
	if netConf.NetConf.RawPrevResult == nil {
		return fmt.Errorf("ptp: Required prevResult missing")
	}
	if err := cniversion.ParsePrevResult(&netConf.NetConf); err != nil {
		return err
	}
	// Convert whatever the IPAM result was into the cniv1 Result type
	result, err := cniv1.NewResultFromResult(netConf.PrevResult)
	if err != nil {
		return err
	}

	var contMap cniv1.Interface
	// Find interfaces for name we know, that of host-device inside container
	for _, intf := range result.Interfaces {
		if args.IfName == intf.Name {
			if args.Netns == intf.Sandbox {
				contMap = *intf
				continue
			}
		}
	}

	// The namespace must be the same as what was configured
	if args.Netns != contMap.Sandbox {
		return fmt.Errorf("sandbox in prevResult %s doesn't match configured netns: %s",
			contMap.Sandbox, args.Netns)
	}

	//
	// Check prevResults for ips, routes and dns against values found in the container
	if err := netns.Do(func(_ ns.NetNS) error {

		// Check interface against values found in the container
		err := validateCniContainerInterface(contMap)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedInterfaceIPs(args.IfName, result.IPs)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedRoute(result.Routes)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	netConf, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	if trace {
		log.Printf("ADD: %v", args)
	}

	session, err := session.NewSession()
	if err != nil {
		return err
	}
	awsConfig := aws.NewConfig().
		// Lots of retries: we have no better strategy available
		WithMaxRetries(20)

	ec2Metadata := metadata.NewCachedIMDS(ec2metadata.New(session, awsConfig))

	procSys := procsys.NewProcSys()

	// run the IPAM plugin and get back the config to apply
	r, err := ipam.ExecAdd(netConf.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	// Invoke ipam del if err to avoid ip leak
	defer func() {
		if err != nil {
			_ = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
		}
	}()

	result, err := cniv1.NewResultFromResult(r)
	if err != nil {
		return err
	}

	if len(result.IPs) == 0 {
		return fmt.Errorf("IPAM plugin returned missing IP config")
	}

	if err := ip.EnableForward(result.IPs); err != nil {
		return fmt.Errorf("could not enable IP forwarding: %v", err)
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	hostInterface, _, err := setupContainerVeth(netns, args.IfName, netConf.MTU, result)
	if err != nil {
		return err
	}

	if err = setupHostVeth(hostInterface.Name, result); err != nil {
		return err
	}

	if err = setupHostEni(ec2Metadata, procSys, netConf.MTU, hostInterface.Name, result); err != nil {
		return err
	}

	if dnsConfSet(netConf.DNS) {
		result.DNS = netConf.DNS
	}

	if trace {
		log.Printf("ADD returning %v", result)
	}

	return types.PrintResult(result, netConf.CNIVersion)
}

func dnsConfSet(dnsConf types.DNS) bool {
	return dnsConf.Nameservers != nil ||
		dnsConf.Search != nil ||
		dnsConf.Options != nil ||
		dnsConf.Domain != ""
}

func cmdDel(args *skel.CmdArgs) error {
	netConf, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	if trace {
		log.Printf("DEL: %v", args)
	}

	if err := ipam.ExecDel(netConf.IPAM.Type, args.StdinData); err != nil {
		return err
	}

	if args.Netns == "" {
		return nil
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// so don't return an error if the device is already removed.
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		var err error
		_, err = ip.DelLinkByNameAddr(args.IfName)
		if err != nil && err == ip.ErrLinkNotFound {
			return nil
		}
		return err
	})

	if err != nil {
		return err
	}

	if trace {
		log.Printf("DEL returning success")
	}

	return nil
}

func validateCniContainerInterface(intf cniv1.Interface) error {

	var link netlink.Link
	var err error

	if intf.Name == "" {
		return fmt.Errorf("container interface name missing in prevResult: %v", intf.Name)
	}
	link, err = netlink.LinkByName(intf.Name)
	if err != nil {
		return fmt.Errorf("ptp: Container Interface name in prevResult: %s not found", intf.Name)
	}
	if intf.Sandbox == "" {
		return fmt.Errorf("ptp: Error: Container interface %s should not be in host namespace", link.Attrs().Name)
	}

	_, isVeth := link.(*netlink.Veth)
	if !isVeth {
		return fmt.Errorf("container interface %s not of type veth/p2p", link.Attrs().Name)
	}

	if intf.Mac != "" {
		if intf.Mac != link.Attrs().HardwareAddr.String() {
			return fmt.Errorf("ptp: Interface %s Mac %s doesn't match container Mac: %s", intf.Name, intf.Mac, link.Attrs().HardwareAddr)
		}
	}

	return nil
}

func isDefaultRoute(dst *net.IPNet) bool {
	// RouteList returns Dst=nil for default route
	if dst == nil {
		return true
	}
	ones, bits := dst.Mask.Size()
	// Reject 0,0 since that means 'invalid'
	return ones == 0 && bits != 0
}
