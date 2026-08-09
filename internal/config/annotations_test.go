package config

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/drsaluml/k3s-tencent-clb-controller/internal/clb"
)

func testConfig() *Config {
	c := &Config{ClusterID: "th1", Region: "ap-bangkok", VpcID: "vpc-test"}
	c.Defaults()
	return c
}

func svcWithPorts(ports ...corev1.ServicePort) *corev1.Service {
	class := DefaultClassExternal
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "traefik"},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: &class,
			Ports:             ports,
		},
	}
}

// DeregisterTargetRst บน UDP ทำให้ Tencent ตอบ
// "Uin does not support reschedule function." แล้ว listener สร้างไม่ได้เลย
// ส่วน CLB ถูกสร้างไปแล้วและคิดเงินต่อ — UDP ไม่มี connection ให้ RST อยู่แล้ว
func TestParse_DeregisterResetOnlyOnTCP(t *testing.T) {
	svc := svcWithPorts(
		corev1.ServicePort{Name: "web", Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
		corev1.ServicePort{Name: "dns", Port: 53, NodePort: 30053, Protocol: corev1.ProtocolUDP},
	)

	spec, err := Parse(svc, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Listeners) != 2 {
		t.Fatalf("expected 2 listeners, got %d", len(spec.Listeners))
	}
	for _, l := range spec.Listeners {
		switch l.Protocol {
		case clb.ProtocolTCP:
			if !l.DeregisterRT {
				t.Error("TCP listener should reset connections when a target is removed")
			}
		case clb.ProtocolUDP:
			if l.DeregisterRT {
				t.Error("UDP listener must not ask for connection reset; Tencent rejects it")
			}
		}
	}
}

// CLB รับ CheckType ได้แค่ CUSTOM บน UDP ซึ่งต้องมี payload เฉพาะแอป
// ส่ง TCP ไปจะโดนปฏิเสธทั้ง listener — ปิด health check ตรงไปตรงมากว่า
func TestParse_UDPHasNoHealthCheck(t *testing.T) {
	svc := svcWithPorts(
		corev1.ServicePort{Name: "web", Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
		corev1.ServicePort{Name: "dns", Port: 53, NodePort: 30053, Protocol: corev1.ProtocolUDP},
	)
	svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	svc.Spec.HealthCheckNodePort = 32000
	// annotation override ต้องไม่ปลุก health check ของ UDP กลับมา
	svc.Annotations = map[string]string{AnnoHealthCheckProtocol: "HTTP"}

	spec, err := Parse(svc, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range spec.Listeners {
		switch l.Protocol {
		case clb.ProtocolUDP:
			if l.HealthCheck.Enabled {
				t.Error("UDP listener must not carry a health check; CLB rejects every type but CUSTOM")
			}
		case clb.ProtocolTCP:
			if !l.HealthCheck.Enabled {
				t.Error("TCP listener lost its health check")
			}
		}
	}
}

func TestParse_RejectsUnsupportedProtocol(t *testing.T) {
	svc := svcWithPorts(corev1.ServicePort{
		Name: "sctp", Port: 80, NodePort: 30080, Protocol: corev1.ProtocolSCTP,
	})
	if _, err := Parse(svc, testConfig()); err == nil {
		t.Fatal("SCTP should be rejected rather than sent to CLB as-is")
	}
}

// nodePort ยังไม่ถูก allocate ไม่ใช่ความผิดของใคร แต่ต้องไม่ไปสร้าง listener ด้วยเลข 0
func TestParse_RejectsUnallocatedNodePort(t *testing.T) {
	svc := svcWithPorts(corev1.ServicePort{
		Name: "web", Port: 80, Protocol: corev1.ProtocolTCP,
	})
	if _, err := Parse(svc, testConfig()); err == nil {
		t.Fatal("a port without a nodePort should not produce a listener")
	}
}

// internal LB ที่ไม่มี subnet จะถูก Tencent ปฏิเสธ — ล้มตั้งแต่ parse ดีกว่า
func TestParse_InternalRequiresSubnet(t *testing.T) {
	cfg := testConfig()
	class := cfg.ClassInternal
	svc := svcWithPorts(corev1.ServicePort{Name: "web", Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP})
	svc.Spec.LoadBalancerClass = &class

	if _, err := Parse(svc, cfg); err == nil {
		t.Fatal("internal load balancer without a subnet should fail to parse")
	}

	cfg.DefaultSubnetID = "subnet-abc"
	spec, err := Parse(svc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Create.Type != clb.LBTypeInternal || spec.Create.SubnetID != "subnet-abc" {
		t.Fatalf("expected an internal LB in subnet-abc, got %+v", spec.Create)
	}
}
