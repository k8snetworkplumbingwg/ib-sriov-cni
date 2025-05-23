#!/bin/sh
# Copyright 2025 ib-sriov-cni authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# SPDX-License-Identifier: Apache-2.0


set -x

OCI_RUNTIME=$1
IMAGE_UNDER_TEST=$2

OUTPUT_DIR=$(mktemp -d)

"${OCI_RUNTIME}" run -v "${OUTPUT_DIR}:/out" "${IMAGE_UNDER_TEST}" --cni-bin-dir=/out --no-sleep

if [ ! -e "${OUTPUT_DIR}/ib-sriov" ]; then
    echo "Output file ${OUTPUT_DIR}/ib-sriov not found"
    exit 1
fi

if [ ! -s "${OUTPUT_DIR}/ib-sriov" ]; then
    echo "Output file ${OUTPUT_DIR}/ib-sriov is empty"
    exit 1
fi

exit 0
