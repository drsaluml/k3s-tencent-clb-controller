FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w" -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
