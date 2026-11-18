# Build both binaries, ship them in a distroless image.
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/snapvol-csi ./cmd/snapvol-csi \
 && CGO_ENABLED=0 go build -trimpath -o /out/snapvol-controlplane ./cmd/controlplane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/snapvol-csi /usr/local/bin/snapvol-csi
COPY --from=build /out/snapvol-controlplane /usr/local/bin/snapvol-controlplane
ENTRYPOINT ["/usr/local/bin/snapvol-csi"]
