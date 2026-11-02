ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS builder

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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

USER 65532:65532
COPY --from=builder /alertly /alertly
EXPOSE 8080
ENTRYPOINT ["/alertly"]
