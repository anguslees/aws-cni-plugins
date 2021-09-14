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
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"
	cniversion "github.com/containernetworking/cni/pkg/version"

	"github.com/anguslees/aws-cni-plugins/internal/metadata"
)

var version string

const trace = false

func main() {
	log.SetFlags(0)
	log.SetPrefix("CNI imds-ipam: ")
	log.SetOutput(os.Stderr) // NB: ends up in kubelet syslog

	rand.Seed(time.Now().UnixNano())

	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, cniversion.All, fmt.Sprintf("imds-ipam CNI plugin %s", version))
}

type NetConfIgnoreInterfaceTerm struct {
	// Ignore DeviceIndex in range [Start,End)
	DeviceIndexStart int `json:"deviceIndexStart"`
	DeviceIndexEnd   int `json:"deviceIndexEnd"`
}

// IPAMConf is our CNI (IPAM) config structure
type IPAMConf struct {
	types.IPAM

	Routes    []*types.Route `json:"routes"`
	DataDir   string         `json:"dataDir"`
	IPVersion string         `json:"ipVersion"`

	// Interfaces to ignore (ignores interfaces matching any term)
	IgnoreInterfaces []NetConfIgnoreInterfaceTerm `json:"ignoreInterfaces"`
}

// NetConf is our CNI config structure
type NetConf struct {
	CNIVersion string `json:"cniVersion,omitempty"`

	Name string    `json:"name,omitempty"`
	IPAM *IPAMConf `json:"ipam,omitempty"`
}

func loadConf(bytes []byte) (*NetConf, *IPAMConf, error) {
	n := &NetConf{}

	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, nil, err
	}

	if n.IPAM == nil {
		return nil, nil, fmt.Errorf("IPAM config missing 'ipam' key")
	}

	if n.IPAM.IPVersion == "" {
		n.IPAM.IPVersion = "4"
	}

	return n, n.IPAM, nil
}

func cmdCheck(args *skel.CmdArgs) error {
	if trace {
		log.Printf("CHECK: %v", args)
	}

	netConf, ipamConf, err := loadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	store := NewStore(filepath.Join(ipamConf.DataDir, netConf.Name))
	if err := store.Open(); err != nil {
		return err
	}
	defer store.Close()

	ip := store.FindByID(args.ContainerID, args.IfName)
	if ip == nil {
		return fmt.Errorf("imds-ipam: Failed to find address added by container %s", args.ContainerID)
	}

	if trace {
		log.Printf("CHECK returning success")
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	ctx := context.TODO()

	if trace {
		log.Printf("ADD: %v", args)
	}

	netConf, ipamConf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	session, err := session.NewSession()
	if err != nil {
		return err
	}
	awsConfig := aws.NewConfig()
	imds := metadata.NewTypedIMDS(metadata.NewCachedIMDS(ec2metadata.New(session, awsConfig)))

	result := &cniv1.Result{}

	store := NewStore(filepath.Join(ipamConf.DataDir, netConf.Name))
	if err := store.Open(); err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			panic(err)
		}
	}()

	allocator := NewIMDSAllocator(imds, &store)

	ipConf, err := allocator.Get(ctx, args.ContainerID, args.IfName, ipamConf.IPVersion)
	if err != nil {
		return err
	}
	result.IPs = append(result.IPs, &ipConf)

	result.Routes = ipamConf.Routes

	if trace {
		log.Printf("ADD returning %v", result)
	}

	return types.PrintResult(result, netConf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	ctx := context.TODO()

	if trace {
		log.Printf("DEL: %v", args)
	}

	netConf, ipamConf, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	session, err := session.NewSession()
	if err != nil {
		return err
	}
	awsConfig := aws.NewConfig()
	imds := metadata.NewTypedIMDS(metadata.NewCachedIMDS(ec2metadata.New(session, awsConfig)))

	store := NewStore(filepath.Join(ipamConf.DataDir, netConf.Name))
	if err := store.Open(); err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			panic(err)
		}
	}()

	allocator := NewIMDSAllocator(imds, &store)

	if err := allocator.Put(ctx, args.ContainerID, args.IfName, ipamConf.IPVersion); err != nil {
		return err
	}

	if trace {
		log.Printf("DEL returning success")
	}

	return nil
}
