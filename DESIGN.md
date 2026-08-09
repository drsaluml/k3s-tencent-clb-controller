# k3s-tencent-clb-controller — Design

Lightweight controller ที่ reconcile **เฉพาะ** `Service` type `LoadBalancer` → Tencent Cloud CLB
สำหรับ K3s/Kubernetes v1.36 ที่ใช้ Traefik เป็น ingress

---

## 1. ทำไมไม่ใช้ tencentcloud-cloud-controller-manager

| ประเด็น | CCM ตัวเต็ม | Controller ตัวนี้ |
|---|---|---|
| Scope | Node + Route + Service + Instance metadata | Service เท่านั้น |
| ต้อง `--cloud-provider=external` | ใช่ | **ไม่** |
| Node taint `node.cloudprovider.kubernetes.io/uninitialized` | ใช่ (node ไม่ Ready จนกว่า CCM จะ init) | ไม่มี |
| สมมติฐาน TKE (VPC-CNI, ENI direct pod, node จาก TKE nodepool) | ผูกอยู่พอสมควร | ไม่ผูก |
| k8s vendored libs | ตามที่ upstream pin ไว้ | pin เองตาม 1.36 |
| Blast radius เวลาพัง | node lifecycle ทั้งคลัสเตอร์ | แค่ LB ไม่แตะ node |

ข้อสำคัญที่สุดสำหรับ K3s: **การเปิด `--cloud-provider=external` จะ taint node ทุกตัวจนกว่า cloud provider จะ initialize**
ถ้า controller พัง/creds หมดอายุ → node ใหม่ไม่ขึ้น, workload ไม่ scheduling
Controller ตัวนี้เป็น Deployment ธรรมดา พังแล้วแค่ LB ไม่ sync — cluster ยังทำงานปกติ

**ทางเลือกที่ไม่เลือก:** fork CCM แล้วรัน `--controllers=service` อย่างเดียว — โค้ดน้อยกว่าจริง แต่ยังลาก dependency tree
และ node/instance abstraction ของ TKE มาทั้งชุด และยังต้องแก้ปัญหา providerID เหมือนกัน

---

## 2. Scope

**In scope**
- `Service` ที่มี `spec.loadBalancerClass: clb.tencentcloud.com/external` (หรือ `/internal`)
- Backend mode: **NodePort** (node IP + nodePort) — ทำงานกับ flannel/ทุก CNI
- Protocol: TCP / UDP listener (Traefik terminate TLS เอง → CLB เป็น L4 passthrough)
- Adopt CLB ที่มีอยู่แล้ว (BYO), สร้างใหม่, และ GC เมื่อ Service ถูกลบ
- `externalTrafficPolicy: Local` + `healthCheckNodePort`

**Out of scope (จงใจ)**
- Node controller / Route controller / node providerID lifecycle
- Direct-to-pod (ENI/VPC-CNI) — เป็นของ TKE ไม่ใช่ K3s บน CVM ธรรมดา
- L7 listener + rule + cert management บน CLB — Traefik ทำแล้ว อย่าทำซ้ำสองที่
- Ingress resource — ไม่แตะ

---

## 3. สถาปัตยกรรม

```
┌─────────────────────────────────────────────┐
│ K3s control plane                           │
│                                             │
│  Service(traefik, type=LoadBalancer,        │
│          loadBalancerClass=clb.../external) │
│  EndpointSlice(traefik)                     │
│  Node                                       │
└───────────────┬─────────────────────────────┘
                │ watch (controller-runtime)
                ▼
┌─────────────────────────────────────────────┐
│ clb-controller (Deployment, replicas=2,     │
│                 leader election)            │
│                                             │
│  Reconciler ── NodeResolver(cache) ──┐      │
│       │                              │      │
│       └── CLBClient (async-aware) ───┤      │
└──────────────────────────────────────┼──────┘
                                       ▼
                          Tencent Cloud API
                          clb  2018-03-17
                          cvm  2017-03-12
                                       │
                                       ▼
                       CLB ── listener :80/:443
                              └─ targets: [ins-xxx:31234, ...]
                                          (node ที่มี traefik pod)
```

---

## 4. Ownership model

ปัญหาคลาสสิกของ LB controller คือ "CLB ตัวนี้เป็นของใคร" ใช้ 3 ชั้น:

1. **Annotation บน Service** — `clb.tencentcloud.com/loadbalancer-id: lb-xxxxxxxx`
   เขียนกลับทันทีหลังสร้างสำเร็จ (เขียน **ก่อน** สร้าง listener เสมอ) เป็น fast path
2. **Tag บน CLB** (source of truth) —
   ```
   k8s-cluster-id = <cluster-id จาก config>
   k8s-service    = <namespace>/<name>
   k8s-managed-by = k3s-tencent-clb-controller
   ```
   ใช้ตอน GC orphan และตอนกู้เมื่อ annotation หาย
3. **Finalizer** — `clb.tencentcloud.com/resources` บน Service
   กัน Service ถูกลบก่อนที่ CLB จะถูกเก็บกวาด → ไม่เกิด orphan ที่คิดเงินต่อ

**กฎ adoption:** ถ้า user ใส่ `clb.tencentcloud.com/existing-loadbalancer-id` เอง
→ controller จะ **ไม่** ใส่ tag ownership และ **ไม่ลบ** CLB ตอน Service หาย (แค่ deregister targets + ลบ listener ที่ตัวเองสร้าง)

**Orphan GC:** ทุก resync period (default 10m) ดึง CLB ที่มี tag `k8s-cluster-id=<เรา>`
→ ตัวไหนไม่มี Service คู่กันแล้ว → ลบ (มี flag `--orphan-gc=false` ให้ปิดได้ + dry-run ใน log ก่อนลบเสมอ)

---

## 5. Reconcile loop

```go
func (r *ServiceReconciler) Reconcile(ctx, req) (Result, error) {
    svc := get(req)
    if notFound            { return ok }            // finalizer จัดการไปแล้ว
    if !isOurClass(svc)    { return ok }            // loadBalancerClass ไม่ตรง

    if svc.DeletionTimestamp != nil {
        if hasFinalizer(svc) {
            if err := r.cleanup(ctx, svc); err != nil { return retry(err) }
            removeFinalizer(svc)
        }
        return ok
    }
    ensureFinalizer(svc)

    spec  := parseAnnotations(svc)                  // desired CLB spec
    lb    := r.ensureLoadBalancer(ctx, svc, spec)   // adopt / find-by-tag / create
    patchAnnotation(svc, lbIDKey, lb.ID)            // ทันที กัน leak

    desiredListeners := listenersFrom(svc.Spec.Ports, spec)
    r.reconcileListeners(ctx, lb, desiredListeners) // create / modify / delete

    targets := r.resolveTargets(ctx, svc)           // §6
    r.reconcileTargets(ctx, lb, targets)            // diff → Register / Deregister

    updateStatus(svc, lb.VIP)                       // status.loadBalancer.ingress
    recordEvent(svc, "EnsuredLoadBalancer")
    return RequeueAfter(resyncPeriod)               // แก้ drift ที่คนไปแก้บน console
}
```

**Watches**
- `Service` — predicate กรอง `loadBalancerClass` ตั้งแต่ informer เพื่อไม่กิน memory
- `EndpointSlice` → map กลับเป็น Service ผ่าน label `kubernetes.io/service-name` (สำคัญกับ `Local` policy)
- `Node` → enqueue ทุก Service ที่ใช้ `Cluster` policy (node เข้า/ออก)

**Requeue** — workqueue key = Service key → **การันตี serialize ต่อ CLB โดยอัตโนมัติ**
(1 Service = 1 CLB) ตั้ง `MaxConcurrentReconciles` ได้อย่างปลอดภัย

---

## 6. Node → CVM InstanceId (จุดที่ต้องระวังที่สุด)

CLB `RegisterTargets` รับ `InstanceId` (`ins-xxxxxxxx`) ไม่ใช่ IP

แต่ **K3s ตั้ง `node.spec.providerID = k3s://<node-name>`** จาก embedded CCM ของมันเอง
→ ใช้ providerID หา CVM ไม่ได้ ต้อง resolve เอง 3 ทาง (ตามลำดับความสำคัญ):

1. Annotation บน Node — `clb.tencentcloud.com/instance-id: ins-xxxx`
   (ทางหนีไฟ + ใช้กับ hybrid/นอก VPC)
2. **Lookup ด้วย private IP** — `cvm:DescribeInstances` filter `private-ip-address=<InternalIP ของ node>`
   เป็น default path
3. ถ้าไม่เจอ → เขียน Event `NodeNotResolvable` บน Service, ข้าม node นั้น, **ไม่ fail ทั้ง reconcile**
   (node นอก VPC / node ที่คนเพิ่มมือ ไม่ควรทำให้ LB ทั้งตัวค้าง)

**Cache** — map `nodeName → instanceID` TTL 1h, invalidate เมื่อ node ถูกลบหรือ InternalIP เปลี่ยน
CVM API มี rate limit ต่ำกว่าที่คิด อย่าเรียกทุก reconcile

---

## 7. Target selection & health check

| `externalTrafficPolicy` | targets ที่ register | health check |
|---|---|---|
| `Cluster` (default) | ทุก Ready node ที่ไม่ unschedulable/ไม่มี taint ที่กำหนดใน `--excluded-taints` | TCP บน `nodePort` |
| `Local` | เฉพาะ node ที่มี ready endpoint ของ Service นั้น (อ่านจาก EndpointSlice) | **HTTP บน `spec.healthCheckNodePort` path `/healthz`** |

`Local` คือค่าที่แนะนำสำหรับ Traefik: ได้ client source IP จริง (`X-Forwarded-For` ถูกต้อง),
ไม่มี SNAT hop ข้าม node, และ kube-proxy ตอบ `healthCheckNodePort` เป็น 503 อัตโนมัติเมื่อ pod ย้ายออกจาก node
→ CLB ถอน node ออกเองโดยที่ controller ไม่ต้องทำอะไร (defence in depth คู่กับ EndpointSlice watch)

**Weight** — ตั้ง 10 เท่ากันหมด (ค่ามาตรฐาน CLB) ไม่ทำ weighted routing

---

## 8. Tencent Cloud API layer

**APIs ที่ใช้ (ทั้งหมดที่ต้องใช้ มีแค่นี้)**

| Action | Product | ใช้ตอน |
|---|---|---|
| `DescribeLoadBalancers` | clb | หา CLB จาก tag/ID |
| `CreateLoadBalancer` | clb | สร้าง |
| `DeleteLoadBalancer` | clb | GC |
| `DescribeListeners` | clb | diff |
| `CreateListener` / `ModifyListener` / `DeleteListener` | clb | diff |
| `DescribeTargets` | clb | diff |
| `RegisterTargets` / `DeregisterTargets` | clb | diff |
| `DescribeTaskStatus` | clb | **poll async ทุก mutating call** |
| `DescribeInstances` | cvm | node → instanceId |

### 8.1 ทุก mutating API ของ CLB เป็น async

นี่คือกับดักอันดับหนึ่ง API ตอบ `RequestId` กลับมาทันทีแต่งานยังไม่เสร็จ
ต้อง poll `DescribeTaskStatus(TaskId=RequestId)` จนได้ `Status`:
`0` = สำเร็จ, `1` = ล้มเหลว, `2` = กำลังทำ

```go
func (c *client) doSync(ctx, call func() (string, error)) error {
    reqID, err := call()
    if err != nil { return err }
    return wait.PollUntilContextTimeout(ctx, 2*time.Second, 5*time.Minute, true,
        func(ctx) (bool, error) {
            st, err := c.DescribeTaskStatus(ctx, reqID)
            switch {
            case err != nil:  return false, nil       // transient → poll ต่อ
            case st == 0:     return true, nil
            case st == 1:     return false, fmt.Errorf("clb task %s failed", reqID)
            default:          return false, nil
            }
        })
}
```

ถ้ายิงคำสั่งถัดไปโดยไม่รอ → เจอ error ตระกูล `FailedOperation` / `IncorrectStatus`
เพราะ CLB ล็อกตัวเองระหว่างมี task ค้าง

### 8.2 Concurrency & rate limit

- **ต่อ CLB หนึ่งตัว: ทำได้ทีละ 1 operation** — บังคับโดย workqueue key ตาม §5
- Client-side rate limiter (`golang.org/x/time/rate`) ต่อ region, default 10 qps
- Retry: exponential backoff + jitter บน error ตระกูล `RequestLimitExceeded`,
  `FailedOperation.*InOperating`, `InternalError` — **ไม่ retry** บน `InvalidParameter*`, `UnauthorizedOperation`
- ทุก API call มี metric `clb_api_requests_total{action,code}` + `clb_api_duration_seconds`

### 8.3 Idempotency

Reconcile ต้องเรียกซ้ำได้เสมอ ทุก ensure* เป็น read-diff-write:
`Describe*` ก่อน แล้วค่อยตัดสินใจ create/modify/delete — ไม่มี "สร้างแล้วจำไว้ใน memory"

---

## 9. Annotations

```yaml
# --- ตัวตน / adoption ---
clb.tencentcloud.com/loadbalancer-id           # controller เขียนเอง (output)
clb.tencentcloud.com/existing-loadbalancer-id  # BYO: adopt แต่ไม่ลบ

# --- ตอนสร้าง (immutable หลังสร้างแล้ว — เปลี่ยนต้องลบสร้างใหม่) ---
clb.tencentcloud.com/vpc-id
clb.tencentcloud.com/subnet-id                 # จำเป็นเมื่อ class=internal
clb.tencentcloud.com/address-ip-version        # IPV4 | IPV6FullChain | IPv6Nat64
clb.tencentcloud.com/sla-type                  # LCU spec, เว้นว่าง = shared

# --- แก้ได้ระหว่างทาง ---
clb.tencentcloud.com/internet-charge-type      # TRAFFIC_POSTPAID_BY_HOUR | BANDWIDTH_POSTPAID_BY_HOUR
clb.tencentcloud.com/internet-max-bandwidth-out
clb.tencentcloud.com/scheduler                 # WRR | LEAST_CONN
clb.tencentcloud.com/session-expire-time
clb.tencentcloud.com/health-check-protocol     # TCP | HTTP
clb.tencentcloud.com/health-check-path
clb.tencentcloud.com/health-check-interval
clb.tencentcloud.com/security-groups
```

**หลัก:** annotation ที่ immutable ให้ validate แล้วเขียน Event `ImmutableFieldChanged` แทนที่จะไปลบ CLB ทิ้งเอง
— controller ไม่ควรทำลาย production LB เพราะคนพิมพ์ annotation ผิด

---

## 10. Deployment

### 10.1 ฝั่ง K3s — ปิด ServiceLB (klipper)

**วิธีที่แนะนำ: ใช้ `loadBalancerClass`** — ServiceLB ของ K3s จะข้าม Service ที่มี
`spec.loadBalancerClass` ตั้งไว้ ทำให้ทั้งสองตัวอยู่ร่วมกันได้ (ยังใช้ klipper กับ Service อื่นได้)

พฤติกรรมนี้เป็นสัญญาระดับ API ของ Kubernetes เอง ไม่ใช่รายละเอียดเฉพาะของ k3s —
`ServiceSpec.LoadBalancerClass` ระบุว่า *"Any default load balancer implementation
(e.g. cloud providers) should ignore Services that set this field."*

**`--disable=servicelb` ไม่ใช่ทางเลือกแทน** — controller ตัวนี้เช็ค `loadBalancerClass`
ก่อนจะรับผิดชอบ Service ปิด klipper เฉยๆ แล้วไม่ตั้ง class จะได้ Service ที่ไม่มีใครดูแล
ค้าง `<pending>` ตลอด การตั้ง class จำเป็นเสมอ ส่วน `--disable=servicelb` เป็นแค่
มาตรการเสริมถ้าอยากปิด klipper ทั้งระบบ
(อย่าใช้ `--disable-cloud-controller` — จะเสีย node metadata handling ของ k3s ไปด้วย)

#### ผลพวงที่ต้องออกแบบขั้นตอน migration รองรับ

field เดียวกันนั้นระบุด้วยว่า **"Once set, it can not be changed"** และ validation
ถือว่า `nil → value` ก็คือการเปลี่ยน แปลว่า **เติม class ลง Service ที่เป็น type
LoadBalancer อยู่แล้วไม่ได้**

คลัสเตอร์ที่มี Traefik รันอยู่ก่อนจึงต้องพา Service ผ่านสถานะ non-LoadBalancer
หนึ่งจังหวะ (`ClusterIP` → กลับมา `LoadBalancer` พร้อม class) ซึ่งดีกว่าการ
`delete svc` ตรงที่ ClusterIP กับ DNS ไม่หาย เสียแค่ nodePort ที่ถูก re-allocate

และลำดับกับ HelmChartConfig สำคัญ: **patch ก่อน แล้วค่อย apply HelmChartConfig**
ถ้า apply ก่อน helm-controller จะพยายาม patch class ลง Service เดิม โดน apiserver
ปฏิเสธ แล้ว helm job ค้างในสถานะ failed

### 10.2 Traefik ใน K3s ถูก deploy ผ่าน HelmChart → แก้ผ่าน HelmChartConfig

```yaml
apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: traefik
  namespace: kube-system
spec:
  valuesContent: |-
    service:
      spec:
        loadBalancerClass: clb.tencentcloud.com/external
        externalTrafficPolicy: Local
      annotations:
        clb.tencentcloud.com/internet-charge-type: TRAFFIC_POSTPAID_BY_HOUR
        clb.tencentcloud.com/internet-max-bandwidth-out: "100"
```

### 10.3 RBAC (ขั้นต่ำจริงๆ)

```yaml
rules:
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get","list","watch","update","patch"]
  - apiGroups: [""]
    resources: ["services/status"]
    verbs: ["update","patch"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get","list","watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get","list","watch"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create","patch"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get","create","update"]     # leader election
```

เทียบกับ CCM ที่ต้องการ `nodes/status update`, `patch nodes`, `delete nodes` — **เราไม่ต้องการเลย**

### 10.4 Credentials & CAM

Secret → env `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY` / `TENCENTCLOUD_REGION`
CAM policy จำกัดเฉพาะ 12 action ใน §8 (policy JSON เต็มอยู่ใน README)

การตั้งชื่อ: policy ตัวเดียวใช้ร่วมทุกคลัสเตอร์ (`K3sTencentCLBController`) แต่
**sub-user แยกต่อคลัสเตอร์** (`k3s-clb-controller-<cluster-id>`) เพื่อให้เพิกถอนคีย์
ของคลัสเตอร์เดียวได้โดยไม่กระทบตัวอื่น และไล่ CloudAudit ย้อนได้ว่าใครแตะ CLB
เปิดสิทธิ์แบบ programmatic access อย่างเดียว — controller ไม่เคยล็อกอิน console

การจำกัด resource ด้วย condition บน tag `k8s-cluster-id` ทำได้เป็นชั้นเสริม แต่
`CreateLoadBalancer` ยังต้องเป็น `*` อยู่ดีเพราะตอนตรวจสิทธิ์ resource ยังไม่เกิด

ยังไม่รองรับ instance role provider (รันบน CVM ที่ผูก role แล้วไม่ต้องเก็บ key) —
เป็นงานที่ควรทำต่อ เพราะตัดปัญหาการหมุนคีย์ทิ้งได้ทั้งหมด

**ข้อจำกัดปัจจุบัน:** controller ไม่ได้ watch secret — หมุนคีย์แล้วต้อง
`rollout restart` deployment เอง

### 10.5 Pod

- `replicas: 2` + leader election (lease) — standby ไม่ทำงาน แต่ failover เร็ว
- `/healthz` `/readyz` `:8080/metrics`
- tolerate `node-role.kubernetes.io/control-plane`, `priorityClassName: system-cluster-critical`

---

## 11. Failure modes ที่ต้องออกแบบรับไว้

| สถานการณ์ | พฤติกรรมที่ถูกต้อง |
|---|---|
| สร้าง CLB สำเร็จ แต่ controller crash ก่อนเขียน annotation | resync หา CLB จาก **tag** เจอ → adopt ไม่สร้างซ้ำ |
| คนไปลบ listener บน console | resync period สร้างคืน + Event `DriftCorrected` |
| Node ถูก drain | EndpointSlice เปลี่ยน → deregister ภายในไม่กี่วินาที; healthCheckNodePort เป็นเบาะรอง |
| Secret หมดอายุ / `UnauthorizedOperation` | ไม่ retry รัว, Event + metric `clb_auth_errors_total`, LB เดิมยังทำงานปกติ |
| Service ถูกลบระหว่าง CLB มี task ค้าง | finalizer กันไว้, poll task จบก่อนค่อยลบ, ลบ CLB ไม่สำเร็จ = ไม่ปลด finalizer |
| CLB เป็น prepaid (รายเดือน) | ลบผ่าน API ไม่ได้ → Event `ManualCleanupRequired` + ปลด finalizer หลัง N ครั้ง (ไม่ให้ Service ค้าง Terminating ตลอดกาล) |
| CLB quota เต็ม | backoff ยาว + Event ชัดเจน ไม่ hot loop เผา API quota |

---

## 12. Repo layout

```
cmd/controller/main.go            # manager setup, flags, leader election
internal/controller/
    service_controller.go         # Reconcile
    listeners.go                  # diff listeners
    targets.go                    # diff targets
    finalizer.go
internal/clb/
    client.go                     # wrapper + rate limit + retry classification
    task.go                       # DescribeTaskStatus polling
    fake.go                       # in-memory fake สำหรับ envtest
internal/node/
    resolver.go                   # node → instanceId + cache
internal/config/                  # annotations parsing, validation
deploy/
    manifests/ | chart/
```

**Dependencies หลัก**
- `sigs.k8s.io/controller-runtime` (เวอร์ชันที่ตรงกับ client-go ของ k8s 1.36)
- `github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/clb/v20180317`
- `github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312`

ไม่มี `k8s.io/cloud-provider`, ไม่มี `k8s.io/kubernetes` — นี่คือเหตุผลที่มันเบา

---

## 13. Testing

1. **Unit** — diff logic (listeners/targets) เป็น pure function รับ desired+actual คืน operation list
   ทดสอบง่ายและครอบคลุมเคสเยอะที่สุด ควรลงแรงตรงนี้มากที่สุด
2. **envtest** — Reconciler + `internal/clb.Fake` ครอบ lifecycle: create → adopt → drift → delete
   รวมถึงเคส "crash ก่อนเขียน annotation"
3. **E2E (manual, 1 คลัสเตอร์จริง)** — checklist: สร้าง, rolling restart Traefik แล้ว 0 downtime,
   drain node, ลบ Service แล้ว CLB หายจริง, ลบ listener บน console แล้วคืนสภาพ

---

## 14. ลำดับการทำ

1. `internal/clb` + task polling + fake  ← ทำก่อน ทุกอย่างพึ่งชั้นนี้
2. node resolver + cache
3. Reconciler: create/adopt/status + finalizer (ยังไม่ต้องมี drift correction)
4. listener/target diff + EndpointSlice watch + `Local` policy
5. annotations ครบชุด + validation
6. orphan GC + metrics + leader election
7. Helm chart

ข้อ 1–3 ก็ใช้งานจริงกับ Traefik ได้แล้ว
