FROM golang:1.26.5-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/witty-reply ./cmd/witty-reply

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl fonts-dejavu-core tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home witty

COPY --from=build /out/witty-reply /usr/local/bin/witty-reply
USER 10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/witty-reply"]
