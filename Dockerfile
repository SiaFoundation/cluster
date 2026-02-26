FROM docker.io/library/golang:1.26 AS builder

WORKDIR /app

# install git-lfs for indexd dependency (contains LFS-tracked files)
RUN apt-get update && apt-get install -y git-lfs && rm -rf /var/lib/apt/lists/*

# setup auth for indexd package access
RUN mkdir -p ~/.ssh \
    && chmod 700 ~/.ssh \
    && ssh-keyscan github.com >> ~/.ssh/known_hosts \
    && chmod 644 ~/.ssh/known_hosts

COPY indexd_ed25519 /root/.ssh/id_ed25519
RUN chmod 600 ~/.ssh/id_ed25519
RUN git config --global url."git@github.com:".insteadOf "https://github.com/"
RUN git config --global url."ssh://git@github.com/".insteadOf "https://github.com/"

ENV GOPRIVATE=go.sia.tech/indexd

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