#!/bin/sh

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
