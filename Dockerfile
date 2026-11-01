FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH TARGETVARIANT
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/intercom-bridge ./cmd/intercom-bridge

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/intercom-bridge /intercom-bridge
ENV STATE_DIR=/data
VOLUME /data
EXPOSE 8790
ENTRYPOINT ["/intercom-bridge"]
