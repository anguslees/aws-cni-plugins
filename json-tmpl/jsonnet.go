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
	"fmt"
	"strings"

	jsonnet "github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"

	"github.com/anguslees/aws-cni-plugins/internal/metadata"
)

type iMDS struct {
	client metadata.EC2MetadataIface
}

func (i iMDS) Metadata(args []interface{}) (interface{}, error) {
	ctx := context.TODO()

	if len(args) != 1 {
		return nil, fmt.Errorf("expected single argument, got %d", len(args))
	}
	path, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("expected string, got %T", args[0])
	}

	result, err := i.client.GetMetadataWithContext(ctx, path)
	if metadata.IsNotFound(err) {
		return "", nil
	}

	return result, nil
}

// JsonnetVM creates and initialises a new jsonnet VM.
func JsonnetVM(ec2imds metadata.EC2MetadataIface, extvars map[string]string) *jsonnet.VM {
	vm := jsonnet.MakeVM()

	imds := iMDS{client: ec2imds}

	vm.NativeFunction(&jsonnet.NativeFunction{
		Name:   "metadata",
		Func:   imds.Metadata,
		Params: []ast.Identifier{"path"},
	})

	for k, v := range extvars {
		vm.ExtVar(k, v)
	}

	return vm
}

// EnvironMap is a helper function to build a key-value map from
// os.Environ's "key=value" slice.
func EnvironMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, e := range env {
		kv := strings.SplitN(e, "=", 2)
		if len(kv) == 2 { // Skip invalid input values
			result[kv[0]] = kv[1]
		}
	}
	return result
}
