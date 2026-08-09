# k3s-tencent-clb-controller

Controller ขนาดเล็กที่ reconcile **เฉพาะ** `Service` type `LoadBalancer` → Tencent Cloud CLB
สำหรับ K3s / Kubernetes 1.36 ที่ใช้ Traefik เป็น ingress

ออกแบบมาแทน [tencentcloud-cloud-controller-manager](https://github.com/TencentCloud/tencentcloud-cloud-controller-manager)
ในเคสที่ต้องการแค่ "Traefik หนึ่ง Service → CLB หนึ่งตัว"
รายละเอียดการตัดสินใจเชิงออกแบบอยู่ใน [DESIGN.md](DESIGN.md)

## ทำไมไม่ใช้ CCM ตัวเต็ม

CCM ต้องเปิด `--cloud-provider=external` ซึ่งจะ taint node ทุกตัวด้วย
`node.cloudprovider.kubernetes.io/uninitialized` จนกว่า cloud provider จะ initialize สำเร็จ
ถ้า controller พังหรือ credential หมดอายุ node ใหม่จะไม่ขึ้นและ workload ไม่ถูก schedule

controller ตัวนี้เป็น Deployment ธรรมดา ไม่แตะ node lifecycle เลย
พังแล้วแค่ LB หยุด sync — คลัสเตอร์ยังทำงานปกติ

| | CCM ตัวเต็ม | ตัวนี้ |
|---|---|---|
| Controller ที่รัน | Node + Route + Service | Service เท่านั้น |
| `--cloud-provider=external` | ต้องเปิด | ไม่ต้อง |
| RBAC บน node | get/list/watch/**update/patch/delete** | get/list/watch |
| Dependency | `k8s.io/cloud-provider`, `k8s.io/kubernetes` | controller-runtime + tencentcloud-sdk-go |

## มันทำงานยังไง

```
Service (type=LoadBalancer, loadBalancerClass=clb.tencentcloud.com/external)
   │
   ├─ 1. ใส่ finalizer                      ← ก่อนแตะ cloud เสมอ
   ├─ 2. หา CLB: annotation → tag → สร้างใหม่
   │        เขียน loadbalancer-id กลับทันทีที่รู้ id
   ├─ 3. listener  ← ServicePort (CLB เปิด :80/:443 ตาม spec.ports[].port)
   ├─ 4. target    ← node + EndpointSlice (ผูก ins-xxxx:nodePort)
   └─ 5. status.loadBalancer.ingress[0].ip = VIP ของ CLB
```

CLB ทำหน้าที่ L4 ล้วน — traffic วิ่งเข้า nodePort ของ Traefik แล้ว Traefik
จัดการ TLS + routing เอง ไม่มีการทำ L7 ซ้ำบน CLB

การ resync เกิดทุก `--resync-period` (default 10 นาที) เพื่อแก้ drift ที่เกิดนอก k8s
เช่นมีคนไปลบ listener ทิ้งบน console

## ตัวอย่าง Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: traefik
  namespace: kube-system
  annotations:
    clb.tencentcloud.com/internet-charge-type: TRAFFIC_POSTPAID_BY_HOUR
    clb.tencentcloud.com/internet-max-bandwidth-out: "100"
spec:
  type: LoadBalancer
  loadBalancerClass: clb.tencentcloud.com/external
  externalTrafficPolicy: Local
  selector:
    app.kubernetes.io/name: traefik
  ports:
    - name: web
      port: 80
      targetPort: web
    - name: websecure
      port: 443
      targetPort: websecure
```

หลัง reconcile สำเร็จ Service จะมี annotation `clb.tencentcloud.com/loadbalancer-id`
เพิ่มเข้ามา และ `status.loadBalancer.ingress` จะมี VIP ของ CLB

## สถานะการพัฒนา

| ส่วน | สถานะ |
|---|---|
| สร้าง / adopt / ลบ CLB ตาม Service | ทำแล้ว |
| Listener sync (TCP/UDP) ตาม ServicePort | ทำแล้ว |
| Target sync ตาม node + EndpointSlice | ทำแล้ว |
| `externalTrafficPolicy: Local` + healthCheckNodePort | ทำแล้ว |
| Finalizer + กู้สถานะจาก tag เมื่อ crash | ทำแล้ว |
| Leader election, healthz/readyz, metrics endpoint | ทำแล้ว |
| Orphan GC (ลบ CLB ที่ไม่มี Service คู่แล้ว) | **ยังไม่ทำ** |
| Prometheus metric ของ CLB API เอง | **ยังไม่ทำ** |
| Helm chart | **ยังไม่ทำ** |
| derive `--cluster-id` จาก UID ของ namespace `kube-system` อัตโนมัติ | **ยังไม่ทำ** |
| CVM instance role (ไม่ต้องเก็บ static key) | **ยังไม่ทำ** |

**สถานะการยืนยันกับ Tencent Cloud API จริง** (k3s v1.36.2, ap-bangkok)

| ขั้น | ผล |
|---|---|
| อ่าน config จาก ConfigMap/Secret, leader election | ผ่าน |
| `DescribeLoadBalancers` ค้นด้วย tag | ผ่าน |
| `CreateLoadBalancer` + รอ async task | ผ่าน |
| `CreateListener` (HTTP health check) | ผ่าน (v0.2.1 หลังแก้ `HttpCheckDomain`) |
| `RegisterTargets` | ผ่าน |
| `status.loadBalancer` | เคยค้างว่างเพราะ CLB เป็นแบบ DNS — แก้แล้ว **ยังไม่ยืนยันซ้ำ** |
| การลบ CLB ตอนลบ Service | **ยังไม่ยืนยัน** |

> **CLB บางภูมิภาคเป็นแบบ DNS ไม่ใช่ IP** — `ap-bangkok` คืน `LoadBalancerVips: []`
> แต่ให้ `Domain` มาแทน controller จึงเขียนลง `status.loadBalancer.ingress[0].hostname`
> (แบบเดียวกับที่ AWS ELB ทำ) ไม่ใช่ `.ip` — `kubectl get svc` จะโชว์เป็นชื่อโดเมน

test อัตโนมัติทั้งหมดใช้ in-memory fake — ทำ checklist ท้ายไฟล์ก่อนขึ้น production

## ติดตั้ง

### 1. เตรียมสิทธิ์ฝั่ง Tencent Cloud

Controller เรียก API แค่ 12 ตัวนี้เท่านั้น สร้าง custom policy ชื่อ **`K3sTencentCLBController`**

```json
{
  "version": "2.0",
  "statement": [
    {
      "effect": "allow",
      "action": [
        "clb:DescribeLoadBalancers",
        "clb:CreateLoadBalancer",
        "clb:DeleteLoadBalancer",
        "clb:DescribeListeners",
        "clb:CreateListener",
        "clb:ModifyListener",
        "clb:DeleteListener",
        "clb:DescribeTargets",
        "clb:RegisterTargets",
        "clb:DeregisterTargets",
        "clb:DescribeTaskStatus",
        "cvm:DescribeInstances"
      ],
      "resource": ["*"]
    }
  ]
}
```

สองตัวที่คนมักลืมและทำให้พังแบบงงๆ:

- **`clb:DescribeTaskStatus`** — CLB API ทุกตัวที่เขียนเป็น async ถ้าไม่มีสิทธิ์นี้
  controller จะไม่รู้ว่างานเสร็จหรือยัง แล้วค้างทุก operation
- **`cvm:DescribeInstances`** — `RegisterTargets` รับ `ins-xxxx` ไม่ใช่ IP และ K3s ตั้ง
  `providerID` เป็น `k3s://<node-name>` ที่ใช้หา CVM ไม่ได้

จากนั้นสร้าง sub-user (**อย่าใช้ key ของ root account**)

| อย่าง | ชื่อที่แนะนำ | เหตุผล |
|---|---|---|
| Policy | `K3sTencentCLBController` | ใช้ร่วมกันได้ทุกคลัสเตอร์ มีชุดเดียวพอ |
| User | `k3s-clb-controller-<cluster-id>` | แยกต่อคลัสเตอร์ เพิกถอนคีย์ตัวเดียวได้โดยไม่กระทบตัวอื่น และไล่ CloudAudit ได้ |

ตอนสร้าง user เลือกแค่ **Programmatic access** — controller ไม่เคยล็อกอินหน้าเว็บ
เปิด console access ทิ้งไว้คือเพิ่มช่องโจมตีเปล่าๆ

> ถ้าเจอ Event `SyncLoadBalancerFailed` ที่มี `UnauthorizedOperation` ตอน **สร้าง** CLB
> บางบัญชีต้องมีสิทธิ์ฝั่ง tag service เพิ่มถึงจะแนบ tag ตอนสร้างได้ ลองเพิ่ม
> `tag:AddResourceTag` `tag:DescribeResourceTagsByResourceIds` `tag:GetTags`

ไฟล์ policy พร้อมใช้อยู่ใน [`deploy/cam/`](deploy/cam/)

#### ป้องกันคนแก้ CLB ผ่านหน้าเว็บ

CLB ที่ controller ดูแลถูกกำหนดโดย Kubernetes — การไปแก้บน console จะถูก reconcile
ทับกลับภายใน `--resync-period` (default 10 นาที) อยู่แล้ว ซึ่งทำให้เกิดสภาพที่แย่กว่าคือ
คนแก้แล้วเห็นว่าได้ผล เดินจากไป แล้วมันย้อนกลับเองทีหลังโดยไม่มีใครรู้

[`deploy/cam/deny-console-edit.json`](deploy/cam/deny-console-edit.json) ปิดช่องนั้นตั้งแต่ต้นทาง
โดย **deny** action ที่เป็นการแก้ไข เฉพาะ CLB ที่มี tag `k8s-managed-by=k3s-tencent-clb-controller`

```
ผูกกับ:     user/group ของคนทั่วไป
ห้ามผูกกับ:  sub-user ของ controller (ไม่งั้น controller ทำงานไม่ได้)
```

ใน CAM **deny ชนะ allow เสมอ** แม้ user จะมี `AdministratorAccess` ก็ยังแก้ไม่ได้
และเงื่อนไขผูกกับ tag ทำให้ CLB ตัวอื่นในบัญชีเดียวกันยังแก้ได้ตามปกติ

**สองชั้นนี้ครอบคนละเรื่อง — ใช้คู่กัน**

| กลไก | กันอะไร |
|---|---|
| `clb.tencentcloud.com/delete-protection: "true"` | กัน **ลบ** CLB (เป็น `DeleteProtect` ของ Tencent เอง) |
| `deploy/cam/deny-console-edit.json` | กัน **แก้ไข** listener / target / attribute |

delete-protection ครอบแค่การลบ ไม่ได้ครอบการแก้ listener — ถ้าอยากกันทั้งสองอย่างต้องใช้ทั้งคู่

> ตอนลบ Service **controller จะปิด delete protection ให้เองแล้วค่อยลบ CLB**
> พร้อม Event `ClearingDeleteProtection` ถ้าไม่ทำแบบนี้ `DeleteLoadBalancer` จะล้มเหลว
> finalizer ไม่ถูกปลด แล้ว Service ค้าง `Terminating` ตลอดกาล
> เจตนาของ protection คือกันคนพลาดจาก console ส่วน Service คือ source of truth

> การรองรับ tag condition ต่างกันไปในแต่ละ product — ทดสอบด้วย user จริงหนึ่งคน
> ก่อนเชื่อว่าใช้ได้ วิธีทดสอบ: ล็อกอินด้วย user นั้นแล้วลองแก้ listener ต้องขึ้น error สิทธิ์

### 2. Deploy

`deploy/manifests.yaml` **ไม่มีค่าที่ต้องแก้ก่อน apply** — ค่าที่ต่างกันในแต่ละคลัสเตอร์
อยู่ใน ConfigMap/Secret ที่สร้างแยก จึง `apply` ซ้ำได้ตลอดโดยไม่ทับของเดิม

**วิธีที่ง่ายที่สุด — คำสั่งเดียวจบ**

```bash
./deploy/install.sh --cluster-id th1 --region ap-bangkok --vpc-id vpc-xxxxxxxx
```

```powershell
.\deploy\install.ps1 -ClusterId th1 -Region ap-bangkok -VpcId vpc-xxxxxxxx
```

สคริปต์จะ validate ค่า, สร้าง ConfigMap + Secret, apply manifests, restart แล้วโชว์ log ให้
รันซ้ำได้เสมอ — **Secret เดิมจะไม่ถูกแตะ** เว้นแต่สั่ง `--rotate-credentials` / `-RotateCredentials`

**หรือทำเองทีละขั้น**

```bash
kubectl -n kube-system create configmap clb-controller-config \
  --from-literal=CLUSTER_ID=th1 \
  --from-literal=TENCENTCLOUD_REGION=ap-bangkok \
  --from-literal=VPC_ID=vpc-xxxxxxxx \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n kube-system create secret generic tencentcloud-credentials \
  --from-literal=TENCENTCLOUD_SECRET_ID="$SID" \
  --from-literal=TENCENTCLOUD_SECRET_KEY="$SKEY"

kubectl apply -f deploy/manifests.yaml
```

> key ของ ConfigMap/Secret ต้องเป็นชื่อ env var ที่ถูกต้อง (ไม่มีขีดกลาง) เพราะ Deployment
> ใช้ `envFrom` — key อย่าง `secret-id` จะถูก kubelet ข้ามทิ้งพร้อม warning event
> แล้ว controller จะบอกว่าหา credential ไม่เจอ

`TENCENTCLOUD_REGION` **ต้องตรงกับภูมิภาคที่ node อยู่จริง** ถ้าผิด controller จะไปสร้าง CLB
คนละภูมิภาคกับ node แล้วหา CVM ไม่เจอ ขึ้น Event `NodesNotResolvable` ทุก node

`envFrom` ถูกอ่านตอน pod start เท่านั้น — **แก้ ConfigMap หรือหมุนคีย์แล้วต้อง**
`kubectl -n kube-system rollout restart deploy/clb-controller` (install script ทำให้อัตโนมัติ)

ถ้า ConfigMap ไม่มี pod จะไม่ start เลย ขึ้น `CreateContainerConfigError` — ตั้งใจให้พังดังๆ
ดีกว่ารันด้วยค่า placeholder แล้วไปสร้าง CLB ผิดที่

#### เรื่อง `--cluster-id`

k3s ไม่มี cluster ID ในตัวให้ใช้ (ต่างจาก TKE ที่มี `cls-xxxxx`) จึงต้องกำหนดเอง
มันถูกใช้สองที่: tag `k8s-cluster-id` บน CLB และชื่อ CLB

| สถานการณ์ | จำเป็นไหม |
|---|---|
| คลัสเตอร์เดียวใน Tencent account | ไม่จำเป็นเชิงตรรกะ แต่โค้ดบังคับให้ระบุ |
| ≥2 คลัสเตอร์ใช้ account เดียวกัน | **จำเป็นมาก** |

เหตุผลของกรณีหลัง: `FindByTags` แมตช์จาก `{cluster-id, service, managed-by}`
ถ้าสองคลัสเตอร์มี `kube-system/traefik` เหมือนกัน**และ cluster-id ตรงกัน**
คลัสเตอร์ A จะ adopt CLB ของ B มาเป็นของตัวเอง แล้ว reconcile target ไปชี้ node ตัวเอง
— traffic ของ B ตายทันทีโดยไม่มีอะไรเตือน

### 3. ย้าย Traefik จาก klipper มาที่ CLB

`spec.loadBalancerClass` ทำสองอย่างพร้อมกัน: บอก ServiceLB (klipper) ให้ปล่อย Service นี้ไป
และบอก controller ตัวนี้ว่าต้องรับผิดชอบ ตาม API contract ของ Kubernetes เอง

> "Any default load balancer implementation (e.g. cloud providers) should **ignore**
> Services that set this field." — `k8s.io/api/core/v1` `ServiceSpec.LoadBalancerClass`

**แต่ field เดียวกันนั้นบอกด้วยว่า "Once set, it can not be changed"** —
เติม class ลง Service ที่เป็น type LoadBalancer อยู่แล้วไม่ได้ `helm upgrade` จะ error
`field is immutable` ต้องให้ Service ผ่านการเป็น non-LoadBalancer ก่อนหนึ่งจังหวะ

**ลำดับสำคัญ: patch ก่อน แล้วค่อย apply HelmChartConfig**
ถ้าสลับกัน helm-controller จะพยายาม patch class ลง Service เดิมแล้วโดนปฏิเสธ ทำให้ job พังค้าง

```bash
# 0. เช็คก่อนว่า controller พร้อมรับช่วง — ถ้ายังไม่รัน จะเหลือสภาพที่ klipper ก็ไปแล้ว
#    ของเราก็ไม่มา ซึ่งแย่กว่าตอนเริ่ม
kubectl -n kube-system get deploy clb-controller
kubectl -n kube-system logs deploy/clb-controller --tail=20   # ต้องเห็น "starting clb controller"
kubectl -n kube-system get svc traefik -o yaml > /tmp/traefik-svc-backup.yaml

# 1. ปลด LoadBalancer ออก — apiserver จะ wipe loadBalancerClass/nodePort/healthCheckNodePort ให้เอง
kubectl -n kube-system patch svc traefik -p '{"spec":{"type":"ClusterIP"}}'

# 2. กลับมาพร้อม class
kubectl -n kube-system patch svc traefik -p '{
  "spec": {
    "type": "LoadBalancer",
    "loadBalancerClass": "clb.tencentcloud.com/external",
    "externalTrafficPolicy": "Local"
  }
}'

# 3. ค่อย apply HelmChartConfig ให้ helm reconcile รอบหน้าไม่ patch ทับ
kubectl apply -f deploy/traefik-helmchartconfig.yaml
```

วิธีสองจังหวะนี้ทำให้ **ClusterIP และ DNS ไม่หาย** ต่างจากการ `delete svc` แล้วรอสร้างใหม่
แต่ `nodePort` จะถูก re-allocate เลขใหม่ — ถ้ามี security group หรือ firewall rule ที่
hardcode nodePort เดิมไว้ ต้องตามไปแก้ (controller อ่านค่าใหม่เองอยู่แล้ว)

**ยืนยัน**

```bash
kubectl -n kube-system get svc traefik -o custom-columns=\
'CLASS:.spec.loadBalancerClass,POLICY:.spec.externalTrafficPolicy,HCPORT:.spec.healthCheckNodePort,EXTIP:.status.loadBalancer.ingress[0].ip'

kubectl -n kube-system get pods -l svccontroller.k3s.cattle.io/svcname=traefik   # ต้องว่าง
kubectl -n kube-system describe svc traefik | tail -20   # EnsuringLoadBalancer → EnsuredLoadBalancer
```

**Rollback** — ลบ class ตรงๆ ไม่ได้ ต้องใช้ท่าเดิมกลับทาง

```bash
kubectl -n kube-system patch svc traefik -p '{"spec":{"type":"ClusterIP"}}'
kubectl -n kube-system patch svc traefik -p '{"spec":{"type":"LoadBalancer"}}'
kubectl delete helmchartconfig -n kube-system traefik
```

#### `--disable=servicelb` อย่างเดียวไม่พอ

มันหยุด klipper ได้จริง แต่ controller ตัวนี้เช็ค `loadBalancerClass` ก่อนจะรับผิดชอบ Service
Service ที่ไม่มี class จะถูกข้ามทั้งคู่ แล้ว EXTERNAL-IP ค้าง `<pending>` ตลอด — **ยังไงก็ต้องตั้ง class**

และอย่าใช้ `--disable-cloud-controller` เพราะจะเสีย node metadata handling ของ k3s ไปด้วย

## Container image

Image ถูก build และ push ขึ้น GHCR อัตโนมัติเมื่อ push git tag ที่ขึ้นต้นด้วย `v`

```bash
git tag v0.1.0
git push origin v0.1.0
```

จะได้ image หลาย tag พร้อมกัน (`linux/amd64` + `linux/arm64`):

```
ghcr.io/drsaluml/k3s-tencent-clb-controller:0.1.0
ghcr.io/drsaluml/k3s-tencent-clb-controller:0.1
ghcr.io/drsaluml/k3s-tencent-clb-controller:latest
```

> **image tag ไม่มี `v` นำหน้า** — git tag คือ `v0.1.0` แต่ `docker/metadata-action`
> ตัด `v` ออกจาก `{{version}}` ดังนั้น `:v0.1.0` จะ pull ไม่เจอ ต้องใช้ `:0.1.0`

> tag `:<major>` (เช่น `:1`) จะถูกสร้างเมื่อออกจาก 0.x แล้วเท่านั้น
> เพราะ semver ถือว่า 0.x ยัง breaking ได้ทุก minor

**ครั้งแรกที่ push ต้องตั้งค่าสองอย่าง:**

1. Settings → Actions → General → Workflow permissions → ต้องไม่ใช่ read-only
   (workflow ขอ `packages: write` ไว้แล้ว แต่ถ้าตั้ง repo เป็น read-only จะถูก override)
2. หลัง push สำเร็จ package จะเป็น **private** โดย default —
   ถ้าอยากให้ `kubectl` ดึงได้โดยไม่ต้องมี imagePullSecret ให้ไปที่
   หน้า package → Package settings → Change visibility → Public

ไม่ต้องสร้าง PAT — workflow ใช้ `GITHUB_TOKEN` ที่ Actions ให้มาอยู่แล้ว

ถ้า package เป็น private ต้องสร้าง pull secret:

```bash
kubectl -n kube-system create secret docker-registry ghcr \
  --docker-server=ghcr.io \
  --docker-username=<github-user> \
  --docker-password=<PAT ที่มีสิทธิ์ read:packages>
```
แล้วเพิ่ม `imagePullSecrets: [{name: ghcr}]` ใน Deployment

### Build เองในเครื่อง

```bash
docker build --build-arg VERSION=dev -t clb-controller:dev .
```

ตรวจว่า image ขึ้น GHCR แล้วหรือยังก่อน rollout (ไม่ต้อง login ถ้า package เป็น public):

```bash
docker manifest inspect ghcr.io/drsaluml/k3s-tencent-clb-controller:0.1.0 >/dev/null && echo ok
```

เวอร์ชันถูก stamp เข้า binary และถูก log ตอน start เสมอ —
เป็นข้อมูลแรกที่ต้องการเวลา debug ว่า pod ที่รันอยู่คือ build ไหน

## CI

| Workflow | ทำงานเมื่อ | ทำอะไร |
|---|---|---|
| `.github/workflows/ci.yml` | push `main`, ทุก PR | gofmt / vet / build / `go test -race` + ลอง build image ทั้งสอง arch (ไม่ push) |
| `.github/workflows/release.yml` | push tag `v*`, กดเอง | รัน CI ให้ผ่านก่อน แล้ว build + push ขึ้น GHCR พร้อม SBOM/provenance และสร้าง GitHub Release |

## Annotations

| Annotation | ความหมาย |
|---|---|
| `clb.tencentcloud.com/loadbalancer-id` | controller เขียนเอง — id ของ CLB ที่สร้างให้ |
| `clb.tencentcloud.com/existing-loadbalancer-id` | adopt CLB ที่มีอยู่ และ **ไม่ลบ** ตอน Service หาย |
| `clb.tencentcloud.com/subnet-id` | subnet สำหรับ internal LB |
| `clb.tencentcloud.com/address-ip-version` | `IPV4` / `IPV6FullChain` / `IPv6Nat64` |
| `clb.tencentcloud.com/sla-type` | LCU spec (เว้นว่าง = shared) |
| `clb.tencentcloud.com/internet-charge-type` | `TRAFFIC_POSTPAID_BY_HOUR` / `BANDWIDTH_POSTPAID_BY_HOUR` |
| `clb.tencentcloud.com/internet-max-bandwidth-out` | Mbps |
| `clb.tencentcloud.com/scheduler` | `WRR` / `LEAST_CONN` / `IP_HASH` |
| `clb.tencentcloud.com/session-expire-time` | วินาที |
| `clb.tencentcloud.com/delete-protection` | `"true"` เปิด 删除保护 ของ CLB — กันลบพลาดจาก console (กันแก้ไขใช้ CAM deny policy) |
| `clb.tencentcloud.com/health-check-protocol` | `TCP` / `HTTP` — override ค่าที่ controller เลือกให้ |
| `clb.tencentcloud.com/health-check-path` | ใช้เมื่อ health check เป็น HTTP |
| `clb.tencentcloud.com/health-check-domain` | Host header ของ HTTP health check (default `<svc>.<ns>`) — CLB บังคับให้มีค่า |
| `clb.tencentcloud.com/health-check-interval` | วินาที |
| `clb.tencentcloud.com/health-check-timeout` | วินาที |

บน Node:

| Annotation | ความหมาย |
|---|---|
| `clb.tencentcloud.com/instance-id` | ระบุ CVM instance id เอง ข้ามการ lookup ผ่าน CVM API |

## สิ่งที่จงใจไม่ทำ

- **L7 บน CLB (HTTP/HTTPS listener + cert)** — Traefik terminate TLS อยู่แล้ว
  การทำ L7 สองที่ทำให้ debug ยากและ cert หลุด sync
- **Direct-to-pod (ENI / VPC-CNI)** — เป็นของ TKE ไม่ใช่ K3s บน CVM ธรรมดา
- **Node / Route controller** — k3s จัดการเองอยู่แล้ว
- **Ingress resource** — ไม่แตะ

## หมายเหตุเชิงเทคนิค

**CLB API ทุกตัวที่เขียนเป็น async** — คืน `RequestId` ทันทีแต่งานยังไม่เสร็จ
ต้อง poll `DescribeTaskStatus` จนได้ status 0 ถ้ายิงคำสั่งถัดไปเลยจะเจอ error
ตระกูล `IncorrectStatus` เพราะ CLB ล็อกตัวเองอยู่ — จัดการใน `internal/clb/task.go`

**K3s ตั้ง `providerID = k3s://<node-name>`** ซึ่งใช้หา CVM ไม่ได้
แต่ `RegisterTargets` ต้องการ `ins-xxxx` — `internal/node/resolver.go` จึง lookup
ผ่าน `cvm:DescribeInstances` ด้วย private IP แล้ว cache ไว้

**Ownership 3 ชั้น** — annotation (fast path) + tag บน CLB (source of truth) + finalizer
ชั้น tag คือสิ่งที่กู้เคส "สร้าง CLB สำเร็จแล้ว crash ก่อนเขียน annotation"
ไม่งั้นได้ CLB ผีที่คิดเงินไปเรื่อยๆ (มี test ครอบไว้)

## Troubleshooting

ทุกอย่างที่สำคัญถูกรายงานเป็น Event บน Service

```bash
kubectl -n kube-system describe svc traefik | tail -20
kubectl -n kube-system logs deploy/clb-controller -f
```

| Event | ความหมาย / วิธีแก้ |
|---|---|
| `InvalidConfiguration` | annotation ผิดรูป หรือ port ยังไม่ได้ nodePort — controller จะลองใหม่ทุกนาที ไม่สร้าง CLB ด้วยค่ามั่ว |
| `NodesNotResolvable` | หา CVM ที่ตรงกับ node ไม่เจอ ถ้าเป็นบาง node จะข้ามไป ถ้าเป็นทุก node จะหยุดและ **ไม่ถอน target เดิมทิ้ง** แก้ด้วย annotation `clb.tencentcloud.com/instance-id` บน node |
| `SyncLoadBalancerFailed` | error จาก Tencent API ดู log ประกอบ — ถ้าเป็น auth/quota controller จะ backoff 5 นาทีแทน retry รัว |
| `EnsuredLoadBalancer` | ทุกอย่าง sync แล้ว |

**Service ค้าง Terminating** — แปลว่าลบ CLB ไม่สำเร็จ finalizer จึงไม่ถูกปลด
ดู Event `DeleteLoadBalancerFailed` ก่อน เคสที่พบบ่อยคือ CLB เป็นแบบ prepaid
ซึ่งลบผ่าน API ไม่ได้ ต้องลบมือบน console แล้ว controller จะปล่อย finalizer เอง
ถ้าจำเป็นจริงๆ ปลดมือด้วย `kubectl patch svc ... -p '{"metadata":{"finalizers":null}}'`
แต่ต้องไปลบ CLB เองด้วย ไม่งั้นเหลือค้างคิดเงิน

**EXTERNAL-IP ขึ้นแล้วแต่ต่อไม่ติด (connect timeout)** — CLB สร้างครบและ target ผูกแล้ว
แต่ traffic ไม่ทะลุ สาเหตุที่พบบ่อยที่สุดคือ **security group ของ node บล็อก nodePort**

โดย default traffic จาก CLB ไป backend ยังถูก security group ของ CVM ตรวจอยู่
ต้องเปิด ingress บน SG ของ node ทุกตัว:

| Port | ทำไม |
|---|---|
| `30000-32767` TCP | ช่วง nodePort ทั้งหมด (เลขเปลี่ยนทุกครั้งที่สร้าง Service ใหม่) |
| `healthCheckNodePort` | อยู่ในช่วงเดียวกัน ถ้าเปิดทั้งช่วงก็ครอบคลุมแล้ว |

แยกให้ชัดว่าเป็นฝั่งไหนก่อนแก้ — ยิงจากในคลัสเตอร์ ถ้าได้ผลแปลว่า k8s ปกติ เหลือแค่ SG:

```bash
kubectl run t --rm -i --restart=Never --image=curlimages/curl -- \
  curl -sS -o /dev/null -w '%{http_code}\n' --max-time 5 http://<NODE_IP>:<nodePort>/
```

**Service ได้ IP จาก klipper แทน** — `spec.loadBalancerClass` ไม่ได้ถูกตั้ง
เช็คด้วย `kubectl get svc traefik -o jsonpath='{.spec.loadBalancerClass}'`
ถ้า Traefik มาจาก HelmChart addon ต้องแก้ผ่าน HelmChartConfig เท่านั้น
แก้ Service ตรงๆ จะถูก reconcile ทับ

## Development

```bash
go build ./...
go test ./...
```

Layer สำคัญ:

```
internal/clb/       wrapper รอบ tencentcloud-sdk-go + async task polling + diff logic
    diff.go         pure function ล้วน — จะสร้าง/แก้/ลบอะไรบ้าง (ที่นี่มี test เยอะสุด)
    fake.go         CLB จำลอง in-memory สำหรับ test
internal/node/      node → CVM instance id + cache
internal/config/    แปลง Service + annotation → spec
internal/controller/ reconciler
```

## Checklist ก่อนขึ้น production

- [ ] สร้าง Service แล้ว CLB โผล่จริง และ `status.loadBalancer.ingress` มี VIP
- [ ] `curl` ผ่าน VIP เข้า Traefik ได้
- [ ] rolling restart Traefik แล้วไม่มี downtime
- [ ] `kubectl drain` node แล้ว target ถูกถอนออกภายในไม่กี่วินาที
- [ ] ลบ listener ทิ้งบน console แล้ว controller สร้างคืนภายใน resync period
- [ ] ลบ Service แล้ว CLB หายจริง ไม่เหลือค้าง
- [ ] kill pod controller ระหว่างสร้าง CLB แล้ว restart — ต้องไม่ได้ CLB สองตัว

## License

MIT
