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
FROM gcr.io/distroless/static-debian13@sha256:47b2d72ff90843eb8a768b5c2f89b40741843b639d065b9b937b07cd59b479c6

COPY --from=builder \
     /usr/src/ib-sriov-cni/build/ib-sriov \
     /usr/src/ib-sriov-cni/build/thin_entrypoint \
     /usr/bin/

WORKDIR /

LABEL io.k8s.display-name="InfiniBand SR-IOV CNI"

ENTRYPOINT ["/usr/bin/thin_entrypoint"]
