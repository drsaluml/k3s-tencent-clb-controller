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

ยังไม่เคยรันกับ Tencent Cloud API จริง — test ทั้งหมดใช้ in-memory fake
ก่อนขึ้น production ควรลองกับคลัสเตอร์ทดสอบตาม checklist ท้ายไฟล์

## ติดตั้ง

### 1. ปิด ServiceLB (klipper) ไม่ให้แย่ง Service

ใช้ `spec.loadBalancerClass` เป็นวิธีหลัก — ServiceLB ของ K3s จะข้าม Service ที่ตั้ง class ไว้
ทำให้ทั้งสองตัวอยู่ร่วมกันได้ (Service อื่นยังใช้ klipper ได้ตามปกติ)

ถ้าอยากปิดทั้งหมด ใช้ `--disable=servicelb` ตอน start k3s server
อย่าใช้ `--disable-cloud-controller` เพราะจะเสีย node metadata handling ของ k3s ไปด้วย

### 2. Credentials + manifests

```bash
kubectl -n kube-system create secret generic tencentcloud-credentials \
  --from-literal=secret-id=AKID... \
  --from-literal=secret-key=...

# แก้ --cluster-id / --region / --vpc-id ใน deploy/manifests.yaml ก่อน
kubectl apply -f deploy/manifests.yaml
```

`--cluster-id` ต้องไม่ซ้ำกับคลัสเตอร์อื่นที่ใช้ Tencent account เดียวกัน
มันคือสิ่งที่แยกว่า CLB ตัวไหนเป็นของใคร — ตั้งซ้ำแล้วสองคลัสเตอร์จะแย่ง CLB กัน

### 3. ชี้ Traefik มาที่ controller

```bash
kubectl apply -f deploy/traefik-helmchartconfig.yaml
```

## สิทธิ์ CAM ที่ต้องการ

Controller เรียก API แค่เท่านี้ ตั้ง policy ให้แคบที่สุดได้ตามนี้

| Product | Actions |
|---|---|
| `clb` | `DescribeLoadBalancers` `CreateLoadBalancer` `DeleteLoadBalancer` `DescribeListeners` `CreateListener` `ModifyListener` `DeleteListener` `DescribeTargets` `RegisterTargets` `DeregisterTargets` `DescribeTaskStatus` |
| `cvm` | `DescribeInstances` |

`cvm:DescribeInstances` จำเป็นเพราะ `RegisterTargets` รับ `InstanceId` ไม่ใช่ IP
และ K3s ไม่ได้ตั้ง providerID ที่ใช้หา CVM ได้

## Container image

Image ถูก build และ push ขึ้น GHCR อัตโนมัติเมื่อ push git tag ที่ขึ้นต้นด้วย `v`

```bash
git tag v0.1.0
git push origin v0.1.0
```

จะได้ image หลาย tag พร้อมกัน (`linux/amd64` + `linux/arm64`):

```
ghcr.io/drsaluml/k3s-tencent-clb-controller:v0.1.0
ghcr.io/drsaluml/k3s-tencent-clb-controller:0.1
ghcr.io/drsaluml/k3s-tencent-clb-controller:latest
```

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
| `clb.tencentcloud.com/health-check-protocol` | `TCP` / `HTTP` — override ค่าที่ controller เลือกให้ |
| `clb.tencentcloud.com/health-check-path` | ใช้เมื่อ health check เป็น HTTP |
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
