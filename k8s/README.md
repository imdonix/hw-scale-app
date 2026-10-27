# sscale Kubernetes / Helm deployment

Helm chart for running `sscale` on the single-node k3s cluster. The daemon
uses the host's `bluetoothd` over the system D-Bus socket, so it is pinned to
the node with the Bluetooth adapter (`lapiz-m`).

## Prerequisites

- The node must have BlueZ running and the adapter powered:

  ```sh
  sudo apt-get install -y bluez
  sudo systemctl enable --now bluetooth
  bluetoothctl show   # Powered: yes
  ```

- A GitHub Actions build of `imdonix/hw-scale-app` must have pushed the image.
- The `keys.json` pairing file (not committed to git).

## Install

```sh
# 1. Namespace and pairing keys (keys.json stays out of git)
kubectl create namespace sscale
kubectl -n sscale create secret generic sscale-keys --from-file=keys.json=./keys.json

# 2. Install the chart
helm upgrade --install sscale ./k8s -n sscale

# 3. Watch the rollout
kubectl -n sscale rollout status deploy/sscale
```

The web UI/API is then available at <https://scale.donix.dev>.

Get the API key (generated on first start and stored in the database):

```sh
kubectl -n sscale exec deploy/sscale -- sscale apikey
```

## Migrating existing data

The SQLite database and daemon state live on the `sscale-data` PVC. To seed it
from a local `./data` directory, scale the deployment down and copy the files
into a helper pod that mounts the PVC:

```sh
kubectl -n sscale scale deploy/sscale --replicas=0
kubectl -n sscale apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: sscale-data-seed
spec:
  restartPolicy: Never
  containers:
    - name: seed
      image: alpine:3.22
      command: ["sleep", "3600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: sscale-data
EOF
kubectl -n sscale wait --for=condition=Ready pod/sscale-data-seed
kubectl -n sscale cp ./data/sscale.db sscale-data-seed:/data/sscale.db
kubectl -n sscale exec sscale-data-seed -- chown -R 0:0 /data
kubectl -n sscale delete pod sscale-data-seed
kubectl -n sscale scale deploy/sscale --replicas=1
```

Use `sqlite3 data/sscale.db ".backup /tmp/sscale.db"` first if the source
database was used with WAL mode.

## Configuration

See [values.yaml](values.yaml). Useful overrides:

| Value | Default | Description |
|---|---|---|
| `image.tag` | `latest` | Pin to `sha-<commit>` or a digest for reproducible deploys |
| `config.adapter` | `""` | Bluetooth adapter, e.g. `hci0` |
| `config.apiKey` | `""` | Fixed API key instead of the generated one (prefer `extraEnv` + Secret) |
| `config.logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `ingress.hosts[0].host` | `scale.donix.dev` | Public hostname |
| `persistence.size` | `1Gi` | Size of the data volume |
| `nodeSelector` | `kubernetes.io/hostname: lapiz-m` | Node with the Bluetooth adapter |

### TLS

By default the ingress uses Traefik's default certificate (Cloudflare
terminates public TLS, as for the other apps on this cluster). To use a
specific certificate, set `ingress.tls[0].secretName` and create that secret.

## Uninstall

```sh
helm uninstall sscale -n sscale
kubectl -n sscale delete pvc sscale-data   # deletes the measurements
```
