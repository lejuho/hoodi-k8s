# hoodi-k8s

An Ethereum **Hoodi testnet** node (Geth + Lighthouse) running on Kubernetes (kind), deployed with **ArgoCD**, with a **Go health-check sidecar** that pulls lagging nodes out of traffic.

A personal project to reproduce day-to-day node operations work: syncing, health-based traffic control, GitOps deploys and upgrade drills.

[한국어 README](README.ko.md)

## Architecture

```
GitHub (manifests/) ──watch/sync──▶ ArgoCD
                                      │ deploy
RPC client ──▶ Service geth :8545 ──▶ Pod geth-0 [ geth (EL) | healthcheck (Go) ]
               (only when ready)            ▲
                                            │ Engine API :8551 (always reachable)
               Service geth-engine ◀── Pod lighthouse-0 (CL)
               publishNotReadyAddresses

Hoodi P2P ◀──▶ hostPort 30303 (geth) / 9000-9001 (lighthouse)
EBS /data (hostPath PV, Retain) ── chain data for both pods
```

Single EC2 host (4 vCPU / 16 GB), Ubuntu 24.04, kind (Kubernetes v1.37), ArgoCD v3.5, Geth v1.17.7, Lighthouse v8.2.

## Repository layout

| Path | What it is |
|---|---|
| `manifests/10-storage.yaml` | Namespace, hostPath PVs/PVCs (`Retain`) pointing at existing chain data |
| `manifests/20-geth.yaml` | Geth StatefulSet + health-check sidecar, `geth` and `geth-engine` Services |
| `manifests/30-lighthouse.yaml` | Lighthouse StatefulSet and Service |
| `healthcheck/` | Go readiness sidecar, unit tests, Dockerfile |
| `kind-config.yaml` | kind cluster with `/data` mount and P2P port mappings |
| `argocd-app.yaml` | ArgoCD Application (auto-sync, self-heal, no prune) |
| `docs/` | Evidence: health-check log, deploy history, upgrade drill log |

## Quick start

```bash
# 0. Data dirs and JWT (Geth/Lighthouse share it)
sudo mkdir -p /data/{geth,lighthouse,jwt}
openssl rand -hex 32 | sudo tee /data/jwt/jwt.hex

# 1. Cluster
kind create cluster --name hoodi --config kind-config.yaml
kubectl apply -f manifests/10-storage.yaml
kubectl -n hoodi create secret generic jwt --from-file=jwt.hex=/data/jwt/jwt.hex

# 2. Health-check image (tests run inside the build)
docker build -t healthcheck:v1 healthcheck/
kind load docker-image healthcheck:v1 --name hoodi

# 3. ArgoCD, then hand the manifests over to it
kubectl create namespace argocd
kubectl apply -n argocd --server-side --force-conflicts \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl apply -f argocd-app.yaml
```

Before deploying elsewhere, change the public IP in `--nat=extip:` (Geth) and `--enr-address=` (Lighthouse). Without pre-synced data in `/data`, Lighthouse checkpoint-syncs and Geth snap-syncs from scratch (about 115 GB, a few hours).

## Health check (`/ready`)

| Rule | Threshold |
|---|---|
| Geth is not syncing | `eth_syncing == false` |
| Enough peers | `net_peerCount >= 3` |
| Head is fresh | latest block younger than 60 s |
| Not behind a public node | at most 5 blocks behind `REF_URL` |

- Fails → HTTP 503 with the reasons in JSON. Kubernetes removes the pod from the `geth` Service.
- If the public reference RPC is down, it only adds a warning. An outside outage must not take our node out of traffic.
- `/healthz` only checks that the sidecar process is alive (liveness).

```bash
docker run --rm -v "$PWD/healthcheck":/src -w /src golang:1 go test -v ./...   # 11 tests
```

## What broke and how it was fixed

| Issue | Cause | Fix |
|---|---|---|
| Geth exited in 4 s on Kubernetes | Service links injected `GETH_PORT=tcp://...`, which Geth reads as `--port` | `enableServiceLinks: false` |
| Lighthouse OOMKilled at 4 Gi | Opening the beacon DB peaked at about 5.2 GiB (`memory.peak`) | limit 8 Gi |
| Node never came back after CL recovery | Readiness removed Geth from the Service that also carried the Engine API, so the CL could not reach the EL: a deadlock | Split Services; `geth-engine` uses `publishNotReadyAddresses` |
| Low peer count after the move | Hypothesis "pod doesn't know its public IP" was **rejected** by a `git revert` A/B test; peers were simply re-collecting after restart | Kept `--enr-address` as an explicit setting |
| Deploy delayed about 6 min | ArgoCD polling plus repo cache | Hard refresh; a webhook would fix it (not used: ArgoCD is not exposed) |

**Drills**
- CL outage: traffic was cut about 2 min after the CL stopped, then restored automatically once the CL came back.
- Version pin: `stable` → `v1.17.7`, so versions only change through Git commits. During the redeploy, traffic was blocked for about 20 s until the new pod caught up.

## Limitations

Testnet, single host, one replica, so each redeploy causes about 20 s of downtime. More replicas with `rollingUpdate.partition` would allow zero-downtime upgrades. The sidecar image is loaded into kind directly, without a registry. There is no metrics-based alerting yet.
