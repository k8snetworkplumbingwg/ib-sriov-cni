FROM golang:1.26 AS builder

COPY . /usr/src/ib-sriov-cni

ARG TARGETOS
ARG TARGETARCH
ARG http_proxy
ARG https_proxy

ENV HTTP_PROXY=$http_proxy \
    HTTPS_PROXY=$https_proxy \
    GOOS=$TARGETOS \
    GOARCH=$TARGETARCH

WORKDIR /usr/src/ib-sriov-cni
RUN make clean && \
    make build

# docker pull gcr.io/distroless/static-debian13
# docker inspect --format='{{index .RepoDigests 0}}' gcr.io/distroless/static-debian13
FROM gcr.io/distroless/static-debian13@sha256:d5f030ca7c5793784e9ea4178a116da360250411d13921a5af27c6cb5a5949bf

COPY --from=builder \
     /usr/src/ib-sriov-cni/build/ib-sriov \
     /usr/src/ib-sriov-cni/build/thin_entrypoint \
     /usr/bin/

WORKDIR /

LABEL io.k8s.display-name="InfiniBand SR-IOV CNI"

ENTRYPOINT ["/usr/bin/thin_entrypoint"]
