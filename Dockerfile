FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder
LABEL maintainer="Antonio Mika <me@antoniomika.me>"

ENV CGO_ENABLED=0

WORKDIR /app

COPY go.* ./

RUN go mod download

FROM builder AS build-image

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG REPOSITORY=antoniomika/pcompose

ARG TARGETOS
ARG TARGETARCH

ENV GOOS=${TARGETOS} GOARCH=${TARGETARCH}

RUN go build -o /go/bin/app -ldflags="-s -w -X github.com/${REPOSITORY}/cmd.Version=${VERSION} -X github.com/${REPOSITORY}/cmd.Commit=${COMMIT} -X github.com/${REPOSITORY}/cmd.Date=${DATE}"

ENTRYPOINT ["/go/bin/app"]

FROM alpine:3@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf AS release
LABEL maintainer="Antonio Mika <me@antoniomika.me>"

WORKDIR /app

RUN apk add --no-cache \
    docker-cli docker-cli-buildx docker-cli-compose git openssh-client \
    tini zsh ncurses ncurses-terminfo curl jq vim less htop \
  && git config --system safe.directory '*'

ENV LANG=C.UTF-8

COPY shell/zshrc /etc/zsh/zshrc
COPY README* LICENSE* /app/
COPY --from=build-image /go/bin/app /app/app

ENTRYPOINT ["/sbin/tini", "--", "/app/app"]
