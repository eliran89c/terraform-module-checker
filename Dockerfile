FROM golang:1.19 as builder

# Copy go.mod and download dependencies
WORKDIR /app
COPY go.mod .
COPY go.sum .
RUN go mod download

ARG CGO_ENABLED=0
ARG GOOS=linux
ARG GOARCH=amd64

# Build
COPY . .
RUN go build -a -ldflags="-w -s" -o bin/terraform-module-checker

# Build the final image with only the binary
FROM alpine
RUN set -x \
    && apk update \
    && apk add --no-cache \
        git \
        git-lfs\
        openssh

COPY --from=builder /app/bin/terraform-module-checker .
ENTRYPOINT ["/terraform-module-checker"]