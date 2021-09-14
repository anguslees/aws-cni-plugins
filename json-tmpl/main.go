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
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/golang/glog"

	"github.com/anguslees/aws-cni-plugins/internal/metadata"
)

var inputFile = flag.String("file", "", "input file or '-' to use stdin (required)")

func _main() error {
	var inputStream io.Reader
	var filename string
	switch *inputFile {
	case "":
		return fmt.Errorf("--file is required")
	case "-":
		filename = "<stdin>"
		inputStream = os.Stdin
	default:
		f, err := os.Open(*inputFile)
		if err != nil {
			return fmt.Errorf("unable to open %s: %w", *inputFile, err)
		}
		defer f.Close()
		filename = *inputFile
		inputStream = f
	}

	session, err := session.NewSession()
	if err != nil {
		return err
	}
	// Our dependencies link in glog anyway, so we may way as well
	// make the logging command line args useful...
	awsLogLevel := aws.LogOff
	if glog.V(3) {
		awsLogLevel = aws.LogDebug
	}
	awsConfig := aws.NewConfig().
		WithLogger(aws.LoggerFunc(glog.V(3).Info)).
		WithLogLevel(awsLogLevel)

	imds := metadata.NewCachedIMDS(ec2metadata.New(session, awsConfig))

	vm := JsonnetVM(imds, EnvironMap(os.Environ()))

	data, err := ioutil.ReadAll(inputStream)
	if err != nil {
		return err
	}

	glog.V(4).Infof("Input is:\n%s", string(data))

	output, err := vm.EvaluateAnonymousSnippet(filename, string(data))
	if err != nil {
		return err
	}

	glog.V(2).Infof("Output is:\n%s", output)

	if _, err = os.Stdout.WriteString(output); err != nil {
		return err
	}

	return nil
}

func main() {
	flag.Parse()

	err := _main()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
