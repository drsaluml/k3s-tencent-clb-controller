#!/usr/bin/env bash
#
# ติดตั้ง k3s-tencent-clb-controller ด้วยคำสั่งเดียว
#
#   ./deploy/install.sh --cluster-id th1 --region ap-bangkok --vpc-id vpc-xxxxxxxx
#
# รันซ้ำได้เสมอ — ค่าเดิมไม่ถูกทับด้วยค่าว่าง และคีย์เดิมไม่ถูกแตะถ้าไม่ได้สั่ง --rotate-credentials
set -euo pipefail

NS=kube-system
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CLUSTER_ID=""
REGION=""
VPC_ID=""
SUBNET_ID=""
IMAGE=""
ROTATE=false

die() { echo "error: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
usage: install.sh --cluster-id ID --region REGION --vpc-id VPC [options]

required (หรือส่งผ่าน env CLUSTER_ID / TENCENTCLOUD_REGION / VPC_ID):
  --cluster-id ID       ต้องไม่ซ้ำกับคลัสเตอร์อื่นใน Tencent account เดียวกัน
  --region REGION       ต้องตรงกับภูมิภาคที่ node อยู่จริง เช่น ap-bangkok
  --vpc-id VPC          VPC ที่ node อยู่

optional:
  --subnet-id SUBNET    จำเป็นเมื่อจะใช้ internal load balancer
  --image IMAGE         override image ใน manifests.yaml
  --rotate-credentials  ถาม secret id/key ใหม่ แม้ Secret เดิมจะมีอยู่แล้ว

credentials อ่านจาก env TENCENTCLOUD_SECRET_ID / TENCENTCLOUD_SECRET_KEY ถ้ามี
ถ้าไม่มีและยังไม่เคยสร้าง Secret จะถามแบบไม่ echo ลงจอ
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --cluster-id)          CLUSTER_ID="$2"; shift 2 ;;
    --region)              REGION="$2";     shift 2 ;;
    --vpc-id)              VPC_ID="$2";     shift 2 ;;
    --subnet-id)           SUBNET_ID="$2";  shift 2 ;;
    --image)               IMAGE="$2";      shift 2 ;;
    --rotate-credentials)  ROTATE=true;     shift ;;
    -h|--help)             usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

# fallback ไป env ถ้าไม่ได้ส่งผ่าน flag
[ -n "$CLUSTER_ID" ] || CLUSTER_ID="$(printenv CLUSTER_ID || true)"
[ -n "$REGION" ]     || REGION="$(printenv TENCENTCLOUD_REGION || true)"
[ -n "$VPC_ID" ]     || VPC_ID="$(printenv VPC_ID || true)"
[ -n "$SUBNET_ID" ]  || SUBNET_ID="$(printenv DEFAULT_SUBNET_ID || true)"

[ -n "$CLUSTER_ID" ] || { usage; die "--cluster-id is required"; }
[ -n "$REGION" ]     || { usage; die "--region is required"; }
[ -n "$VPC_ID" ]     || { usage; die "--vpc-id is required"; }

# จับค่าที่ copy มาจากเอกสารแล้วลืมแก้ ก่อนที่มันจะไปสร้าง CLB ผิดที่
case "$CLUSTER_ID$REGION$VPC_ID" in
  *CHANGE*|*change-me*|*REPLACE*) die "placeholder value detected; pass real values" ;;
esac
case "$VPC_ID" in
  vpc-*) ;;
  *) die "--vpc-id should look like vpc-xxxxxxxx, got: $VPC_ID" ;;
esac

command -v kubectl >/dev/null || die "kubectl not found in PATH"
kubectl version -o json >/dev/null 2>&1 || die "cannot reach the cluster; check your kubectl context"

echo "==> context: $(kubectl config current-context)"
echo "==> cluster-id=$CLUSTER_ID region=$REGION vpc-id=$VPC_ID"

# ---------- credentials ----------
# ไม่แตะ Secret เดิมโดยไม่ได้สั่ง — การเขียนทับคีย์ที่ใช้งานอยู่ด้วยค่าว่าง
# ทำให้ controller auth ไม่ผ่านโดยไม่มีอะไรบอกสาเหตุ
secret_exists=false
kubectl -n "$NS" get secret tencentcloud-credentials >/dev/null 2>&1 && secret_exists=true

if [ "$secret_exists" = false ] || [ "$ROTATE" = true ]; then
  SID="$(printenv TENCENTCLOUD_SECRET_ID || true)"
  SKEY="$(printenv TENCENTCLOUD_SECRET_KEY || true)"
  if [ -z "$SID" ] || [ -z "$SKEY" ]; then
    [ -t 0 ] || die "no TTY and TENCENTCLOUD_SECRET_ID/KEY not set; cannot obtain credentials"
    read -rs -p "TENCENTCLOUD_SECRET_ID: "  SID; echo
    read -rs -p "TENCENTCLOUD_SECRET_KEY: " SKEY; echo
  fi
  [ -n "$SID" ] && [ -n "$SKEY" ] || die "credentials must not be empty"

  echo "==> writing Secret/tencentcloud-credentials"
  kubectl -n "$NS" create secret generic tencentcloud-credentials \
    --from-literal=TENCENTCLOUD_SECRET_ID="$SID" \
    --from-literal=TENCENTCLOUD_SECRET_KEY="$SKEY" \
    --dry-run=client -o yaml | kubectl apply -f -
  unset SID SKEY
else
  echo "==> Secret/tencentcloud-credentials exists, leaving it alone (--rotate-credentials to replace)"
fi

# ---------- config ----------
echo "==> writing ConfigMap/clb-controller-config"
cm_args=(
  --from-literal=CLUSTER_ID="$CLUSTER_ID"
  --from-literal=TENCENTCLOUD_REGION="$REGION"
  --from-literal=VPC_ID="$VPC_ID"
)
# ต้องใช้ if ไม่ใช่ `[ ] && ...` เพราะ set -e จะทำให้สคริปต์จบทันทีเมื่อ test เป็นเท็จ
if [ -n "$SUBNET_ID" ]; then
  cm_args+=(--from-literal=DEFAULT_SUBNET_ID="$SUBNET_ID")
fi

kubectl -n "$NS" create configmap clb-controller-config "${cm_args[@]}" \
  --dry-run=client -o yaml | kubectl apply -f -

# ---------- workload ----------
echo "==> applying manifests"
kubectl apply -f "$SCRIPT_DIR/manifests.yaml"

if [ -n "$IMAGE" ]; then
  echo "==> setting image to $IMAGE"
  kubectl -n "$NS" set image deploy/clb-controller controller="$IMAGE"
fi

# ConfigMap เปลี่ยนแล้ว pod ที่รันอยู่ไม่รู้ตัว เพราะ envFrom อ่านตอน start เท่านั้น
echo "==> restarting to pick up config"
kubectl -n "$NS" rollout restart deploy/clb-controller
kubectl -n "$NS" rollout status deploy/clb-controller --timeout=120s

echo
echo "==> logs"
kubectl -n "$NS" logs deploy/clb-controller --tail=20
echo
echo "done. ถ้าเห็น 'starting clb controller' พร้อม cluster/region/vpc ที่ถูกต้อง แปลว่าพร้อมใช้งาน"
