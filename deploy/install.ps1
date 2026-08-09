<#
.SYNOPSIS
ติดตั้ง k3s-tencent-clb-controller ด้วยคำสั่งเดียว (เวอร์ชัน PowerShell)

.EXAMPLE
.\deploy\install.ps1 -ClusterId th1 -Region ap-bangkok -VpcId vpc-xxxxxxxx

.DESCRIPTION
รันซ้ำได้เสมอ — ค่าเดิมไม่ถูกทับด้วยค่าว่าง และคีย์เดิมไม่ถูกแตะถ้าไม่ได้สั่ง -RotateCredentials
#>
[CmdletBinding()]
param(
    # ต้องไม่ซ้ำกับคลัสเตอร์อื่นใน Tencent account เดียวกัน
    [string]$ClusterId = $env:CLUSTER_ID,
    # ต้องตรงกับภูมิภาคที่ node อยู่จริง เช่น ap-bangkok
    [string]$Region = $env:TENCENTCLOUD_REGION,
    # VPC ที่ node อยู่
    [string]$VpcId = $env:VPC_ID,
    # จำเป็นเมื่อจะใช้ internal load balancer
    [string]$SubnetId = $env:DEFAULT_SUBNET_ID,
    # override image ใน manifests.yaml
    [string]$Image,
    # ถาม secret id/key ใหม่ แม้ Secret เดิมจะมีอยู่แล้ว
    [switch]$RotateCredentials
)

$ErrorActionPreference = 'Stop'
$ns = 'kube-system'
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path

function Die($msg) { Write-Error "error: $msg"; exit 1 }

if (-not $ClusterId) { Die '-ClusterId is required' }
if (-not $Region)    { Die '-Region is required' }
if (-not $VpcId)     { Die '-VpcId is required' }

# จับค่าที่ copy มาจากเอกสารแล้วลืมแก้ ก่อนที่มันจะไปสร้าง CLB ผิดที่
if ("$ClusterId$Region$VpcId" -match 'CHANGE|REPLACE|change-me') {
    Die 'placeholder value detected; pass real values'
}
if ($VpcId -notmatch '^vpc-') { Die "-VpcId should look like vpc-xxxxxxxx, got: $VpcId" }

if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) { Die 'kubectl not found in PATH' }
kubectl version -o json 2>&1 | Out-Null
if ($LASTEXITCODE -ne 0) { Die 'cannot reach the cluster; check your kubectl context' }

Write-Host "==> context: $(kubectl config current-context)"
Write-Host "==> cluster-id=$ClusterId region=$Region vpc-id=$VpcId"

# ---------- credentials ----------
# ไม่แตะ Secret เดิมโดยไม่ได้สั่ง — การเขียนทับคีย์ที่ใช้งานอยู่ด้วยค่าว่าง
# ทำให้ controller auth ไม่ผ่านโดยไม่มีอะไรบอกสาเหตุ
kubectl -n $ns get secret tencentcloud-credentials 2>&1 | Out-Null
$secretExists = ($LASTEXITCODE -eq 0)

if (-not $secretExists -or $RotateCredentials) {
    $sid  = $env:TENCENTCLOUD_SECRET_ID
    $skey = $env:TENCENTCLOUD_SECRET_KEY

    if (-not $sid -or -not $skey) {
        $sidSecure  = Read-Host 'TENCENTCLOUD_SECRET_ID'  -AsSecureString
        $skeySecure = Read-Host 'TENCENTCLOUD_SECRET_KEY' -AsSecureString
        $sid  = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
            [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sidSecure))
        $skey = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
            [Runtime.InteropServices.Marshal]::SecureStringToBSTR($skeySecure))
    }
    if (-not $sid -or -not $skey) { Die 'credentials must not be empty' }

    Write-Host '==> writing Secret/tencentcloud-credentials'
    kubectl -n $ns create secret generic tencentcloud-credentials `
        --from-literal=TENCENTCLOUD_SECRET_ID="$sid" `
        --from-literal=TENCENTCLOUD_SECRET_KEY="$skey" `
        --dry-run=client -o yaml | kubectl apply -f -
    if ($LASTEXITCODE -ne 0) { Die 'failed to write the credentials Secret' }

    Remove-Variable sid, skey
} else {
    Write-Host '==> Secret/tencentcloud-credentials exists, leaving it alone (-RotateCredentials to replace)'
}

# ---------- config ----------
Write-Host '==> writing ConfigMap/clb-controller-config'
$cmArgs = @(
    "--from-literal=CLUSTER_ID=$ClusterId"
    "--from-literal=TENCENTCLOUD_REGION=$Region"
    "--from-literal=VPC_ID=$VpcId"
)
if ($SubnetId) { $cmArgs += "--from-literal=DEFAULT_SUBNET_ID=$SubnetId" }

kubectl -n $ns create configmap clb-controller-config @cmArgs --dry-run=client -o yaml | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { Die 'failed to write the ConfigMap' }

# ---------- workload ----------
Write-Host '==> applying manifests'
kubectl apply -f (Join-Path $scriptDir 'manifests.yaml')
if ($LASTEXITCODE -ne 0) { Die 'failed to apply manifests' }

if ($Image) {
    Write-Host "==> setting image to $Image"
    kubectl -n $ns set image deploy/clb-controller controller="$Image"
}

# ConfigMap เปลี่ยนแล้ว pod ที่รันอยู่ไม่รู้ตัว เพราะ envFrom อ่านตอน start เท่านั้น
Write-Host '==> restarting to pick up config'
kubectl -n $ns rollout restart deploy/clb-controller
kubectl -n $ns rollout status deploy/clb-controller --timeout=120s

Write-Host ''
Write-Host '==> logs'
kubectl -n $ns logs deploy/clb-controller --tail=20
Write-Host ''
Write-Host "done. ถ้าเห็น 'starting clb controller' พร้อม cluster/region/vpc ที่ถูกต้อง แปลว่าพร้อมใช้งาน"
