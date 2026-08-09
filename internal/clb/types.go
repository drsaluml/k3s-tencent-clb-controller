package clb

import "context"

// LoadBalancer คือ CLB instance ที่ normalize มาจาก SDK แล้ว
// controller ไม่ควรเห็น struct ของ tencentcloud-sdk-go โดยตรง
type LoadBalancer struct {
	ID   string
	Name string
	Type string // OPEN | INTERNAL
	VIPs []string
	// Domain มีค่าเมื่อภูมิภาคนั้นให้ CLB เป็นแบบ DNS แทนที่จะเป็น IP
	// (เช่น ap-bangkok) — กรณีนี้ VIPs จะว่างและต้องใช้ Domain แทน
	Domain string
	Status uint64 // 0 = กำลังสร้าง, 1 = พร้อมใช้งาน
	Tags   map[string]string
}

// Ready บอกว่า CLB พร้อมรับ listener แล้วหรือยัง
func (l *LoadBalancer) Ready() bool { return l.Status == 1 }

// VIP คืน IP ตัวแรกของ CLB ("" ถ้ายังไม่มี)
func (l *LoadBalancer) VIP() string {
	if len(l.VIPs) == 0 {
		return ""
	}
	return l.VIPs[0]
}

// HasAddress บอกว่า CLB มีที่อยู่ให้ client ใช้แล้วหรือยัง
// ไม่มีทั้ง IP และ domain = ยังเรียกใช้ไม่ได้ ต้อง requeue ไม่ใช่ประกาศว่าเสร็จ
func (l *LoadBalancer) HasAddress() bool {
	return l.VIP() != "" || l.Domain != ""
}

// CreateSpec คือทุกอย่างที่ต้องรู้ตอนสร้าง CLB
// field เหล่านี้ immutable หลังสร้างแล้ว — เปลี่ยนได้ต้องลบสร้างใหม่เท่านั้น
type CreateSpec struct {
	Name             string
	Type             string // OPEN | INTERNAL
	VpcID            string
	SubnetID         string // จำเป็นเมื่อ Type == INTERNAL
	AddressIPVersion string
	SLAType          string
	ChargeType       string
	BandwidthOut     int64
	Tags             map[string]string

	// ClientToken ทำให้ CreateLoadBalancer idempotent ฝั่ง Tencent
	// ยิงซ้ำด้วย token เดิมจะได้ CLB ตัวเดิม ไม่สร้างใหม่
	// เราใช้ Service UID → กัน CLB ผีเวลา controller crash หลังสร้างสำเร็จ
	// แต่ก่อนเขียน annotation กลับ
	ClientToken string
}

// ListenerSpec คือ listener หนึ่งตัวที่ map มาจาก ServicePort หนึ่ง port
type ListenerSpec struct {
	Name         string
	Protocol     string // TCP | UDP
	Port         int64  // port ที่ CLB เปิดรับ = service port
	Scheduler    string
	SessionTime  int64
	HealthCheck  HealthCheck
	DeregisterRT bool // ตัด connection ทันทีเมื่อ deregister target
}

// Key คือ identity ของ listener ที่ใช้เทียบ desired กับ actual
// CLB ไม่ให้มี listener ซ้ำ protocol+port บนตัวเดียวกัน จึงใช้คู่นี้เป็น key ได้
type ListenerKey struct {
	Protocol string
	Port     int64
}

func (s ListenerSpec) Key() ListenerKey { return ListenerKey{s.Protocol, s.Port} }

// Listener คือ listener ที่มีอยู่จริงบน CLB
type Listener struct {
	ID          string
	Name        string
	Protocol    string
	Port        int64
	Scheduler   string
	SessionTime int64
	HealthCheck HealthCheck
}

func (l Listener) Key() ListenerKey { return ListenerKey{l.Protocol, l.Port} }

// HealthCheck ของ listener
// CheckType TCP = เช็ค nodePort ตรงๆ (externalTrafficPolicy: Cluster)
// CheckType HTTP = เช็ค healthCheckNodePort ที่ kube-proxy เปิด (policy: Local)
type HealthCheck struct {
	Enabled  bool
	Type     string // TCP | HTTP
	Port     int64
	HTTPPath string
	// HTTPDomain ไปเป็น Host header ของ health check
	// CLB บังคับให้มีค่าเมื่อ CheckType เป็น HTTP บน listener แบบ TCP
	// (ตอบกลับ "HealthCheck.HttpCheckDomain can't be None" ถ้าไม่ส่ง)
	// kube-proxy ตอบ healthCheckNodePort โดยไม่สนใจ Host ค่านี้จึงเป็นแค่ป้ายชื่อ
	HTTPDomain  string
	HTTPMethod  string
	Interval    int64
	Timeout     int64
	HealthNum   int64
	UnhealthNum int64
}

// Target คือ backend หนึ่งตัว (CVM instance + nodePort)
type Target struct {
	InstanceID string
	Port       int64
	Weight     int64
}

// Key ใช้เทียบ target โดยไม่สน weight — weight เปลี่ยนไม่ต้อง re-register
type TargetKey struct {
	InstanceID string
	Port       int64
}

func (t Target) Key() TargetKey { return TargetKey{t.InstanceID, t.Port} }

// Interface คือสัญญาที่ controller ใช้ ทำให้ทดสอบด้วย Fake ได้โดยไม่แตะ cloud จริง
// ทุก method ต้อง idempotent และรอ async task ให้เสร็จก่อน return
type Interface interface {
	// FindByTags หา CLB จาก tag — เป็น source of truth ของ ownership
	FindByTags(ctx context.Context, tags map[string]string) ([]LoadBalancer, error)
	// Get คืน nil, nil ถ้าไม่พบ (ไม่ใช่ error) เพราะ "ถูกลบไปแล้ว" คือสถานะปกติ
	Get(ctx context.Context, id string) (*LoadBalancer, error)
	Create(ctx context.Context, spec CreateSpec) (string, error)
	Delete(ctx context.Context, id string) error

	ListListeners(ctx context.Context, lbID string) ([]Listener, error)
	CreateListener(ctx context.Context, lbID string, spec ListenerSpec) (string, error)
	UpdateListener(ctx context.Context, lbID, listenerID string, spec ListenerSpec) error
	DeleteListener(ctx context.Context, lbID, listenerID string) error

	ListTargets(ctx context.Context, lbID string, listenerIDs []string) (map[string][]Target, error)
	RegisterTargets(ctx context.Context, lbID, listenerID string, targets []Target) error
	DeregisterTargets(ctx context.Context, lbID, listenerID string, targets []Target) error
}
