package config

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/drsaluml/k3s-tencent-clb-controller/internal/clb"
)

// Prefix ของ annotation ทุกตัวที่ controller นี้อ่าน
const Prefix = "clb.tencentcloud.com/"

const (
	// AnnoLoadBalancerID controller เขียนเอง — id ของ CLB ที่สร้างให้ Service นี้
	AnnoLoadBalancerID = Prefix + "loadbalancer-id"
	// AnnoExistingLoadBalancerID user เขียนเอง — adopt CLB ที่มีอยู่ ไม่ลบตอน Service หาย
	AnnoExistingLoadBalancerID = Prefix + "existing-loadbalancer-id"

	// immutable หลังสร้าง CLB แล้ว
	AnnoSubnetID         = Prefix + "subnet-id"
	AnnoAddressIPVersion = Prefix + "address-ip-version"
	AnnoSLAType          = Prefix + "sla-type"

	// แก้ได้ระหว่างทาง
	AnnoChargeType        = Prefix + "internet-charge-type"
	AnnoBandwidthOut      = Prefix + "internet-max-bandwidth-out"
	AnnoScheduler         = Prefix + "scheduler"
	AnnoSessionExpireTime = Prefix + "session-expire-time"

	// AnnoDeleteProtection เปิด "删除保护" ของ CLB — กันคนลบพลาดจาก console
	// ครอบเฉพาะการลบ ไม่ได้ครอบการแก้ไข การกันแก้ไขต้องใช้ CAM deny policy
	// (deploy/cam/deny-console-edit.json)
	AnnoDeleteProtection = Prefix + "delete-protection"

	// AnnoSecurityGroups ผูก SG กับตัว CLB เอง — คั่นด้วย comma เช่น "sg-aaa,sg-bbb"
	// รูปแบบค่าเหมือน service.cloud.tencent.com/security-groups ของ TKE
	// ค่าว่าง ("") = ถอด SG ออกทั้งหมด ส่วน "ไม่ใส่ annotation" = ไม่เข้าไปยุ่งเลย
	AnnoSecurityGroups = Prefix + "security-groups"
	// AnnoPassToTarget เปิด "放通" — CLB ส่ง traffic ถึง backend ได้โดยไม่ต้องผ่าน
	// SG ของ node ตรวจแค่ SG ของ CLB ชั้นเดียว
	// รูปแบบค่าเหมือน service.cloud.tencent.com/pass-to-target ของ TKE
	AnnoPassToTarget = Prefix + "pass-to-target"

	AnnoHealthCheckProtocol = Prefix + "health-check-protocol"
	AnnoHealthCheckPath     = Prefix + "health-check-path"
	AnnoHealthCheckDomain   = Prefix + "health-check-domain"
	AnnoHealthCheckInterval = Prefix + "health-check-interval"
	AnnoHealthCheckTimeout  = Prefix + "health-check-timeout"
)

// tag key ที่เขียนลงบน CLB — เป็น source of truth ของ ownership
const (
	TagClusterID = "k8s-cluster-id"
	TagService   = "k8s-service"
	TagManagedBy = "k8s-managed-by"

	ManagedByValue = "k3s-tencent-clb-controller"
)

// Finalizer กัน Service ถูกลบก่อนที่ CLB จะถูกเก็บกวาด
const Finalizer = Prefix + "resources"

// LBSpec คือทุกอย่างที่แปลงมาจาก Service แล้ว พร้อมส่งให้ชั้น clb
type LBSpec struct {
	Create    clb.CreateSpec
	Listeners []clb.ListenerSpec

	// ExistingID != "" แปลว่า user เอา CLB มาให้ใช้ ห้ามลบตอน Service หาย
	ExistingID string

	// DeleteProtect เปิด delete protection บน CLB
	DeleteProtect bool

	// SecurityGroups เป็น tri-state โดยตั้งใจ
	//   nil            = ไม่มี annotation → ไม่แตะ SG ที่ผูกอยู่
	//   []string{}     = annotation ค่าว่าง → ถอด SG ออกทั้งหมด
	//   [...]          = ผูกตามรายการนี้
	// ที่ต้องแยก nil ออกจาก empty เพราะ CLB ที่ adopt มาอาจมี SG ที่คนตั้งไว้เอง
	// ถ้าเหมารวมว่า "ไม่มี annotation = ไม่ต้องมี SG" controller จะถอดทิ้งเงียบๆ
	SecurityGroups []string
	// PassToTarget เป็น nil เมื่อไม่มี annotation — เหตุผลเดียวกับ SecurityGroups
	// การเผลอตั้งเป็น false บน CLB ที่เปิดไว้อยู่จะทำให้ traffic ตายทันที
	// เพราะ SG ของ node กลับมาถูกตรวจอีกครั้ง
	PassToTarget *bool
}

// Adopted บอกว่า CLB ตัวนี้เป็นของ user ไม่ใช่ของเราสร้าง
func (s LBSpec) Adopted() bool { return s.ExistingID != "" }

// Parse แปลง Service → LBSpec
//
// คืน error เมื่อ annotation ผิดรูป — เราอยากให้ reconcile ล้มพร้อม Event ที่อ่านรู้เรื่อง
// ดีกว่าไปสร้าง CLB ด้วยค่าที่เดาเอง
func Parse(svc *corev1.Service, cfg *Config) (LBSpec, error) {
	var spec LBSpec
	a := svc.Annotations

	lbType := clb.LBTypeOpen
	if svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass == cfg.ClassInternal {
		lbType = clb.LBTypeInternal
	}

	subnet := get(a, AnnoSubnetID, cfg.DefaultSubnetID)
	if lbType == clb.LBTypeInternal && subnet == "" {
		return spec, fmt.Errorf("internal load balancer requires %s (or controller --default-subnet-id)", AnnoSubnetID)
	}

	bandwidth, err := getInt64(a, AnnoBandwidthOut, 0)
	if err != nil {
		return spec, err
	}

	spec.ExistingID = a[AnnoExistingLoadBalancerID]
	spec.DeleteProtect = isTrue(a[AnnoDeleteProtection])

	if sgs, ok := a[AnnoSecurityGroups]; ok {
		parsed, err := parseSecurityGroups(sgs)
		if err != nil {
			return spec, err
		}
		spec.SecurityGroups = parsed
	}
	if v, ok := a[AnnoPassToTarget]; ok {
		spec.PassToTarget = boolPtr(isTrue(v))
	}

	spec.Create = clb.CreateSpec{
		Name:             lbName(svc, cfg.ClusterID),
		Type:             lbType,
		VpcID:            cfg.VpcID,
		SubnetID:         subnet,
		AddressIPVersion: a[AnnoAddressIPVersion],
		SLAType:          a[AnnoSLAType],
		ChargeType:       a[AnnoChargeType],
		BandwidthOut:     bandwidth,
		Tags:             OwnershipTags(svc, cfg.ClusterID),
		// UID ของ Service ไม่ซ้ำและไม่เปลี่ยนตลอดอายุ object
		// → ยิง CreateLoadBalancer ซ้ำด้วย token เดิมได้ CLB ตัวเดิม
		ClientToken: string(svc.UID),
	}

	spec.Listeners, err = parseListeners(svc, a)
	if err != nil {
		return spec, err
	}
	return spec, nil
}

// OwnershipTags คือ tag ที่ทำให้เรารู้ว่า CLB ตัวไหนเป็นของ Service ไหน
// ใช้กู้สถานะเมื่อ annotation หาย และใช้ตอน orphan GC
func OwnershipTags(svc *corev1.Service, clusterID string) map[string]string {
	return map[string]string{
		TagClusterID: clusterID,
		TagService:   svc.Namespace + "/" + svc.Name,
		TagManagedBy: ManagedByValue,
	}
}

// lbName ตั้งชื่อให้อ่านออกบน console ว่ามาจากไหน
// CLB จำกัดชื่อไว้ 60 ตัวอักษร
func lbName(svc *corev1.Service, clusterID string) string {
	name := fmt.Sprintf("k8s-%s-%s-%s", clusterID, svc.Namespace, svc.Name)
	if len(name) > 60 {
		name = name[:60]
	}
	return name
}

func parseListeners(svc *corev1.Service, a map[string]string) ([]clb.ListenerSpec, error) {
	scheduler := get(a, AnnoScheduler, clb.SchedulerWRR)

	sessionTime, err := getInt64(a, AnnoSessionExpireTime, 0)
	if err != nil {
		return nil, err
	}
	interval, err := getInt64(a, AnnoHealthCheckInterval, 0)
	if err != nil {
		return nil, err
	}
	timeout, err := getInt64(a, AnnoHealthCheckTimeout, 0)
	if err != nil {
		return nil, err
	}

	// externalTrafficPolicy: Local คือค่าที่แนะนำสำหรับ Traefik
	// kube-proxy จะเปิด healthCheckNodePort ที่ตอบ 200 เฉพาะ node ที่มี pod อยู่จริง
	// ใช้ HTTP health check ชี้ไปที่ port นั้น → CLB ถอน node ที่ไม่มี pod ออกเอง
	// ได้ทั้ง client source IP จริง และไม่ต้องพึ่ง controller ในการ failover
	local := svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal
	hcNodePort := int64(svc.Spec.HealthCheckNodePort)

	out := make([]clb.ListenerSpec, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		proto := string(p.Protocol)
		if proto != clb.ProtocolTCP && proto != clb.ProtocolUDP {
			return nil, fmt.Errorf("port %s: protocol %s is not supported (TCP and UDP only)", p.Name, proto)
		}
		if p.NodePort == 0 {
			// ยังไม่ถูก allocate — reconcile รอบหน้าค่อยว่ากัน ไม่ใช่ error
			return nil, fmt.Errorf("port %s: nodePort not allocated yet", p.Name)
		}

		hc := clb.HealthCheck{
			Enabled:  true,
			Type:     clb.HealthCheckTCP,
			Port:     int64(p.NodePort),
			Interval: interval,
			Timeout:  timeout,
		}
		// UDP ใช้ HTTP health check ไม่ได้ และ healthCheckNodePort มีเฉพาะตอน Local
		if local && proto == clb.ProtocolTCP && hcNodePort > 0 {
			hc.Type = clb.HealthCheckHTTP
			hc.Port = hcNodePort
		}
		// ให้ annotation override ได้ แต่เฉพาะกรณีที่ user รู้ว่าทำอะไรอยู่
		if v := a[AnnoHealthCheckProtocol]; v != "" {
			hc.Type = strings.ToUpper(v)
		}
		if hc.Type == clb.HealthCheckHTTP {
			hc.HTTPPath = get(a, AnnoHealthCheckPath, "/healthz")
			hc.HTTPMethod = "GET"
			// CLB ปฏิเสธ HTTP health check ที่ไม่มี domain ("can't be None")
			// kube-proxy ตอบ healthCheckNodePort โดยไม่สนใจ Host header
			// ค่านี้จึงเป็นแค่ป้ายชื่อ — ตั้งเป็นชื่อ Service ให้อ่านออกบน console
			hc.HTTPDomain = get(a, AnnoHealthCheckDomain, svc.Name+"."+svc.Namespace)
		}

		out = append(out, clb.ListenerSpec{
			Name:        listenerName(svc, p),
			Protocol:    proto,
			Port:        int64(p.Port),
			Scheduler:   scheduler,
			SessionTime: sessionTime,
			HealthCheck: hc,
			// ตัด connection ทันทีที่ deregister — ไม่งั้น node ที่ drain ไปแล้ว
			// ยังรับ traffic ค้างอยู่จนกว่า connection จะหมดอายุเอง
			DeregisterRT: true,
		})
	}
	return out, nil
}

func listenerName(svc *corev1.Service, p corev1.ServicePort) string {
	if p.Name != "" {
		return fmt.Sprintf("%s-%s-%s", svc.Namespace, svc.Name, p.Name)
	}
	return fmt.Sprintf("%s-%s-%d", svc.Namespace, svc.Name, p.Port)
}

// parseSecurityGroups แปลง "sg-aaa, sg-bbb" เป็น []string
//
// คืน slice ที่ไม่ใช่ nil เสมอเมื่อ annotation มีอยู่ — แม้ค่าจะว่าง
// เพราะ caller ใช้ nil เป็นสัญญาณว่า "ไม่มี annotation" ไม่ใช่ "ไม่มี SG"
//
// id ที่ผิดรูปทำให้ Parse ล้มพร้อม Event ดีกว่าปล่อยผ่านแล้วไปเจอ error
// จาก Tencent ที่อ่านไม่รู้เรื่องตอน reconcile
func parseSecurityGroups(v string) ([]string, error) {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		sg := strings.TrimSpace(part)
		if sg == "" {
			continue
		}
		if !strings.HasPrefix(sg, "sg-") {
			return nil, fmt.Errorf("annotation %s: %q is not a security group id (expected sg-xxxxxxxx)",
				AnnoSecurityGroups, sg)
		}
		out = append(out, sg)
	}
	return out, nil
}

func boolPtr(b bool) *bool { return &b }

// isTrue รับได้ทั้ง "true" และ "1" — ต้องเป็น string ใน YAML อยู่แล้ว
// จึงมักถูกเขียนมาได้หลายแบบ
func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

func get(a map[string]string, key, def string) string {
	if v, ok := a[key]; ok && v != "" {
		return v
	}
	return def
}

func getInt64(a map[string]string, key string, def int64) (int64, error) {
	v, ok := a[key]
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("annotation %s: %q is not a valid integer", key, v)
	}
	return n, nil
}
