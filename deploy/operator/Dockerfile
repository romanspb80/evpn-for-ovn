FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
COPY deploy/operator ./deploy/operator

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/operator ./deploy/operator

FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /out/operator /operator
COPY deploy/operator/core-resources.yaml /core-resources.yaml

ENTRYPOINT ["/operator"]
