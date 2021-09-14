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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/anguslees/aws-cni-plugins/internal/metadata"
)

func TestJsonnetIMDS(t *testing.T) {
	imds := metadata.FakeIMDS(map[string]interface{}{
		"local-ipv4": "192.100.1.2",
	})

	vm := JsonnetVM(imds, nil)

	tests := []struct {
		in    string
		out   string
		error bool
	}{
		{
			in:  "'My IP is ' + std.native('metadata')('local-ipv4')",
			out: "\"My IP is 192.100.1.2\"\n",
		},
		{
			in:  "'A 404 is ' + std.native('metadata')('unknown') + ' ok'",
			out: "\"A 404 is  ok\"\n",
		},
		{
			in:    "std.native('metadata')()",
			error: true,
		},
		{
			in:    "std.native('metadata')(42)",
			error: true,
		},
		{
			in:    "std.native('metadata')('a', 'b')",
			error: true,
		},
	}

	for i, test := range tests {
		json, err := vm.EvaluateSnippet(
			fmt.Sprintf("<test %d>", i),
			test.in,
		)

		if !test.error {
			if assert.NoError(t, err) {
				assert.Equal(t, test.out, json)
			}
		} else {
			assert.Error(t, err)
		}
	}
}

func TestJsonnetExtvars(t *testing.T) {
	vars := map[string]string{
		"foo": "bar",
	}
	vm := JsonnetVM(metadata.FakeIMDS(nil), vars)

	json, err := vm.EvaluateSnippet("<test>", "'foo is ' + std.extVar('foo')")
	if assert.NoError(t, err) {
		assert.Equal(t, "\"foo is bar\"\n", json)
	}
}

func TestEnvironMap(t *testing.T) {
	result := EnvironMap([]string{"a=foo", "b=", "c", ""})
	assert.Equal(t, map[string]string{
		"a": "foo",
		"b": "",
	}, result)
}
