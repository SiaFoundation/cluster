# Cluster

Daemon for spinning up a cluster of nodes and connecting them to a local
test network. Primarily useful for local e2e API testing.

## Updating Daemons

The cluster daemon uses go.mod to manage the daemon dependency. To test a specific
version of a daemon against a cluster, update the go.mod file to point to the
desired commit.

```
go get go.sia.tech/hostd@asdasd
```

## Usage

```bash
$ go run ./cmd/clusterd -renterd=1 --hostd=3
```

Once the cluster is up, the cluster API will be available to interact with the
testnet.

Each node has a unique api address to interact with its respective daemon.

CTRL-C or SIGINT will shut down the cluster and cleanup the resources

### `[GET] /nodes` 
Lists all running nodes in the cluster

**Response Body**
```json
[
        {
                "id": "facaba60da2e71bb",
                "type": "renterd",
                "apiAddress": "http://[::]:53873/api",
                "password": "sia is cool",
                "walletAddress": "addr:1ca3bfe60a50fe3e700a67fae7c9670446849a0177b0da0378399c6d6ca9cb13dfcb3d084c2d"
        },
        {
                "id": "eabf85c452cf4bca",
                "type": "walletd",
                "apiAddress": "http://[::]:53874/api",
                "password": "sia is cool",
                "walletAddress": "addr:000000000000000000000000000000000000000000000000000000000000000089eb0d6a8a69"
        },
        {
                "id": "1c01bc97b794c8d7",
                "type": "hostd",
                "apiAddress": "http://[::]:53864/api",
                "password": "sia is cool",
                "walletAddress": "addr:862b5ca325b4c77ffe861a892e65f48bbcb43f2287b2dfe8d7c70459659f7367a2ffbcf62de9"
        },
        {
                "id": "31b3a65f8a1842d2",
                "type": "hostd",
                "apiAddress": "http://[::]:53867/api",
                "password": "sia is cool",
                "walletAddress": "addr:b906d603430c7e814a107910d8029f931da96a6f2afef6e6dfa93b488e9b337ae29d9f195d23"
        },
        {
                "id": "712e5c22050c0700",
                "type": "hostd",
                "apiAddress": "http://[::]:53870/api",
                "password": "sia is cool",
                "walletAddress": "addr:42fd84ddb08a96105c41b31414913a1a3e09a94046d8444f65398e9b96a814a7708027a323f7"
        }
]
```

### `POST /mine` 

Mines a preset number of blocks on the testnet

**Request Body**
```json
{
	"blocks": 10
}
```
