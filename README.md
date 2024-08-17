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

## `* /nodes/proxy/:filter/*path`

Proxies a request to the node with the matching ID or type. The filter can be
either the node ID or the node type. The path is the path to the API endpoint on
the node.

The request body is streamed and proxied to each matching node and the response array is
returned back to the client. Since the response for each proxied request is buffered
in memory, it is not recommended to use this endpoint when a large response is expected.

This is particularly useful for updating settings for all nodes in a cluster.

Example:
```sh
curl http://localhost:3001/nodes/proxy/hostd/api/state
```
```json
[
        {
                "nodeID": "7762f569131ca047",
                "statusCode": 200,
                "data": {
                        "publicKey": "ed25519:b6c094cebfd8e4b321eedcbced94df0df7afae3ac811812fb0c8bc1e1b3412bf",
                        "lastAnnouncement": {
                                "index": {
                                        "height": 213,
                                        "id": "bid:ea2f489547baeea20037dad4074b56a33baabd430822b18715f0da1049fbae17"
                                },
                                "address": "[::]:56884"
                        },
                        "startTime": "2024-08-16T20:37:53.543713-07:00",
                        "explorer": {
                                "enabled": true,
                                "url": "https://api.siascan.com"
                        },
                        "version": "?",
                        "commit": "?",
                        "os": "darwin",
                        "buildTime": "1969-12-31T16:00:00-08:00"
                }
        },
        {
                "nodeID": "1541a08b2d365c1f",
                "statusCode": 200,
                "data": {
                        "publicKey": "ed25519:b8d51d2dd3902c1c78d278871d40ced7e07e8b3f1051e53c8b2fdf65a8376eff",
                        "lastAnnouncement": {
                                "index": {
                                        "height": 222,
                                        "id": "bid:8e3d7aab6e612c402afcb785bd757a3b7fd5e31cd3e76603d22a6b45c05d59ac"
                                },
                                "address": "[::]:56887"
                        },
                        "startTime": "2024-08-16T20:37:53.543713-07:00",
                        "explorer": {
                                "enabled": true,
                                "url": "https://api.siascan.com"
                        },
                        "version": "?",
                        "commit": "?",
                        "os": "darwin",
                        "buildTime": "1969-12-31T16:00:00-08:00"
                }
        },
        {
                "nodeID": "1e6b324862be4fac",
                "statusCode": 200,
                "data": {
                        "publicKey": "ed25519:fadf3d2b5856ffe077a8b6753529a3dbfabd5355e898c06f013bac74563b48a2",
                        "lastAnnouncement": {
                                "index": {
                                        "height": 213,
                                        "id": "bid:ea2f489547baeea20037dad4074b56a33baabd430822b18715f0da1049fbae17"
                                },
                                "address": "[::]:56881"
                        },
                        "startTime": "2024-08-16T20:37:53.543713-07:00",
                        "explorer": {
                                "enabled": true,
                                "url": "https://api.siascan.com"
                        },
                        "version": "?",
                        "commit": "?",
                        "os": "darwin",
                        "buildTime": "1969-12-31T16:00:00-08:00"
                }
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
