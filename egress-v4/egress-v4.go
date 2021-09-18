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
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"
	cniversion "github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/utils"
	"github.com/j-keck/arping"
	"github.com/vishvananda/netlink"
)

var version string

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// NetConf is our CNI config structure
type NetConf struct {
	types.NetConf

	// Interface inside container to create
	IfName string `json:"ifName"`
	MTU    int    `json:"mtu"`

	// IP to use as SNAT target.
	SnatIP net.IP `json:"snatIP"`
}

func loadConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{IfName: "nat0"}

	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, err
	}

	if n.RawPrevResult != nil {
		// Workaround incorrect case bug:
		// https://github.com/containernetworking/cni/issues/861
		n.RawPrevResult["CNIVersion"] = n.RawPrevResult["cniVersion"]

		if err := cniversion.ParsePrevResult(&n.NetConf); err != nil {
			return nil, fmt.Errorf("could not parse prevResult: %v", err)
		}
	}
	return n, nil
}

// The bulk of this file is mostly based on standard ptp CNI plugin.
//
// Note: There are other options, for example we could add a new
// address onto an existing container/host interface.
//
// Unfortunately kubelet's dockershim (at least) ignores the CNI
// result structure, and directly queries the addresses on the
// container's IfName - and then prefers any global v4 address found.
// We do _not_ want our v4 NAT address to become "the" pod IP!
//
// Also, standard `loopback` CNI plugin checks and aborts if it finds
// any global-scope addresses on `lo`, so we can't just do that
// either.
//
// So we have to create a new interface (not args.IfName) to hide our
// NAT address from all this logic (or patch dockershim, or (better)
// just stop using dockerd...).  Hence ptp.
//

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
			Scope:     netlink.SCOPE_LINK, // <- ptp uses SCOPE_HOST here
			Dst:       ipn,
		})
		if err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to add route on host: %v", err)
		}
	}

	return nil
}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)
	log.SetPrefix("CNI egress-v4: ")
	log.SetOutput(os.Stderr) // NB: ends up in kubelet syslog

	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, cniversion.All, fmt.Sprintf("egress-v4 CNI plugin %s", version))
}

func cmdCheck(args *skel.CmdArgs) error {
	netConf, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	if netConf.PrevResult == nil {
		return fmt.Errorf("must be called as a chained plugin")
	}

	prevResult, err := cniv1.GetResult(netConf.PrevResult)
	if err != nil {
		return err
	}

	chain := utils.MustFormatChainNameWithPrefix(netConf.Name, args.ContainerID, "E4-")
	comment := utils.FormatComment(netConf.Name, args.ContainerID)

	if netConf.SnatIP != nil {
		for _, ipc := range prevResult.IPs {
			if ipc.Address.IP.To4() != nil {
				if err := snat4Check(netConf.SnatIP, ipc.Address.IP, chain, comment); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	netConf, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	//log.Printf("doing ADD: conf=%v", netConf)

	if netConf.PrevResult == nil {
		return fmt.Errorf("must be called as a chained plugin")
	}

	result, err := cniv1.GetResult(netConf.PrevResult)
	if err != nil {
		return err
	}

	for _, ipc := range result.IPs {
		if ipc.Address.IP.To4() != nil {
			// Already has an IPv4 address somehow, just
			// do nothing and exit.
			return types.PrintResult(result, netConf.CNIVersion)
		}
	}

	chain := utils.MustFormatChainNameWithPrefix(netConf.Name, args.ContainerID, "E4-")
	comment := utils.FormatComment(netConf.Name, args.ContainerID)

	ipamResultI, err := ipam.ExecAdd(netConf.IPAM.Type, args.StdinData)
	if err != nil {
		return fmt.Errorf("running IPAM plugin failed: %v", err)
	}

	// Invoke ipam del if err to avoid ip leak
	defer func() {
		if err != nil {
			_ = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
		}
	}()

	tmpResult, err := cniv1.NewResultFromResult(ipamResultI)
	if err != nil {
		return err
	}

	if len(tmpResult.IPs) == 0 {
		return fmt.Errorf("IPAM plugin returned zero IPs")
	}

	if err := ip.EnableForward(tmpResult.IPs); err != nil {
		return fmt.Errorf("could not enable IP forwarding: %v", err)
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	// NB: This uses netConf.IfName NOT args.IfName.
	hostInterface, _, err := setupContainerVeth(netns, netConf.IfName, netConf.MTU, tmpResult)
	if err != nil {
		return err
	}

	if err = setupHostVeth(hostInterface.Name, tmpResult); err != nil {
		return err
	}

	//log.Printf("Set up host iface %q. Result: %v", hostInterface.Name, tmpResult)

	if netConf.SnatIP != nil {
		for _, ipc := range tmpResult.IPs {
			if ipc.Address.IP.To4() != nil {
				//log.Printf("Configuring SNAT %s -> %s", ipc.Address.IP, netConf.SnatIP)
				if err := snat4(netConf.SnatIP, ipc.Address.IP, chain, comment); err != nil {
					return err
				}
			}
		}
	}

	// Copy interfaces over to result, but not IPs.
	result.Interfaces = append(result.Interfaces, tmpResult.Interfaces...)

	//log.Printf("Returning result: %v", result)

	// Pass through the previous result
	return types.PrintResult(result, netConf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	netConf, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	if err := ipam.ExecDel(netConf.IPAM.Type, args.StdinData); err != nil {
		return fmt.Errorf("running IPAM plugin failed: %v", err)
	}

	var ipnets []*net.IPNet
	if args.Netns != "" {
		err := ns.WithNetNSPath(args.Netns, func(hostNS ns.NetNS) error {
			var err error
			ipnets, err = ip.DelLinkByNameAddr(netConf.IfName)
			if err != nil && err == ip.ErrLinkNotFound {
				return nil
			}
			return err
		})
		if err != nil {
			return err
		}
	}

	chain := utils.MustFormatChainNameWithPrefix(netConf.Name, args.ContainerID, "E4-")
	comment := utils.FormatComment(netConf.Name, args.ContainerID)

	if netConf.SnatIP != nil {
		for _, ipn := range ipnets {
			if err := snat4Del(ipn.IP, chain, comment); err != nil {
				return err
			}
		}
	}

	return nil
}
