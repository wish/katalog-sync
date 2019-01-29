FROM       golang:alpine as builder

COPY . /go/src/github.com/wish/katalog-sync
RUN cd /go/src/github.com/wish/katalog-sync/cmd/katalog-sync-daemon && CGO_ENABLED=0 go build
RUN cd /go/src/github.com/wish/katalog-sync/cmd/katalog-sync-sidecar && CGO_ENABLED=0 go build

FROM golang:alpine

COPY --from=builder /go/src/github.com/wish/katalog-sync/cmd/katalog-sync-daemon/katalog-sync-daemon /bin/katalog-sync-daemon
COPY --from=builder /go/src/github.com/wish/katalog-sync/cmd/katalog-sync-sidecar/katalog-sync-sidecar /bin/katalog-sync-sidecar
