FROM golang:1.20-alpine as builder

COPY . /usr/src/ib-sriov-cni

ENV HTTP_PROXY $http_proxy
ENV HTTPS_PROXY $https_proxy

RUN apk add --no-cache --virtual build-dependencies build-base=~0.5 linux-headers=~6.3
WORKDIR /usr/src/ib-sriov-cni
RUN make clean && \
    make build

FROM alpine:3.18.2
COPY --from=builder /usr/src/ib-sriov-cni/build/ib-sriov /usr/bin/
WORKDIR /

LABEL io.k8s.display-name="InfiniBand SR-IOV CNI"

COPY ./images/entrypoint.sh /

ENTRYPOINT ["/entrypoint.sh"]
