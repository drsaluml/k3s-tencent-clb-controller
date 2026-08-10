# k3s-tencent-clb-controller

Controller เล็กๆ ที่ reconcile `Service` type `LoadBalancer` → Tencent Cloud CLB
สำหรับ K3s / Kubernetes 1.36 ที่ใช้ Traefik เป็น ingress

ทำมาแทน [tencentcloud-cloud-controller-manager](https://github.com/TencentCloud/tencentcloud-cloud-controller-manager)
ในเคสที่ต้องการแค่ "Traefik หนึ่ง Service → CLB หนึ่งตัว"
เหตุผลเชิงออกแบบอยู่ใน [DESIGN.md](DESIGN.md)

## ทำไมไม่ใช้ CCM ตัวเต็ม

CCM ต้องเปิด `--cloud-provider=external` ซึ่ง taint node ทุกตัวด้วย
`node.cloudprovider.kubernetes.io/uninitialized` จนกว่าจะ initialize สำเร็จ
ถ้า controller พังหรือ credential หมดอายุ node ใหม่จะไม่ขึ้นและ workload ไม่ถูก schedule

ตัวนี้เป็น Deployment ธรรมดา ไม่แตะ node lifecycle เลย พังแล้วแค่ LB หยุด sync
คลัสเตอร์ยังทำงานปกติ

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
   ├─ 3. listener  ← ServicePort
   ├─ 4. target    ← node + EndpointSlice (ผูก ins-xxxx:nodePort)
   └─ 5. status.loadBalancer.ingress
```

CLB ทำ L4 ล้วน — traffic วิ่งเข้า nodePort ของ Traefik แล้ว Traefik จัดการ TLS กับ
routing เอง ไม่ทำ L7 ซ้ำบน CLB

resync ทุก `--resync-period` (default 10 นาที) เพื่อดึง drift ที่เกิดนอก k8s กลับมา
เช่นมีคนไปลบ listener ทิ้งบน console

### Orphan GC

CLB จะกลายเป็น orphan เมื่อ Service หายไปโดยที่ finalizer ไม่ได้ทำงาน — ลบตอน
controller ดับ, ลบทั้ง namespace จน finalizer ถูก force ทิ้ง, หรือ `DeleteLoadBalancer`
ล้มเหลวแล้วไม่มีใครกลับมาดู ทุกเคสจบเหมือนกันคือ CLB ที่ไม่มีใครใช้แต่ยังคิดเงิน

ตัวเก็บกวาดเดินทุก `--orphan-gc-interval` (default 1 ชั่วโมง) เทียบ CLB ที่ติด tag
ของคลัสเตอร์นี้กับ Service ที่มีอยู่จริง

```
--orphan-gc-interval      1h     รอบการตรวจ, 0 = ปิด
--orphan-gc-grace-period  30m    ต้องดูเหมือน orphan ติดต่อกันเท่านี้ก่อนถึงจะลบได้
--orphan-gc-delete        false  ค่าเริ่มต้นคือรายงานลง log อย่างเดียว
```

**ค่าเริ่มต้นไม่ลบอะไรทั้งนั้น** — มันบอกว่าเจออะไรบ้างใน log แล้วให้คนตัดสินใจ
การได้ตัวลบ cloud resource อัตโนมัติมาฟรีจากการอัปเกรดเวอร์ชันไม่ใช่เรื่องที่ควรเกิด
เปิด `--orphan-gc-delete` เมื่อเชื่อรายงานแล้ว

กันพลาดไว้สี่ชั้น: อ่าน Service ตรงจาก apiserver ไม่ผ่าน cache (cache ที่ sync
ไม่เสร็จจะทำให้ Service ทุกตัวดูเหมือนหายไป), error ใดๆ ยกเลิกทั้งรอบโดยไม่ลบอะไรเลย,
ต้องเห็นเป็น orphan ติดต่อกันจนพ้น grace period, และกรองด้วย `k8s-cluster-id`
เสมอจึงไม่แตะ CLB ของคลัสเตอร์อื่นใน account เดียวกัน

> CLB ที่ adopt มาด้วย `existing-loadbalancer-id` ไม่เคยถูกติด tag ของเรา
> GC จึงมองไม่เห็นตั้งแต่แรก

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

reconcile สำเร็จแล้ว Service จะได้ annotation `clb.tencentcloud.com/loadbalancer-id`
กับค่าใน `status.loadBalancer.ingress` เพิ่มมา

## สถานะการพัฒนา

**ทำแล้ว** — สร้าง/adopt/ลบ CLB, listener sync (TCP/UDP), target sync ตาม node +
EndpointSlice, `externalTrafficPolicy: Local` พร้อม healthCheckNodePort, override
health check ผ่าน annotation, finalizer + กู้สถานะจาก tag เมื่อ crash, delete
protection (เปิดเป็นค่าเริ่มต้น), security group + `pass-to-target`, CLB แบบ DNS
ที่ไม่มี VIP, resolve node → CVM พร้อม cache, rate limit + รอ async task,
leader election / healthz / metrics, CAM policy สำเร็จรูป, CI + release ขึ้น GHCR

**ยังไม่ทำ** — metric ของ CLB API, Helm chart, derive `--cluster-id` อัตโนมัติ,
CVM instance role (ยังใช้ static key), e2e test ที่ยิง API จริง (test ทั้งหมดใช้
in-memory fake)

**ยืนยันกับ API จริงแล้วแค่ไหน** (k3s v1.36.2, ap-bangkok) — แยกออกมาเพราะ
"โค้ดเขียนแล้ว" กับ "รู้ว่าใช้ได้จริง" ไม่ใช่เรื่องเดียวกัน

| ขั้น | ผล |
|---|---|
| config, leader election, ค้น CLB ด้วย tag | ผ่าน |
| `CreateLoadBalancer` + รอ async task | ผ่าน |
| `CreateListener` (HTTP health check) | ผ่าน (v0.2.1 หลังแก้ `HttpCheckDomain`) |
| `RegisterTargets`, `status.loadBalancer` เป็น hostname | ผ่าน |
| traffic ทะลุ CLB ถึง Traefik | ผ่าน — ต้องใช้ `pass-to-target` ดู Troubleshooting |
| `ModifyLoadBalancerAttributes` | ผ่าน — delete protection กับ pass-to-target ติดจริง |
| `SetLoadBalancerSecurityGroups` | ผ่าน — ผูกแล้วกรอง traffic จริง |
| ลบ CLB ตอนลบ Service | ผ่าน — ปิด protection เอง ลบ CLB แล้วปล่อย finalizer ใน 11 วินาที |
| CLB แบบ `INTERNAL` | ผ่าน — ได้ VIP ในซับเน็ตของ node (ต่างจาก OPEN ที่ได้ domain) |
| listener UDP | ผ่านหลังแก้สองจุด — `DeregisterTargetRst` และ health check (ดูด้านล่าง) |
| ลบ listener ทิ้งแล้ว controller สร้างคืน | ผ่าน — สร้างคืนใน **578 วิ** ด้วย listener id ใหม่ (`resync-period=10m`) ไม่มีอะไรเร่งให้เร็วกว่านี้เพราะการแก้ของบนคลาวด์ไม่มี watch มาบอก |
| rolling restart Traefik | **ไม่ผ่าน — downtime ~10 วิ** วัดผ่าน Cloudflare ได้ 521 หนึ่งครั้งกับ timeout สองครั้ง ไม่ใช่บั๊ก controller แต่เป็น target churn ดูด้านล่าง |
| kill leader ระหว่างสร้าง CLB | ผ่าน — pod ตายหลัง `CreateLoadBalancer` สำเร็จแต่ก่อนเขียน id ลง Service (`SyncLoadBalancerFailed: recording load balancer id on service: context canceled`) ตัวใหม่ค้นเจอด้วย tag แล้ว adopt ต่อ ไม่ได้สร้างซ้ำ |
| orphan GC (report-only) | ผ่าน — รอบแรกบน v0.2.8 ได้ `sweep finished owned=1 watching=0 orphaned=0 deleted=0` ตรงกับที่เทียบกับ `DescribeLoadBalancers` เอง (CLB ที่ tag ไว้ตัวเดียว Service ยังอยู่) ยังไม่เคยรันในโหมดลบจริง |
| `IPV6FullChain` | **บัญชีนี้ไม่รองรับ** — `Uin ... do not support create IPv6 full chain loadbalancer` ต้องขอเปิดกับ Tencent |
| CAM deny policy กันแก้จาก console | **ยังไม่ยืนยัน** — ต้องลองด้วย user จริง |

**UDP listener มีข้อห้ามสองข้อที่ error message ไม่ได้บอกพร้อมกัน** เจอทีละอัน
เพราะอันแรกบังอันที่สองอยู่:

- **ห้ามส่ง `DeregisterTargetRst`** Tencent นับ flag นี้บน UDP เป็นฟีเจอร์ 重调度
  ที่บัญชีต้องได้สิทธิ์ก่อน → `FailedOperation: Uin does not support reschedule
  function.` ตัว flag เองก็ไม่มีความหมายกับ UDP เพราะไม่มี connection ให้ RST
- **health check ต้องเป็น `CUSTOM` เท่านั้น** → `InvalidParameterValue:
  HealthCheck.CheckType should be CUSTOM in UDP listener.` ซึ่ง CUSTOM ต้องมี
  `SendContext`/`RecvContext` เป็น payload เฉพาะแอป ไม่มีค่ากลางที่ probe nodePort
  ทั่วไปได้ controller จึง **ปิด health check ของ UDP ไปเลย**

ทั้งสองเคสจบเหมือนกันคือ listener สร้างไม่ได้ แต่ CLB ถูกสร้างไปแล้วและคิดเงินต่อ

> UDP ที่ไม่มี health check แปลว่า CLB แยกไม่ออกว่า node ไหนมี pod อยู่
> คู่กับ `externalTrafficPolicy: Local` ที่ kube-proxy ทิ้ง packet บน node ที่ไม่มี
> pod ผลคือ traffic หายเงียบๆ บางส่วน — controller เตือนด้วย Event
> `UDPHealthCheckUnavailable` ใช้ `Cluster` แทนถ้าไม่จำเป็นต้องได้ source IP จริง

> **CLB บางภูมิภาคเป็น DNS ไม่ใช่ IP** — `ap-bangkok` คืน `LoadBalancerVips: []`
> แต่ให้ `Domain` มาแทน controller จึงเขียนลง `ingress[0].hostname` ไม่ใช่ `.ip`

## ติดตั้ง

### 1. เตรียมสิทธิ์ฝั่ง Tencent Cloud

Controller เรียก API แค่ 14 ตัวนี้ สร้าง custom policy ชื่อ **`K3sTencentCLBController`**

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
        "clb:ModifyLoadBalancerAttributes",
        "clb:SetLoadBalancerSecurityGroups",
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

> ⚠️ **`deploy/cam/*.json` เป็นแค่เทมเพลต แก้ไฟล์ไม่เปลี่ยนอะไรบนคลาวด์**
> ต้องเอาไปอัปเดต policy ตัวจริงบน CAM console ทุกครั้ง ไม่งั้น controller ยิง API
> แล้วโดนปฏิเสธ โผล่เป็น Event บน Service เท่านั้น — เสียเวลาไล่หาบั๊กที่ไม่มีอยู่

สี่ตัวที่คนมักลืมแล้วพังแบบงงๆ:

| Action | ขาดแล้วเป็นยังไง |
|---|---|
| `clb:DescribeTaskStatus` | CLB API ที่เขียนเป็น async ทุกตัว ไม่มีสิทธิ์นี้แล้วไม่รู้ว่างานจบยัง ค้างทุก operation |
| `cvm:DescribeInstances` | `RegisterTargets` รับ `ins-xxxx` ไม่ใช่ IP และ `providerID` ของ k3s ใช้หา CVM ไม่ได้ |
| `clb:ModifyLoadBalancerAttributes` | delete protection (ซึ่งเปิดเป็นค่าเริ่มต้น) กับ `pass-to-target` ไม่ทำงาน แต่ CLB สร้างได้ปกติ จึงดูไม่ออกว่าเป็นเรื่องสิทธิ์ |
| `clb:SetLoadBalancerSecurityGroups` | annotation `security-groups` ไม่ทำงาน |

จากนั้นสร้าง sub-user (**อย่าใช้ key ของ root account**) เลือกแค่ **Programmatic access**
— controller ไม่เคยล็อกอินหน้าเว็บ เปิด console access ทิ้งไว้คือเพิ่มช่องโจมตีเปล่าๆ

| อย่าง | ชื่อที่แนะนำ | เหตุผล |
|---|---|---|
| Policy | `K3sTencentCLBController` | ใช้ร่วมกันได้ทุกคลัสเตอร์ |
| User | `k3s-clb-controller-<cluster-id>` | แยกต่อคลัสเตอร์ เพิกถอนคีย์ตัวเดียวได้ และไล่ CloudAudit ง่าย |

> `UnauthorizedOperation` ตอน **สร้าง** CLB — บางบัญชีต้องมีสิทธิ์ฝั่ง tag ด้วยถึงจะ
> แนบ tag ตอนสร้างได้ ลองเพิ่ม `tag:AddResourceTag`
> `tag:DescribeResourceTagsByResourceIds` `tag:GetTags`

#### ป้องกันคนแก้ CLB ผ่านหน้าเว็บ

CLB ที่ controller ดูแลถูกกำหนดโดย Kubernetes การไปแก้บน console จะถูก reconcile
ทับกลับอยู่แล้ว ซึ่งให้สภาพที่แย่กว่าคือคนแก้เห็นว่าได้ผล เดินจากไป
แล้วมันย้อนกลับเองทีหลังโดยไม่มีใครรู้

สามชั้นนี้ครอบคนละเรื่อง ใช้ร่วมกัน:

| กลไก | กันอะไร | ต้องตั้งค่าไหม |
|---|---|---|
| `delete-protection` | **ลบ** CLB | ไม่ — เปิดเป็นค่าเริ่มต้น |
| [`deploy/cam/deny-console-edit.json`](deploy/cam/deny-console-edit.json) | **แก้** listener / target / attribute | ผูก policy กับ user/group ของคนเอง |
| resync ทุก 10 นาที | **ดึงกลับ** สิ่งที่ถูกแก้ไปแล้ว | ไม่ |

deny policy ผูกกับคนทั่วไปเท่านั้น **ห้ามผูกกับ sub-user ของ controller** ไม่งั้น
controller ทำงานไม่ได้ ใน CAM deny ชนะ allow เสมอแม้ user จะมี `AdministratorAccess`
และเงื่อนไขผูกกับ tag `k8s-managed-by` ทำให้ CLB ตัวอื่นในบัญชียังแก้ได้ตามปกติ

> การรองรับ tag condition ต่างกันไปในแต่ละ product — ล็อกอินด้วย user จริงหนึ่งคน
> แล้วลองแก้ listener ต้องขึ้น error สิทธิ์ ถึงจะเชื่อว่าใช้ได้

> **ลบ Service = สั่งให้ CLB หายไป** controller จะปิด delete protection ให้เอง
> (Event `ClearingDeleteProtection`) แล้วค่อยลบ ถ้าไม่ทำแบบนี้ `DeleteLoadBalancer`
> จะล้มเหลว finalizer ไม่ถูกปลด แล้ว Service ค้าง `Terminating` ตลอดกาล
> protection มีไว้กันคนพลาดจาก console — Service คือ source of truth

> **CLB API ไม่มี "modification protection" ให้เปิด** `AttributeFlags` รับแค่
> `DeleteProtect`, `UserInVisible`, `BlockStatus`, `NoLBNat`, `BanStatus`,
> `ShiftupFlag`, `Stop` ส่วน `DescribeLBOperateProtect` อ่านได้อย่างเดียว
> ที่ TKE เรียก `modification-protection` คือ CCM ตรวจเจอแล้วดึงกลับเอง ไม่ใช่ธง
> บนคลาวด์ — resync บวก deny policy ให้ผลเดียวกันและกันตั้งแต่ต้นทางด้วย

### 2. Deploy

`deploy/manifests.yaml` ไม่มีค่าที่ต้องแก้ก่อน apply — ค่าที่ต่างกันในแต่ละคลัสเตอร์
อยู่ใน ConfigMap/Secret ที่สร้างแยก apply ซ้ำได้ตลอดโดยไม่ทับของเดิม

```bash
./deploy/install.sh --cluster-id th1 --region ap-bangkok --vpc-id vpc-xxxxxxxx
```
```powershell
.\deploy\install.ps1 -ClusterId th1 -Region ap-bangkok -VpcId vpc-xxxxxxxx
```

สคริปต์ validate ค่า สร้าง ConfigMap + Secret, apply, restart แล้วโชว์ log ให้
รันซ้ำได้เสมอ — Secret เดิมไม่ถูกแตะเว้นแต่สั่ง `--rotate-credentials`

<details>
<summary>ทำเองทีละขั้น</summary>

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
</details>

สามเรื่องที่ทำให้พังบ่อย:

- key ของ ConfigMap/Secret ต้องเป็นชื่อ env var ที่ถูกต้อง (ไม่มีขีดกลาง) เพราะใช้
  `envFrom` — key อย่าง `secret-id` จะถูก kubelet ข้ามทิ้งเงียบๆ
- `TENCENTCLOUD_REGION` ต้องตรงกับภูมิภาคที่ node อยู่จริง ผิดแล้วจะสร้าง CLB คนละที่
  กับ node แล้วขึ้น `NodesNotResolvable` ทุก node
- `envFrom` อ่านตอน pod start เท่านั้น — แก้ ConfigMap หรือหมุนคีย์แล้วต้อง
  `rollout restart` (install script ทำให้อัตโนมัติ)

ถ้า ConfigMap ไม่มี pod จะไม่ start เลย ขึ้น `CreateContainerConfigError` — ตั้งใจให้พังดังๆ
ดีกว่ารันด้วยค่า placeholder แล้วไปสร้าง CLB ผิดที่

#### `--cluster-id` จำเป็นแค่ไหน

k3s ไม่มี cluster ID ในตัว (ต่างจาก TKE ที่มี `cls-xxxxx`) จึงต้องกำหนดเอง ใช้เป็น
tag `k8s-cluster-id` บน CLB และเป็นส่วนหนึ่งของชื่อ CLB

คลัสเตอร์เดียวใน account ไม่จำเป็นเชิงตรรกะ แต่โค้ดบังคับให้ระบุ — **ตั้งแต่ 2
คลัสเตอร์ขึ้นไปใน account เดียวกันคือเรื่องคอขาดบาดตาย** เพราะ `FindByTags` แมตช์จาก
`{cluster-id, service, managed-by}` ถ้าสองคลัสเตอร์มี `kube-system/traefik` เหมือนกัน
และ cluster-id ตรงกัน คลัสเตอร์ A จะ adopt CLB ของ B มาแล้ว reconcile target ไปชี้
node ตัวเอง — traffic ของ B ตายทันทีโดยไม่มีอะไรเตือน

### 3. ย้าย Traefik จาก klipper มาที่ CLB

`spec.loadBalancerClass` ทำสองอย่างพร้อมกัน: บอก klipper ให้ปล่อย Service นี้ไป
และบอก controller ตัวนี้ว่าต้องรับผิดชอบ แต่ field นี้ **immutable** — เติม class ลง
Service ที่เป็น LoadBalancer อยู่แล้วไม่ได้ (`helm upgrade` จะ error `field is
immutable`) ต้องให้มันผ่านการเป็น non-LoadBalancer ก่อนหนึ่งจังหวะ

**patch ก่อน แล้วค่อย apply HelmChartConfig** สลับลำดับแล้ว helm-controller จะพยายาม
patch class ลง Service เดิมแล้วโดนปฏิเสธ job พังค้าง

```bash
# 0. เช็คว่า controller พร้อมรับช่วง — ถ้ายังไม่รัน จะเหลือสภาพที่ klipper ก็ไปแล้ว
#    ของเราก็ไม่มา ซึ่งแย่กว่าตอนเริ่ม
kubectl -n kube-system logs deploy/clb-controller --tail=20   # ต้องเห็น "starting clb controller"
kubectl -n kube-system get svc traefik -o yaml > /tmp/traefik-svc-backup.yaml

# 1. ปลด LoadBalancer ออก — apiserver จะ wipe class/nodePort/healthCheckNodePort ให้เอง
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

สองจังหวะนี้ทำให้ ClusterIP กับ DNS ไม่หาย ต่างจากการ `delete svc` แล้วสร้างใหม่
แต่ `nodePort` จะได้เลขใหม่ — ถ้ามี firewall rule ที่ hardcode เลขเดิมไว้ต้องตามไปแก้

**ยืนยัน**

```bash
kubectl -n kube-system get svc traefik -o custom-columns=\
'CLASS:.spec.loadBalancerClass,POLICY:.spec.externalTrafficPolicy,HCPORT:.spec.healthCheckNodePort,ADDR:.status.loadBalancer.ingress[0]'

kubectl -n kube-system get pods -l svccontroller.k3s.cattle.io/svcname=traefik   # ต้องว่าง
kubectl -n kube-system describe svc traefik | tail -20   # ต้องจบที่ EnsuredLoadBalancer
```

**Rollback** — ลบ class ตรงๆ ไม่ได้ ต้องใช้ท่าเดิมกลับทาง

```bash
kubectl -n kube-system patch svc traefik -p '{"spec":{"type":"ClusterIP"}}'
kubectl -n kube-system patch svc traefik -p '{"spec":{"type":"LoadBalancer"}}'
kubectl delete helmchartconfig -n kube-system traefik
```

> `--disable=servicelb` อย่างเดียวไม่พอ — มันหยุด klipper ได้จริง แต่ controller ตัวนี้
> เช็ค `loadBalancerClass` ก่อนรับผิดชอบ Service ที่ไม่มี class จะถูกข้ามทั้งคู่แล้ว
> EXTERNAL-IP ค้าง `<pending>` ตลอด และอย่าใช้ `--disable-cloud-controller`
> เพราะจะเสีย node metadata handling ของ k3s ไปด้วย

## Annotations

บน Service — ทุกตัวใช้ prefix `clb.tencentcloud.com/`

| Annotation | ความหมาย |
|---|---|
| `loadbalancer-id` | controller เขียนเอง — id ของ CLB ที่สร้างให้ |
| `existing-loadbalancer-id` | adopt CLB ที่มีอยู่ และ **ไม่ลบ** ตอน Service หาย |
| `subnet-id` | subnet สำหรับ internal LB |
| `address-ip-version` | `IPV4` / `IPV6FullChain` / `IPv6Nat64` |
| `sla-type` | LCU spec (เว้นว่าง = shared) |
| `internet-charge-type` | `TRAFFIC_POSTPAID_BY_HOUR` / `BANDWIDTH_POSTPAID_BY_HOUR` |
| `internet-max-bandwidth-out` | Mbps |
| `scheduler` | `WRR` / `LEAST_CONN` / `IP_HASH` |
| `session-expire-time` | วินาที |
| `delete-protection` | **default `"true"`** ใส่ `"false"` เพื่อปิด |
| `security-groups` | `sg-aaa,sg-bbb` ผูก SG กับตัว CLB (สูงสุด 5 ตัว ลำดับ = ลำดับความสำคัญ) |
| `pass-to-target` | `"true"` ให้ CLB ถึง nodePort ได้โดยไม่ผ่าน SG ของ node |
| `health-check-protocol` | `TCP` / `HTTP` — override ค่าที่ controller เลือกให้ |
| `health-check-path` | ใช้เมื่อ health check เป็น HTTP |
| `health-check-domain` | Host header ของ HTTP health check (default `<svc>.<ns>`) — CLB บังคับให้มีค่า |
| `health-check-interval` / `health-check-timeout` | วินาที |

บน Node:

| Annotation | ความหมาย |
|---|---|
| `instance-id` | ระบุ CVM instance id เอง ข้ามการ lookup ผ่าน CVM API |

ไม่รับชื่อ annotation ของ CCM/TKE เป็น alias แต่รูปแบบค่าเหมือนกัน ย้ายมาจาก TKE
([เอกสาร](https://www.tencentcloud.com/ind/document/product/457/39142)) แก้แค่ prefix:
`service.cloud.tencent.com/security-groups` และ `.../pass-to-target`
→ `clb.tencentcloud.com/...`

## Container image

push git tag ที่ขึ้นต้นด้วย `v` แล้ว CI จะ build + push ขึ้น GHCR ให้เอง
ทั้ง `linux/amd64` และ `linux/arm64`

```bash
git tag v0.1.0 && git push origin v0.1.0
# → ghcr.io/drsaluml/k3s-tencent-clb-controller:0.1.0 / :0.1 / :latest
```

> **image tag ไม่มี `v` นำหน้า** — `docker/metadata-action` ตัดออก `:v0.1.0` จึง
> pull ไม่เจอ ต้องใช้ `:0.1.0` ส่วน `:<major>` จะมีเมื่อออกจาก 0.x แล้ว

ครั้งแรกต้องตั้งสองอย่าง (ไม่ต้องสร้าง PAT — workflow ใช้ `GITHUB_TOKEN`):
Workflow permissions ต้องไม่ใช่ read-only, และ package ที่ push ครั้งแรกจะเป็น
private ให้เปลี่ยนเป็น public ที่ Package settings → Change visibility
ถ้าไม่อยากใช้ imagePullSecret

build เองในเครื่อง: `docker build --build-arg VERSION=dev -t clb-controller:dev .`
เวอร์ชันถูก stamp เข้า binary และ log ตอน start เสมอ — เป็นข้อมูลแรกที่ต้องการ
เวลา debug ว่า pod ที่รันอยู่คือ build ไหน

| Workflow | ทำงานเมื่อ | ทำอะไร |
|---|---|---|
| `ci.yml` | push `main`, ทุก PR | gofmt / vet / build / `go test -race` + ลอง build image สอง arch (ไม่ push) |
| `release.yml` | push tag `v*`, กดเอง | รัน CI ให้ผ่านก่อน แล้ว push ขึ้น GHCR พร้อม SBOM/provenance และสร้าง Release |

## สิ่งที่จงใจไม่ทำ

- **L7 บน CLB (HTTP/HTTPS listener + cert)** — Traefik terminate TLS อยู่แล้ว
  ทำ L7 สองที่ทำให้ debug ยากและ cert หลุด sync
- **Direct-to-pod (ENI / VPC-CNI)** — เป็นของ TKE ไม่ใช่ K3s บน CVM ธรรมดา
- **Node / Route controller** — k3s จัดการเองอยู่แล้ว
- **Ingress resource** — ไม่แตะ
- **`cleanup-policy: retain`** — ลบ Service = ลบ CLB เสมอ พิจารณาแล้วไม่ทำ
  (2026-08-09) เพราะ CLB ไร้เจ้าของยังคิดเงินและยังไม่มี Orphan GC มาเก็บ
  ถ้าต้องการ CLB ที่ไม่ถูกลบให้สร้างเองแล้ว adopt ด้วย `existing-loadbalancer-id`

## หมายเหตุเชิงเทคนิค

**CLB API ทุกตัวที่เขียนเป็น async** คืน `RequestId` ทันทีแต่งานยังไม่เสร็จ ต้อง poll
`DescribeTaskStatus` จนได้ status 0 ยิงคำสั่งถัดไปเลยจะเจอ `IncorrectStatus`
เพราะ CLB ล็อกตัวเองอยู่ — อยู่ใน `internal/clb/task.go`

**k3s ตั้ง `providerID = k3s://<node-name>`** ซึ่งใช้หา CVM ไม่ได้ แต่ `RegisterTargets`
ต้องการ `ins-xxxx` — `internal/node/resolver.go` จึง lookup ด้วย private IP แล้ว cache ไว้

**Ownership 3 ชั้น** annotation (fast path) + tag บน CLB (source of truth) + finalizer
ชั้น tag คือสิ่งที่กู้เคส "สร้าง CLB สำเร็จแล้ว crash ก่อนเขียน annotation" ไม่งั้นได้
CLB ผีที่คิดเงินไปเรื่อยๆ (มี test ครอบไว้)

**target churn ทำให้ rolling restart มี downtime** — วัดได้ ~10 วิ (2026-08-10)
เกิดจาก `externalTrafficPolicy: Local` + pod ย้าย node ทุกครั้งที่ deploy ไม่ใช่บั๊ก
ของ controller เพราะ target ของ CLB คือ *node* ไม่ใช่ pod พอ pod ย้ายจาก node A
ไป node B ต้องมีช่วงที่ CLB ยังชี้ A ที่ไม่มี pod แล้ว log ยืนยันว่า controller
ตามแก้ให้ 4 รอบ (`targets synced registered=1 deregistered=1`) แต่ระหว่างนั้น
request ที่วิ่งเข้า node เดิมตาย

ที่ทำแล้วช่วยได้บางส่วนคือขยาย Traefik เป็น 2 replica คนละ node — เดิม replica เดียว
ทำให้ตอน restart ไม่เหลือ node ที่รับ traffic ได้เลย

ที่ยังแก้ไม่จบคือ pod **ย้าย node** ทุกรอบ เพราะ `podAntiAffinity` เป็น `required`
และมี 3 node ให้ 2 replica พอ `maxSurge: 1` สร้าง pod ใหม่ scheduler จะเลือก node
ที่ว่างเสมอ ทางที่ควรลองต่อ: ทำให้ตำแหน่ง pod นิ่งข้ามการ deploy (replica เท่าจำนวน
node) หรือยอมสละ source IP แล้วกลับไปใช้ `externalTrafficPolicy: Cluster`
ซึ่ง node ไหนก็รับ traffic ได้ ไม่ต้องมี target churn เลย

## Troubleshooting

ทุกอย่างที่สำคัญถูกรายงานเป็น Event บน Service

```bash
kubectl -n kube-system describe svc traefik | tail -20
kubectl -n kube-system logs deploy/clb-controller -f
```

| Event | ความหมาย / วิธีแก้ |
|---|---|
| `InvalidConfiguration` | annotation ผิดรูป หรือ port ยังไม่ได้ nodePort — ลองใหม่ทุกนาที ไม่สร้าง CLB ด้วยค่ามั่ว |
| `NodesNotResolvable` | หา CVM ที่ตรงกับ node ไม่เจอ บาง node จะข้ามไป ทุก node จะหยุดและ **ไม่ถอน target เดิม** แก้ด้วย annotation `instance-id` บน node |
| `SyncLoadBalancerFailed` | error จาก Tencent API — ถ้าเป็น auth/quota จะ backoff 5 นาทีแทน retry รัว |
| ↳ พร้อม `UnauthorizedOperation` | **สิทธิ์ CAM ขาด ไม่ใช่บั๊ก** ชื่อ action อยู่ในข้อความ error ไปเพิ่มใน policy ตัวจริงบน console |
| `BackendSecurityGroupBypassed` | เปิด `pass-to-target` แต่ไม่ได้ผูก SG ให้ CLB — nodePort รับทุกคนที่ยิงถึง CLB ได้ |
| `UDPHealthCheckUnavailable` | UDP + `externalTrafficPolicy: Local` — CLB health check UDP ไม่ได้ traffic ที่ไปลง node ที่ไม่มี pod จึงหายเงียบๆ |
| `EnsuredLoadBalancer` | sync แล้ว |

**Service ค้าง Terminating** = ลบ CLB ไม่สำเร็จ finalizer จึงไม่ถูกปลด ดู Event
`DeleteLoadBalancerFailed` ก่อน เคสที่พบบ่อยคือ CLB เป็นแบบ prepaid ซึ่งลบผ่าน API
ไม่ได้ ต้องลบมือบน console แล้ว controller จะปล่อย finalizer เอง ถ้าจำเป็นจริงๆ
ปลดมือด้วย `kubectl patch svc ... -p '{"metadata":{"finalizers":null}}'` แต่ต้องไป
ลบ CLB เองด้วย ไม่งั้นเหลือค้างคิดเงิน

**ต่อไม่ติด / connect timeout ทั้งที่ CLB มีที่อยู่แล้ว** — สาเหตุที่พบบ่อยที่สุดคือ
security group ของ node บล็อก nodePort เพราะ traffic จาก CLB ไป backend ยังถูก SG
ของ CVM ตรวจอยู่ตาม default เลือกทางแก้ทางใดทางหนึ่ง

**ทาง ก. เปิด `30000-32767` TCP บน SG ของ node** ทุกตัว (ทำได้แค่บน console —
controller ไม่มีสิทธิ์แก้ SG) ครอบ `healthCheckNodePort` ไปด้วยเพราะอยู่ในช่วงเดียวกัน

**ทาง ข. ให้ CLB ข้าม SG ของ node ไปเลย** สั่งได้จาก Service ไม่ต้องแตะ console
และไม่ต้องเปิดช่วง port กว้างทิ้งไว้

```yaml
metadata:
  annotations:
    clb.tencentcloud.com/pass-to-target: "true"
    # ย้ายด่านกรองมาไว้ที่ CLB — ไม่ใส่ = ใครยิงถึง CLB ได้ ก็ถึง nodePort ได้หมด
    clb.tencentcloud.com/security-groups: "sg-xxxxxxxx"
```

`pass-to-target` ทำให้ Tencent ตรวจแค่ SG ของ **CLB** ไม่ตรวจ SG ของ CVM ปลายทาง
ด่านกรองจึงเหลือชั้นเดียว controller เตือนด้วย Event `BackendSecurityGroupBypassed`
เมื่อเปิดโดยไม่ผูก SG แต่ยังทำตามที่สั่ง

สองกับดักของ `security-groups`:

- **SG ของ CLB คนละบทบาทกับ SG ของ node** ของ CLB คุมว่าใครยิงเข้า CLB ได้
  ของ node คุมว่าใครยิงเข้า node ได้ เอา SG ของ node มาผูกกับ CLB ถ้ามันไม่มีกฎเปิด
  80/443 ให้คนนอก traffic จะตันทั้งเส้น ยิ่งเมื่อ `pass-to-target` เปิดอยู่
  เพราะ SG ของ CLB คือด่านเดียวที่เหลือ
- **ลบบรรทัด annotation ทิ้งไม่ได้ถอด SG ออก** ทั้งสองตัวถูกจัดการก็ต่อเมื่อใส่ไว้
  เท่านั้น ไม่ใส่ = "ไม่เข้าไปยุ่ง" (กัน CLB ที่ adopt มาถูกถอด SG เงียบๆ)
  ถอดจริงต้องสั่งด้วยค่าว่าง `security-groups: ""`

แยกให้ชัดว่าปัญหาอยู่ฝั่งไหนก่อนแก้ ยิงจากในคลัสเตอร์ ถ้าได้ผลแปลว่า k8s ปกติ เหลือแค่ SG:

```bash
kubectl run t --rm -i --restart=Never --image=curlimages/curl -- \
  curl -sS -o /dev/null -w '%{http_code}\n' --max-time 5 http://<NODE_IP>:<nodePort>/
```

**Service ได้ IP จาก klipper แทน** = `spec.loadBalancerClass` ไม่ได้ถูกตั้ง เช็คด้วย
`kubectl get svc traefik -o jsonpath='{.spec.loadBalancerClass}'` ถ้า Traefik มาจาก
HelmChart addon ต้องแก้ผ่าน HelmChartConfig เท่านั้น แก้ Service ตรงๆ จะถูก reconcile ทับ

## Development

```bash
go build ./... && go test ./...
```

```
internal/clb/        wrapper รอบ tencentcloud-sdk-go + async task polling + diff logic
    diff.go          pure function ล้วน — จะสร้าง/แก้/ลบอะไรบ้าง (test เยอะสุดอยู่ที่นี่)
    fake.go          CLB จำลอง in-memory สำหรับ test
internal/node/       node → CVM instance id + cache
internal/config/     Service + annotation → spec
internal/controller/ reconciler
```

## Checklist ก่อนขึ้น production

- [ ] อัปเดต policy ตัวจริง **บน CAM console** ให้ตรงกับ `deploy/cam/controller-policy.json`
      รอบล่าสุด — แก้ไฟล์ในรีโปอย่างเดียวไม่มีผลใดๆ
- [x] สร้าง Service แล้ว CLB โผล่จริง และ `status.loadBalancer.ingress` มีค่า
      (`ip` หรือ `hostname` แล้วแต่ภูมิภาค)
- [x] `curl` ผ่าน CLB เข้า Traefik ได้
- [ ] rolling restart Traefik แล้วไม่มี downtime
      **ทดสอบแล้วไม่ผ่าน (2026-08-10)** — ยังมี downtime ~10 วิ ดู "target churn"
      ด้านล่าง ต้องแก้ topology ก่อน ไม่ใช่แก้ controller
- [ ] `kubectl drain` node แล้ว target ถูกถอนออกภายในไม่กี่วินาที
- [x] ลบ listener ทิ้งบน console แล้ว controller สร้างคืนภายใน resync period
      (2026-08-10 ลบผ่าน API บน CLB ของ smoke test สร้างคืนใน 578 วิ)
- [x] ลบ Service แล้ว CLB หายจริง ไม่เหลือค้าง (smoke-test 2026-08-09)
- [x] kill pod controller ระหว่างสร้าง CLB แล้ว restart — ต้องไม่ได้ CLB สองตัว
      (2026-08-10 ดูตารางยืนยันด้านบน)
- [x] ผูก `security-groups` แล้ว traffic จากนอก SG ถูกบล็อกจริง
- [ ] ลอง deny policy ด้วย user จริงหนึ่งคน — แก้ listener บน console ต้องขึ้น error สิทธิ์

## License

MIT
