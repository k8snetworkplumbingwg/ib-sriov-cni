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
FROM gcr.io/distroless/static-debian13@sha256:f2ea2709ac8db56323cbd7d014277f32cb572d9ea124b0076f7aafe5980678fe

COPY --from=builder \
     /usr/src/ib-sriov-cni/build/ib-sriov \
     /usr/src/ib-sriov-cni/build/thin_entrypoint \
     /usr/bin/

WORKDIR /

LABEL io.k8s.display-name="InfiniBand SR-IOV CNI"

ENTRYPOINT ["/usr/bin/thin_entrypoint"]
