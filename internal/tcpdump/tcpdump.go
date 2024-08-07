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

package tcpdump

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
)

// Tcpdump runs tcpdump asynchronously in the current network namespace.
//
// Useful debugging tool. Example usage:
//
//	err = originalNs.Do(func(ns.NetNS) error {
//	       return Tcpdump(ctx, GinkgoWriter, "original", "any")
//	})
//	Expect(err).NotTo(HaveOccurred())
func Tcpdump(ctx context.Context, w io.Writer, label, ifname string) error {
	copy := func(r io.Reader) {
		buf := bufio.NewReader(r)
		for {
			line, err := buf.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					fmt.Fprintf(w, "%s: [Error: %v]\n", label, err)
				}
				break
			}
			fmt.Fprintf(w, "%s: %s", label, line)
		}
	}

	cmd := exec.CommandContext(ctx, "tcpdump", "-v", "-n", "-i", ifname)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	go copy(stdout)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	go copy(stderr)

	return cmd.Start()
}
