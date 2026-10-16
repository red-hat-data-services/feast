# Go feature server (Alpha)

## Overview
The Go feature server is an HTTP/gRPC endpoint that serves features. It is written in Go.

## Configuration of `feature_store.yaml`
The current Go feature server needs a Python based feature Transformation service support. Please refer to the following code as an example:
```
# -*- coding: utf-8 -*-
from feast.feature_store import FeatureStore


def main():
    # Init the Feature Store
    store = FeatureStore(repo_path="./feature_repo/")

    # Start the feature transformation server
    # default port is 6569
    store.serve_transformations(6569)

if __name__ == "__main__":
    main()
```
At the same time, we need to configure the `feature_store.yaml` as following:

```
...
entity_key_serialization_version: 3
feature_server:
    type: local
    transformation_service_endpoint: "localhost:6569"
...
```

### Securing the transformation service connection
By default the Go feature server dials the transformation service over plaintext gRPC,
since the endpoint above points at a sidecar on `localhost` and the connection never
leaves the pod. When the transformation service runs elsewhere, enable TLS:

```
...
feature_server:
    type: local
    transformation_service_endpoint: "transformations.example.svc:6569"
    transformation_service_tls: true
    transformation_service_cert: "/etc/pki/tls/certs/service-ca.crt"
...
```

| Key | Default | Description |
|:---|:---|:---|
| `transformation_service_tls` | `false` | Dial the transformation service over TLS instead of plaintext. |
| `transformation_service_cert` | unset | Path to a PEM CA bundle trusted *in addition to* the system roots. Only used when `transformation_service_tls` is `true`. Set this when the service presents a certificate signed by a service CA or a self-signed certificate; leave it unset to verify against the host trust store alone. |

## Supported APIs
Here is the list of supported APIs:
| Method | API | Comment |
|:---: | :---: | :---: |
| POST   | /get-online-features | Retrieve features of one or many entities |
| GET    | /health              | Status of the Go Feature Server |

## OTEL based Observability
The Go feature server support [OTEL](https://opentelemetry.io/) based Observabilities.
To enable it, we need to set the global env `ENABLE_OTEL_TRACING` to `"true"` (as a string type!) in the container or your local OS.
```
export ENABLE_OTEL_TRACING='true'
```
There are example OTEL infra setup under the `/go/infra/docker/otel` folder.

## Demo
Please check the Reference[2] for a local demo of Go feature server. If you want to see a real world example of applying Go feature server in Production, please check Reference[1].

## Reference
1. [Expedia Group's Go Feature Server Implementation (in Production)](https://github.com/EXPEbdodla/feast)
2. [A Go Feature server demo from Feast](https://github.com/feast-dev/feast-credit-score-local-tutorial)