# Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License"). You
# may not use this file except in compliance with the License. A copy of
# the License is located at
#
#       http://aws.amazon.com/apache2.0/
#
# or in the "license" file accompanying this file. This file is
# distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF
# ANY KIND, either express or implied. See the License for the specific
# language governing permissions and limitations under the License.
#

GO ?= $(shell which go)

# Tests in cmd/egress-v4 require NET_ADMIN/SYS_ADMIN.
# 'unshare' provides this in a user namespace, otherwise 'sudo' will give "real" root.
#ROOT_CMD = sudo
ROOT_CMD = unshare -rm

BINDIR ?= _out

all: bin-attach-enis bin-egress-v4 bin-imds-ipam bin-imds-ptp bin-json-tmpl

bin-%:
	$(GO) build -o $(BINDIR)/$* ./$*

test:
	$(ROOT_CMD) $(GO) test ./...

generate:
	$(GO) generate ./...
