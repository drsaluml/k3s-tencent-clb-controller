# --platform=$BUILDPLATFORM ทำให้ stage นี้รันบนสถาปัตยกรรมของ runner เสมอ
# แล้ว cross-compile ด้วย GOOS/GOARCH — เร็วกว่าให้ qemu emulate ทั้ง toolchain มาก
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" \
    -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
