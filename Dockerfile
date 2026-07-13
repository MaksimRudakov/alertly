ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS builder

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w \
        -X github.com/MaksimRudakov/alertly/internal/version.Version=${VERSION} \
        -X github.com/MaksimRudakov/alertly/internal/version.Commit=${COMMIT} \
        -X github.com/MaksimRudakov/alertly/internal/version.Date=${DATE}" \
      -o /alertly ./cmd/alertly

FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f

USER 65532:65532
COPY --from=builder /alertly /alertly
EXPOSE 8080
ENTRYPOINT ["/alertly"]
