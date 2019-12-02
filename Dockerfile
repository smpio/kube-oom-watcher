FROM golang:1.13 as builder

WORKDIR /go/src/github.com/smpio/kube-oom-watcher/


RUN curl https://glide.sh/get | sh

COPY glide.yaml glide.lock ./
RUN glide install

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags "-s -w"

RUN mkdir /tmp/empty-dir


FROM alpine as ca-bundle-builder
RUN apk add ca-certificates


FROM scratch
COPY --from=builder /tmp/empty-dir /tmp
COPY --from=ca-bundle-builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /go/src/github.com/smpio/kube-oom-watcher/kube-oom-watcher /
ENTRYPOINT ["/kube-oom-watcher"]
