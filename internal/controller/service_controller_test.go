package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/drsaluml/k3s-tencent-clb-controller/internal/clb"
	"github.com/drsaluml/k3s-tencent-clb-controller/internal/config"
)

const testClusterID = "k3s-hk"

// fakeResolver แปลง node name → instance id แบบตายตัว
type fakeResolver struct{ missing map[string]bool }

func (f *fakeResolver) InstanceID(_ context.Context, n *corev1.Node) (string, error) {
	if f.missing[n.Name] {
		return "", context.Canceled
	}
	return "ins-" + n.Name, nil
}
func (f *fakeResolver) Forget(string) {}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func readyNode(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func traefikService() *corev1.Service {
	class := config.DefaultClassExternal
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "traefik", Namespace: "kube-system", UID: types.UID("uid-traefik-1"),
		},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass:     &class,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
			Ports: []corev1.ServicePort{
				{Name: "web", Port: 80, Protocol: corev1.ProtocolTCP, NodePort: 31000},
				{Name: "websecure", Port: 443, Protocol: corev1.ProtocolTCP, NodePort: 31001},
			},
		},
	}
}

func newHarness(t *testing.T, objs ...client.Object) (*ServiceReconciler, *clb.Fake, client.Client) {
	t.Helper()
	s := testScheme(t)
	c := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}).
		Build()

	fake := clb.NewFake()
	cfg := &config.Config{ClusterID: testClusterID, Region: "ap-hongkong", VpcID: "vpc-test"}
	cfg.Defaults()

	return &ServiceReconciler{
		Client:   c,
		CLB:      fake,
		Nodes:    &fakeResolver{missing: map[string]bool{}},
		Recorder: record.NewFakeRecorder(100),
		Config:   cfg,
	}, fake, c
}

func reconcileOnce(t *testing.T, r *ServiceReconciler, svc *corev1.Service) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getService(t *testing.T, c client.Client, svc *corev1.Service) *corev1.Service {
	t.Helper()
	var out corev1.Service
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(svc), &out); err != nil {
		t.Fatalf("get service: %v", err)
	}
	return &out
}

func TestReconcile_CreatesLoadBalancerForService(t *testing.T) {
	svc := traefikService()
	r, fake, c := newHarness(t, svc, readyNode("node-a", "10.0.0.1"), readyNode("node-b", "10.0.0.2"))

	reconcileOnce(t, r, svc)

	got := getService(t, c, svc)

	lbID := got.Annotations[config.AnnoLoadBalancerID]
	if lbID == "" {
		t.Fatal("load balancer id was not recorded on the service")
	}
	if !containsString(got.Finalizers, config.Finalizer) {
		t.Fatalf("finalizer must be set before any cloud resource exists, got %v", got.Finalizers)
	}

	lb := fake.LBs[lbID]
	if lb == nil {
		t.Fatal("load balancer was not created")
	}
	if lb.Type != clb.LBTypeOpen {
		t.Fatalf("expected an OPEN load balancer, got %s", lb.Type)
	}
	if lb.Tags[config.TagService] != "kube-system/traefik" || lb.Tags[config.TagClusterID] != testClusterID {
		t.Fatalf("ownership tags are wrong: %v", lb.Tags)
	}

	if n := len(fake.Listeners[lbID]); n != 2 {
		t.Fatalf("expected one listener per service port, got %d", n)
	}

	// ทุก listener ต้องมี node ครบทั้งสองตัวผูกอยู่
	for _, l := range fake.Listeners[lbID] {
		targets := fake.Targets[lbID][l.ID]
		if len(targets) != 2 {
			t.Fatalf("listener %d: expected 2 targets, got %d", l.Port, len(targets))
		}
	}

	if len(got.Status.LoadBalancer.Ingress) != 1 || got.Status.LoadBalancer.Ingress[0].IP != lb.VIP() {
		t.Fatalf("service status was not updated with the CLB VIP: %+v", got.Status.LoadBalancer.Ingress)
	}
}

// reconcile ซ้ำต้องไม่สร้างอะไรเพิ่ม — เป็นคุณสมบัติที่ทั้ง controller พึ่งพา
func TestReconcile_IsIdempotent(t *testing.T) {
	svc := traefikService()
	r, fake, c := newHarness(t, svc, readyNode("node-a", "10.0.0.1"))

	reconcileOnce(t, r, svc)
	reconcileOnce(t, r, getService(t, c, svc))
	reconcileOnce(t, r, getService(t, c, svc))

	if fake.CreateCalls != 1 {
		t.Fatalf("expected exactly one CLB to ever be created, got %d", fake.CreateCalls)
	}
	lbID := getService(t, c, svc).Annotations[config.AnnoLoadBalancerID]
	if n := len(fake.Listeners[lbID]); n != 2 {
		t.Fatalf("listeners were duplicated across reconciles: %d", n)
	}
	for _, l := range fake.Listeners[lbID] {
		if n := len(fake.Targets[lbID][l.ID]); n != 1 {
			t.Fatalf("targets were duplicated across reconciles: %d", n)
		}
	}
}

// เคสที่เป็นเหตุผลหลักของการ tag CLB:
// controller สร้าง CLB สำเร็จแล้ว crash ก่อนเขียน annotation
// รอบถัดไปต้อง "เจอของเดิม" ไม่ใช่สร้างใหม่ ไม่งั้นได้ CLB ผีที่คิดเงินไปเรื่อยๆ
func TestReconcile_RecoversOrphanedLBByTagsAfterCrash(t *testing.T) {
	svc := traefikService()
	r, fake, c := newHarness(t, svc, readyNode("node-a", "10.0.0.1"))

	// จำลองสภาพหลัง crash: CLB มีอยู่พร้อม tag แต่ Service ไม่มี annotation
	orphanID, err := fake.Create(context.Background(), clb.CreateSpec{
		Name: "leftover",
		Type: clb.LBTypeOpen,
		Tags: config.OwnershipTags(svc, testClusterID),
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.CreateCalls = 0

	reconcileOnce(t, r, svc)

	if fake.CreateCalls != 0 {
		t.Fatalf("controller created a duplicate CLB instead of adopting the tagged one")
	}
	if got := getService(t, c, svc).Annotations[config.AnnoLoadBalancerID]; got != orphanID {
		t.Fatalf("expected the orphaned CLB %s to be adopted, got %q", orphanID, got)
	}
}

// annotation ชี้ไปยัง CLB ที่ถูกลบไปแล้ว (คนลบมือบน console) ต้องสร้างใหม่ ไม่ใช่ค้าง error
func TestReconcile_RecreatesWhenAnnotatedLBIsGone(t *testing.T) {
	svc := traefikService()
	svc.Annotations = map[string]string{config.AnnoLoadBalancerID: "lb-deleted-by-hand"}
	r, fake, c := newHarness(t, svc, readyNode("node-a", "10.0.0.1"))

	reconcileOnce(t, r, svc)

	got := getService(t, c, svc).Annotations[config.AnnoLoadBalancerID]
	if got == "lb-deleted-by-hand" || got == "" {
		t.Fatalf("expected a freshly created CLB, got %q", got)
	}
	if fake.CreateCalls != 1 {
		t.Fatalf("expected exactly one create, got %d", fake.CreateCalls)
	}
}

// externalTrafficPolicy: Local → register เฉพาะ node ที่มี pod อยู่จริง
func TestReconcile_LocalPolicyOnlyRegistersNodesWithEndpoints(t *testing.T) {
	svc := traefikService()
	svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
	svc.Spec.HealthCheckNodePort = 32500

	ready := true
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "traefik-abc",
			Namespace: "kube-system",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "traefik"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.42.0.5"},
			NodeName:   strPtr("node-a"),
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
	}

	r, fake, c := newHarness(t, svc, readyNode("node-a", "10.0.0.1"), readyNode("node-b", "10.0.0.2"), slice)

	reconcileOnce(t, r, svc)

	lbID := getService(t, c, svc).Annotations[config.AnnoLoadBalancerID]
	for _, l := range fake.Listeners[lbID] {
		targets := fake.Targets[lbID][l.ID]
		if len(targets) != 1 || targets[0].InstanceID != "ins-node-a" {
			t.Fatalf("listener %d: only the node running the pod may be registered, got %+v", l.Port, targets)
		}
		// health check ต้องชี้ที่ healthCheckNodePort ไม่ใช่ nodePort
		// นี่คือสิ่งที่ทำให้ kube-proxy ถอน node ที่ไม่มี pod ออกได้เอง
		if l.HealthCheck.Type != clb.HealthCheckHTTP || l.HealthCheck.Port != 32500 {
			t.Fatalf("listener %d: expected HTTP health check on 32500, got %s/%d",
				l.Port, l.HealthCheck.Type, l.HealthCheck.Port)
		}
		// CLB ปฏิเสธ HTTP health check ที่ไม่มี domain ด้วย
		// "HealthCheck.HttpCheckDomain can't be None" — listener จึงสร้างไม่ได้เลย
		if l.HealthCheck.HTTPDomain == "" || l.HealthCheck.HTTPPath == "" {
			t.Fatalf("listener %d: HTTP health check needs both domain and path, got domain=%q path=%q",
				l.Port, l.HealthCheck.HTTPDomain, l.HealthCheck.HTTPPath)
		}
	}
}

// Service ที่ไม่มี loadBalancerClass เป็นของ klipper (ServiceLB ของ K3s) ห้ามแตะ
func TestReconcile_IgnoresServicesWithoutOurClass(t *testing.T) {
	svc := traefikService()
	svc.Spec.LoadBalancerClass = nil
	r, fake, c := newHarness(t, svc, readyNode("node-a", "10.0.0.1"))

	reconcileOnce(t, r, svc)

	if fake.CreateCalls != 0 {
		t.Fatal("controller must not touch services owned by another load balancer implementation")
	}
	if fs := getService(t, c, svc).Finalizers; len(fs) != 0 {
		t.Fatalf("no finalizer may be added to a service we do not own, got %v", fs)
	}
}

func TestReconcile_DeletesLoadBalancerAndReleasesFinalizer(t *testing.T) {
	svc := traefikService()
	r, fake, c := newHarness(t, svc, readyNode("node-a", "10.0.0.1"))

	reconcileOnce(t, r, svc)
	lbID := getService(t, c, svc).Annotations[config.AnnoLoadBalancerID]

	if err := c.Delete(context.Background(), getService(t, c, svc)); err != nil {
		t.Fatal(err)
	}
	// finalizer ยังอยู่ → object ยังไม่หาย แค่ติด DeletionTimestamp
	reconcileOnce(t, r, svc)

	if _, still := fake.LBs[lbID]; still {
		t.Fatal("CLB was left behind after the service was deleted")
	}
	var out corev1.Service
	err := c.Get(context.Background(), client.ObjectKeyFromObject(svc), &out)
	if err == nil {
		t.Fatalf("service should be gone once the finalizer is released, still have %v", out.Finalizers)
	}
}

// CLB ที่ user เอามาให้ใช้ ห้ามถูกลบตอน Service หาย
func TestReconcile_AdoptedLoadBalancerSurvivesServiceDeletion(t *testing.T) {
	svc := traefikService()
	r, fake, c := newHarness(t, svc, readyNode("node-a", "10.0.0.1"))

	// เตรียม CLB ที่ "มีอยู่ก่อน" แล้วชี้ Service มาที่มัน
	byoID, err := fake.Create(context.Background(), clb.CreateSpec{Name: "user-owned", Type: clb.LBTypeOpen})
	if err != nil {
		t.Fatal(err)
	}
	cur := getService(t, c, svc)
	cur.Annotations = map[string]string{config.AnnoExistingLoadBalancerID: byoID}
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}

	reconcileOnce(t, r, cur)
	if fake.CreateCalls != 1 {
		t.Fatalf("controller must reuse the provided CLB, not create another (creates=%d)", fake.CreateCalls)
	}

	if err := c.Delete(context.Background(), getService(t, c, svc)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, svc)

	if _, ok := fake.LBs[byoID]; !ok {
		t.Fatal("a user-provided CLB must never be deleted by the controller")
	}
	if n := len(fake.Listeners[byoID]); n != 0 {
		t.Fatalf("listeners created by the controller should be cleaned up, %d left", n)
	}
}

func strPtr(s string) *string { return &s }

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
