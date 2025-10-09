FROM golang:1.24 AS builder

COPY . /usr/src/ib-sriov-cni

# Declare build arguments for proxy settings
ARG http_proxy
ARG https_proxy

# Use modern ENV syntax and only set if proxy args are provided
ENV HTTP_PROXY=${http_proxy}
ENV HTTPS_PROXY=${https_proxy}

WORKDIR /usr/src/ib-sriov-cni
RUN make clean && \
    make build

FROM gcr.io/distroless/base-debian12:latest
ARG BUILD_VARIANT=standard

COPY --from=builder /usr/src/ib-sriov-cni/build/ib-sriov /usr/src/ib-sriov-cni/bin/
COPY --from=builder /usr/src/ib-sriov-cni/LICENSE /usr/src/ib-sriov-cni/LICENSE
WORKDIR /

COPY --from=builder /usr/src/ib-sriov-cni/build/thin_entrypoint /

LABEL io.k8s.display-name="InfiniBand SR-IOV CNI"

ENTRYPOINT ["/thin_entrypoint"]
