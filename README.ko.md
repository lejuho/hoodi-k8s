# hoodi-k8s

이더리움 **Hoodi 테스트넷** 노드(Geth + Lighthouse)를 Kubernetes(kind)에서 운영하는 프로젝트입니다. **ArgoCD**로 배포하고, **Go 헬스체크 사이드카**가 뒤처진 노드를 트래픽에서 자동으로 뺍니다.

노드 운영 업무(동기화, 상태 기반 트래픽 제어, GitOps 배포, 업그레이드 훈련)를 직접 재현해 본 개인 프로젝트입니다.

[English README](README.md)

## 구성

```
GitHub (manifests/) ──감시/동기화──▶ ArgoCD
                                       │ 배포
RPC 클라이언트 ──▶ Service geth :8545 ──▶ Pod geth-0 [ geth (EL) | healthcheck (Go) ]
                  (ready일 때만 연결)          ▲
                                               │ Engine API :8551 (항상 연결)
                  Service geth-engine ◀── Pod lighthouse-0 (CL)
                  publishNotReadyAddresses

Hoodi P2P ◀──▶ hostPort 30303 (geth) / 9000-9001 (lighthouse)
EBS /data (hostPath PV, Retain) ── 두 파드의 체인 데이터
```

EC2 1대(4 vCPU / 16GB), Ubuntu 24.04, kind(Kubernetes v1.37), ArgoCD v3.5, Geth v1.17.7, Lighthouse v8.2.

## 저장소 구성

| 경로 | 내용 |
|---|---|
| `manifests/10-storage.yaml` | 네임스페이스, 기존 체인 데이터를 가리키는 hostPath PV/PVC (`Retain`) |
| `manifests/20-geth.yaml` | Geth StatefulSet과 헬스체크 사이드카, `geth`·`geth-engine` Service |
| `manifests/30-lighthouse.yaml` | Lighthouse StatefulSet과 Service |
| `healthcheck/` | Go readiness 사이드카, 단위 테스트, Dockerfile |
| `kind-config.yaml` | `/data` 마운트와 P2P 포트를 연결한 kind 클러스터 설정 |
| `argocd-app.yaml` | ArgoCD Application (자동 동기화, self-heal, prune 끔) |
| `docs/` | 증거 자료: 헬스체크 로그, 배포 이력, 업그레이드 훈련 기록 |

## 실행 방법

```bash
# 0. 데이터 디렉터리와 JWT (Geth와 Lighthouse가 같이 사용)
sudo mkdir -p /data/{geth,lighthouse,jwt}
openssl rand -hex 32 | sudo tee /data/jwt/jwt.hex

# 1. 클러스터
kind create cluster --name hoodi --config kind-config.yaml
kubectl apply -f manifests/10-storage.yaml
kubectl -n hoodi create secret generic jwt --from-file=jwt.hex=/data/jwt/jwt.hex

# 2. 헬스체크 이미지 (빌드 중 테스트 실행)
docker build -t healthcheck:v1 healthcheck/
kind load docker-image healthcheck:v1 --name hoodi

# 3. ArgoCD 설치 후 매니페스트 배포를 맡김
kubectl create namespace argocd
kubectl apply -n argocd --server-side --force-conflicts \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl apply -f argocd-app.yaml
```

다른 환경에 배포할 때는 Geth의 `--nat=extip:`와 Lighthouse의 `--enr-address=`에 들어간 공인 IP를 바꾸세요. `/data`에 미리 동기화한 데이터가 없으면 Lighthouse는 체크포인트 싱크, Geth는 snap sync로 처음부터 받습니다(약 115GB, 몇 시간 소요).

## 헬스체크 (`/ready`)

| 조건 | 기준 |
|---|---|
| 동기화 완료 | `eth_syncing == false` |
| 피어 충분 | `net_peerCount >= 3` |
| head가 최신 | 최신 블록이 60초 이내 |
| 외부 노드 대비 | `REF_URL`보다 5블록 이내 |

- 조건을 하나라도 못 맞추면 HTTP 503과 실패 이유를 JSON으로 반환합니다. Kubernetes가 해당 파드를 `geth` Service에서 뺍니다.
- 외부 기준 RPC가 응답하지 않으면 경고만 남깁니다. 외부 서비스 장애 때문에 우리 노드가 트래픽에서 빠지면 안 되기 때문입니다.
- `/healthz`는 사이드카 프로세스가 살아 있는지만 확인합니다(liveness).

```bash
docker run --rm -v "$PWD/healthcheck":/src -w /src golang:1 go test -v ./...   # 테스트 11개
```

## 겪은 문제와 해결

| 문제 | 원인 | 해결 |
|---|---|---|
| Kubernetes에서 Geth가 4초 만에 종료 | Service links가 넣은 `GETH_PORT=tcp://...`를 Geth가 `--port` 플래그로 읽음 | `enableServiceLinks: false` |
| Lighthouse가 4Gi에서 OOMKilled | 비콘 DB를 열 때 최대 약 5.2GiB 사용 (`memory.peak`로 측정) | limit 8Gi |
| CL을 복구해도 노드가 돌아오지 않음 | readiness 실패로 Geth가 Service에서 빠지면서, 같은 Service로 연결하던 Engine API까지 끊겨 CL이 EL에 접속하지 못하는 데드락 발생 | Service 분리, `geth-engine`에 `publishNotReadyAddresses` 설정 |
| 이관 후 피어 수 감소 | "파드가 공인 IP를 모른다"는 가설을 세웠으나 `git revert` A/B 비교로 **기각**. 재시작 직후 피어를 다시 모으는 시간 때문이었음 | `--enr-address`는 명시 설정으로 유지 |
| 배포가 약 6분 늦게 반영됨 | ArgoCD 폴링 주기와 저장소 캐시 | hard refresh로 대응. Webhook이 근본 해결이지만 ArgoCD를 외부에 공개하지 않아 미적용 |

**훈련**
- CL 장애: CL을 멈추면 약 2분 뒤 트래픽이 차단되고, CL을 복구하면 자동으로 다시 연결됩니다.
- 버전 고정: `stable` → `v1.17.7`. 이제 버전은 Git 커밋으로만 바뀝니다. 재배포 중에는 새 파드가 따라잡을 때까지 약 20초간 트래픽이 차단됐습니다.

## 한계

테스트넷, 서버 1대, 레플리카 1개 구성이라 재배포할 때마다 약 20초 중단이 생깁니다. 레플리카를 늘리고 `rollingUpdate.partition`으로 한 대씩 올리면 무중단 업그레이드가 가능합니다. 사이드카 이미지는 레지스트리 없이 kind에 직접 넣었고, 메트릭 기반 알림은 아직 없습니다.
