FROM docker.io/library/golang:1.26 AS builder

WORKDIR /app

# get dependencies
COPY go.mod go.sum ./
RUN go mod download

# copy source
COPY . .
# codegen
RUN go generate ./...
# build
RUN CGO_ENABLED=1 go build -o bin/ -tags='netgo timetzdata' -trimpath -a -ldflags '-s -w -linkmode external -extldflags "-static"'  ./cmd/clusterd

FROM debian:bookworm-slim

LABEL maintainer="The Sia Foundation <info@sia.tech>" \
      org.opencontainers.image.description.vendor="The Sia Foundation" \
      org.opencontainers.image.description="A cluster container for testing a local Sia network" \
      org.opencontainers.image.source="https://github.com/SiaFoundation/hostd" \
      org.opencontainers.image.licenses=MIT

ENV PUID=0
ENV PGID=0

# copy binary and prepare data dir.
COPY --from=builder /app/bin/* /usr/bin/

EXPOSE 3001/tcp

USER ${PUID}:${PGID}

ENTRYPOINT [ "clusterd" ]