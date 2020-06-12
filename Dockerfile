FROM golang:1.14-alpine as builder
LABEL maintainer="Antonio Mika <me@antoniomika.me>"

ENV GOCACHE /gocache
ENV GOTMPDIR /gotmpdir
ENV CGO_ENABLED 0

WORKDIR /app

RUN mkdir -p /gocache /gotmpdir

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG REPOSITORY=unknown

RUN go generate ./...
RUN go test ./...
RUN go install -ldflags="-s -w -X github.com/${REPOSITORY}/cmd.Version=${VERSION} -X github.com/${REPOSITORY}/cmd.Commit=${COMMIT} -X github.com/${REPOSITORY}/cmd.Date=${DATE}"

FROM alpine
LABEL maintainer="Antonio Mika <me@antoniomika.me>"

WORKDIR /app

RUN apk add --no-cache git docker-cli docker-compose

COPY --from=builder /app/deploy/ /app/deploy/
COPY --from=builder /app/README* /app/LICENSE* /app/
COPY --from=builder /go/bin/ /app/

ENTRYPOINT ["/app/pcompose"]
