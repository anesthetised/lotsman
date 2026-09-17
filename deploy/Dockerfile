# Runtime image. The container needs the kernel bits Lotsman programs:
#   docker run -d --name lotsman \
#     --cap-add NET_ADMIN --device /dev/net/tun \
#     --sysctl net.ipv4.ip_forward=1 --sysctl net.ipv4.conf.default.rp_filter=2 \
#     -p 51820:51820/udp \
#     -v /etc/lotsman:/etc/lotsman:ro -v lotsman-state:/var/lib/lotsman \
#     ghcr.io/anesthetised/lotsman
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /lotsman ./cmd/lotsman

FROM gcr.io/distroless/static-debian12:nonroot
# Root: TUN creation and netlink need it even with NET_ADMIN granted.
USER root
COPY --from=build /lotsman /usr/local/bin/lotsman
VOLUME /var/lib/lotsman
EXPOSE 51820/udp
ENTRYPOINT ["/usr/local/bin/lotsman"]
CMD ["serve"]
