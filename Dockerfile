ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35

USER 65532:65532
COPY --from=builder /alertly /alertly
EXPOSE 8080
ENTRYPOINT ["/alertly"]
