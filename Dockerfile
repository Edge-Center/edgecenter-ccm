FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
ADD go.mod go.sum /build/
RUN go mod download -x
ADD cmd /build/cmd
ADD internal /build/internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o ./ec-ccm ./cmd/main.go

FROM alpine:3.18.4
COPY --from=builder /build/ec-ccm /usr/local/bin/
ENTRYPOINT ["ec-ccm"]