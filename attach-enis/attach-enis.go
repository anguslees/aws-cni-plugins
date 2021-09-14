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
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"

	"github.com/anguslees/aws-cni-plugins/internal/metadata"
)

var numENIs = flag.Int("enis", 0,
	"Number of ENIs to allocate, including primary ENI. 0 means maximum supported by instance type.")
var numIPs = flag.Int("ips", 0,
	"Number of IPs to allocate per ENI, including primary IP. 0 means maximum supported by instance type.")
var maxIPs = flag.Int("max-ips", 250,
	"Stop attaching ENIs after allocation at least this many IPs.")
var ipv4 = flag.Bool("ipv4", true,
	"Attach IPv4 VPC addresses.")
var ipv6 = flag.Bool("ipv6", false,
	"Attach IPv6 VPC addresses.")

func main() {
	flag.Parse()

	if err := doSetup(context.TODO()); err != nil {
		panic(err)
	}
}

func doSetup(ctx context.Context) error {
	session, err := session.NewSession()
	if err != nil {
		return err
	}
	awsConfig := aws.NewConfig().
		// Lots of retries: we have no better strategy available
		WithMaxRetries(20).
		WithLogLevel(aws.LogDebugWithRequestRetries)

	ec2Metadata := ec2metadata.New(session, awsConfig)
	region, err := ec2Metadata.Region()
	if err != nil {
		return err
	}

	ec2Svc := ec2.New(session, awsConfig.WithRegion(region))
	ec2Svc.Handlers.Send.PushBack(request.MakeAddToUserAgentHandler("attach-enis", "0"))

	addrFamily := make([]int, 0, 2)
	if *ipv4 {
		addrFamily = append(addrFamily, 4)
	}
	if *ipv6 {
		addrFamily = append(addrFamily, 6)
	}

	for _, family := range addrFamily {
		if err := attachENIs(ctx, ec2Metadata, ec2Svc, family); err != nil {
			return err
		}
	}

	return nil
}

// Create/attach all the desired ENIs.  In a ideal world, this would
// happen during boot in the launchtemplate, and we could remove this
// function.  Currently, ASG rejects launchtemplates that create more
// than one interface, however. :(
func attachENIs(ctx context.Context, ec2Metadata metadata.EC2MetadataIface, ec2Svc ec2iface.EC2API, addrFamily int) error {
	imds := metadata.TypedIMDS{metadata.NewCachedIMDS(ec2Metadata)}

	// NB: This is ~carefully written to make _no_ AWS API calls
	// unless necessary (excluding IMDS).

	instanceID, err := imds.GetInstanceID(ctx)
	if err != nil {
		return err
	}
	primaryMAC, err := imds.GetMAC(ctx)
	if err != nil {
		return err
	}
	sgIDs, err := imds.GetSecurityGroupIDs(ctx, primaryMAC)
	if err != nil {
		return err
	}
	subnetID, err := imds.GetSubnetID(ctx, primaryMAC)
	if err != nil {
		return err
	}

	availableIPs := 0
	devNums := make(map[int]string)

	macs, err := imds.GetMACs(ctx)
	if err != nil {
		return err
	}

	getIPs := imds.GetLocalIPv4s
	if addrFamily == 6 {
		getIPs = imds.GetIPv6s
	}

	// Find existing ENI device numbers (read-only)
	for _, mac := range macs {
		num, err := imds.GetDeviceNumber(ctx, mac)
		if err != nil {
			return err
		}
		devNums[num] = mac

		ips, err := getIPs(ctx, mac)
		if err != nil {
			return err
		}

		log.Printf("Found existing ENI (%s) with %d IPs", mac, len(ips))

		availableIPs += len(ips) - 1 // -1 for primary IP
		if availableIPs >= *maxIPs {
			// Found enough IPs, with nothing to do!
			log.Printf("Proceeding with at least %d available IPs across %d ENIs", availableIPs, len(devNums))
			return nil
		}
	}

	// Need to attach more ENIs/IPs

	if *numENIs == 0 || *numIPs == 0 {
		itype, err := imds.GetInstanceType(ctx)
		if err != nil {
			return err
		}

		ditOut, err := ec2Svc.DescribeInstanceTypesWithContext(ctx, &ec2.DescribeInstanceTypesInput{
			InstanceTypes: aws.StringSlice([]string{itype}),
		})
		if err != nil {
			return err
		}

		if len(ditOut.InstanceTypes) != 1 {
			return fmt.Errorf("describe instance-types returned %d results for %q", len(ditOut.InstanceTypes), itype)
		}

		info := ditOut.InstanceTypes[0].NetworkInfo
		if *numENIs == 0 {
			*numENIs = int(aws.Int64Value(info.MaximumNetworkInterfaces))
		}
		if *numIPs == 0 {
			// FIXME: Should also check info.Ipv6AddressesPerInterface
			*numIPs = int(aws.Int64Value(info.Ipv4AddressesPerInterface))
		}
		log.Printf("Using --enis=%d --ips=%d", *numENIs, *numIPs)
	}

	// Add to existing ENIs if possible
	for _, mac := range devNums {
		if availableIPs >= *maxIPs {
			// Good enough!
			break
		}

		ips, err := getIPs(ctx, mac)
		if err != nil {
			return err
		}

		if len(ips) < *numIPs {
			// Existing interface needs more IPs.
			interfaceID, err := imds.GetInterfaceID(ctx, mac)
			if err != nil {
				return err
			}

			log.Printf("Assigning %d additional IPv%d IPs to %s", *numIPs-len(ips), addrFamily, mac)

			switch addrFamily {
			case 4:
				_, err = ec2Svc.AssignPrivateIpAddressesWithContext(ctx, &ec2.AssignPrivateIpAddressesInput{
					NetworkInterfaceId:             aws.String(interfaceID),
					SecondaryPrivateIpAddressCount: aws.Int64(int64(*numIPs - len(ips))),
				})
			case 6:
				_, err = ec2Svc.AssignIpv6AddressesWithContext(ctx, &ec2.AssignIpv6AddressesInput{
					NetworkInterfaceId: aws.String(interfaceID),
					Ipv6AddressCount:   aws.Int64(int64(*numIPs - len(ips))),
				})
			}
			if err != nil {
				return err
			}

			availableIPs += *numIPs - len(ips)
		}
	}

	// Create+attach new ENIs up to numENIs
	for devNum := 0; len(devNums) < *numENIs; devNum++ {
		if availableIPs >= *maxIPs {
			// Good enough!
			break
		}

		if _, ok := devNums[devNum]; ok {
			// This devNum already exists
			continue
		}

		log.Printf("Creating additional ENI with %d IPv%d IPs", *numIPs, addrFamily)
		cniReq := ec2.CreateNetworkInterfaceInput{
			Description: aws.String(fmt.Sprintf("ENI for %s", instanceID)),
			Groups:      aws.StringSlice(sgIDs),
			SubnetId:    aws.String(subnetID),
			TagSpecifications: []*ec2.TagSpecification{{
				ResourceType: aws.String("network-interface"),
				// These match the tags used in vpc-cni
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("node.k8s.amazonaws.com/instance_id"),
						Value: aws.String(instanceID),
					},
					{
						Key:   aws.String("node.k8s.amazonaws.com/createdAt"),
						Value: aws.String(time.Now().Format(time.RFC3339)),
					},
				},
			}},
		}
		switch addrFamily {
		case 4:
			cniReq.SetSecondaryPrivateIpAddressCount(int64(*numIPs - 1)) // +1 for primary
		case 6:
			cniReq.SetIpv6AddressCount(int64(*numIPs))
		}

		cniOut, err := ec2Svc.CreateNetworkInterfaceWithContext(ctx, &cniReq)
		if err != nil {
			return err
		}
		interfaceID := aws.StringValue(cniOut.NetworkInterface.NetworkInterfaceId)

		cleanupENI := func(id, aid *string) {
			// Best-effort cleanup.  No error checking, no context.
			if aid != nil {
				log.Printf("Attempting to detach ENI %s", *id)
				ec2Svc.DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{
					AttachmentId: aid,
				})
			}
			log.Printf("Attempting to delete ENI %s", *id)
			ec2Svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: id,
			})
		}

		log.Printf("Attaching new ENI %s to index %d", interfaceID, devNum)
		aniOut, err := ec2Svc.AttachNetworkInterfaceWithContext(ctx, &ec2.AttachNetworkInterfaceInput{
			DeviceIndex:        aws.Int64(int64(devNum)),
			InstanceId:         aws.String(instanceID),
			NetworkInterfaceId: aws.String(interfaceID),
		})
		if err != nil {
			cleanupENI(aws.String(interfaceID), nil)
			return err
		}

		log.Printf("Setting DeleteOnTermination on interface %s attachment %s", interfaceID, aws.StringValue(aniOut.AttachmentId))
		_, err = ec2Svc.ModifyNetworkInterfaceAttributeWithContext(ctx, &ec2.ModifyNetworkInterfaceAttributeInput{
			NetworkInterfaceId: aws.String(interfaceID),
			Attachment: &ec2.NetworkInterfaceAttachmentChanges{
				AttachmentId:        aniOut.AttachmentId,
				DeleteOnTermination: aws.Bool(true),
			},
		})
		if err != nil {
			cleanupENI(aws.String(interfaceID), aniOut.AttachmentId)
			return err
		}

		devNums[devNum] = aws.StringValue(cniOut.NetworkInterface.MacAddress)
		availableIPs += *numIPs - 1
	}

	log.Printf("Proceeding with at least %d available secondary IPs across %d ENIs", availableIPs, len(devNums))

	// Wait for all those interfaces+IPs to actually arrive
	waitDuration := 1 * time.Second
	for devNum, mac := range devNums {
		for {
			ips, err := getIPs(ctx, mac)
			if err == nil && len(ips) >= *numIPs {
				// Ready to go!
				break
			}
			if err != nil && !metadata.IsNotFound(err) {
				return err
			}

			log.Printf("Waiting %s for interface %s (device-index %d) to report %d IPv%d IPs in IMDS", waitDuration, mac, devNum, *numIPs, addrFamily)
			time.Sleep(waitDuration)

			// Arbitrary geometric increase
			waitDuration = time.Duration(float64(waitDuration) * 1.4)

			// Invalidate IMDS cache
			imds = metadata.TypedIMDS{metadata.NewCachedIMDS(ec2Metadata)}
			switch addrFamily {
			case 4:
				getIPs = imds.GetLocalIPv4s
			case 6:
				getIPs = imds.GetIPv6s
			}
		}
	}

	return nil
}
