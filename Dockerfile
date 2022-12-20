FROM --platform=$BUILDPLATFORM golang:1.19-alpine as builder
LABEL maintainer="Antonio Mika <me@antoniomika.me>"

ENV CGO_ENABLED 0

WORKDIR /app

COPY go.* ./

RUN go mod download

FROM builder as build-image

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG REPOSITORY=unknown

ARG TARGETOS
ARG TARGETARCH

ENV GOOS=${TARGETOS} GOARCH=${TARGETARCH}

RUN go build -o /go/bin/app -ldflags="-s -w -X github.com/${REPOSITORY}/cmd.Version=${VERSION} -X github.com/${REPOSITORY}/cmd.Commit=${COMMIT} -X github.com/${REPOSITORY}/cmd.Date=${DATE}"

ENTRYPOINT ["/go/bin/app"]

FROM snkshukla/alpine-zsh as release
LABEL maintainer="Antonio Mika <me@antoniomika.me>"

WORKDIR /app

RUN apk add --no-cache git docker-cli docker-compose

COPY --from=build-image /app/deploy/ /app/deploy/
COPY --from=build-image /app/README* /app/LICENSE* /app/
COPY --from=build-image /go/bin/ /app/

ENTRYPOINT ["/app/app"]
