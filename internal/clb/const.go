package clb

// ชนิดของ CLB
const (
	LBTypeOpen     = "OPEN"     // มี public IP
	LBTypeInternal = "INTERNAL" // อยู่ใน VPC เท่านั้น ต้องระบุ SubnetId
)

// protocol ของ listener ที่ controller นี้รองรับ
// จงใจไม่รองรับ HTTP/HTTPS บน CLB — Traefik terminate TLS เองแล้ว
// การทำ L7 สองที่ทำให้ debug ยากและ cert หลุด sync
const (
	ProtocolTCP = "TCP"
	ProtocolUDP = "UDP"
)

// scheduler
const (
	SchedulerWRR       = "WRR"
	SchedulerLeastConn = "LEAST_CONN"
	SchedulerIPHash    = "IP_HASH"
)

// ชนิด health check
const (
	HealthCheckTCP  = "TCP"
	HealthCheckHTTP = "HTTP"
)

// การคิดเงินขาออกของ CLB แบบ OPEN
const (
	ChargeTrafficPostpaidByHour   = "TRAFFIC_POSTPAID_BY_HOUR"
	ChargeBandwidthPostpaidByHour = "BANDWIDTH_POSTPAID_BY_HOUR"
)

// attrDeleteProtect คือชื่อ flag ใน LoadBalancer.AttributeFlags ที่บอกว่า
// delete protection เปิดอยู่ — Tencent ไม่ได้ให้เป็น boolean แยก field
const attrDeleteProtect = "DeleteProtect"

// DefaultWeight คือน้ำหนักมาตรฐานของ target
// controller นี้ไม่ทำ weighted routing — ให้ทุก node เท่ากันหมด
const DefaultWeight int64 = 10
