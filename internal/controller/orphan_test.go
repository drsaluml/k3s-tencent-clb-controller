package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/drsaluml/k3s-tencent-clb-controller/internal/clb"
	"github.com/drsaluml/k3s-tencent-clb-controller/internal/config"
)

// errReader จำลอง apiserver ที่อ่านไม่ได้ — GC ต้องไม่ตีความว่า "Service หายแล้ว"
type errReader struct {
	client.Reader
	err error
}

func (e errReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return e.err
}

func newCollector(t *testing.T, fake *clb.Fake, reader client.Reader, del bool) (*OrphanCollector, *time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	c := &OrphanCollector{
		CLB:         fake,
		Config:      &config.Config{ClusterID: testClusterID},
		Reader:      reader,
		Interval:    time.Hour,
		GracePeriod: 30 * time.Minute,
		Delete:      del,
		firstSeen:   map[string]time.Time{},
	}
	c.now = func() time.Time { return now }
	return c, &now
}

// ownedLB สร้าง CLB ใน fake พร้อม tag ครบชุดเหมือนที่ controller สร้างจริง
func ownedLB(t *testing.T, fake *clb.Fake, svcKey string) string {
	t.Helper()
	id, err := fake.Create(context.Background(), clb.CreateSpec{
		Name: "k8s-" + testClusterID,
		Type: clb.LBTypeOpen,
		Tags: map[string]string{
			config.TagManagedBy: config.ManagedByValue,
			config.TagClusterID: testClusterID,
			config.TagService:   svcKey,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// readerWith สร้าง client อ่านอย่างเดียวที่มีแค่ object ที่ระบุ
func readerWith(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	return fakeclient.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

func existingService(t *testing.T, namespace, name string) client.Reader {
	t.Helper()
	return readerWith(t, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	})
}

func TestOrphanGC_KeepsLBWhoseServiceStillExists(t *testing.T) {
	fake := clb.NewFake()
	id := ownedLB(t, fake, "kube-system/traefik")
	c, now := newCollector(t, fake, existingService(t, "kube-system", "traefik"), true)

	for i := 0; i < 3; i++ {
		if err := c.collectOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
	}

	if _, ok := fake.LBs[id]; !ok {
		t.Fatal("a load balancer with a live Service was deleted")
	}
}

// รอบแรกต้องไม่ลบเด็ดขาด แม้จะเป็น orphan จริง — ต้องผ่าน grace period ก่อน
func TestOrphanGC_WaitsForGracePeriod(t *testing.T) {
	fake := clb.NewFake()
	id := ownedLB(t, fake, "gone/service")
	c, now := newCollector(t, fake, readerWith(t), true)

	if err := c.collectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.LBs[id]; !ok {
		t.Fatal("deleted on the very first sighting, grace period was not honoured")
	}

	*now = now.Add(29 * time.Minute)
	if err := c.collectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.LBs[id]; !ok {
		t.Fatal("deleted before the grace period elapsed")
	}

	*now = now.Add(2 * time.Minute)
	if err := c.collectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.LBs[id]; ok {
		t.Fatal("orphan survived past the grace period")
	}
}

// delete protection เปิดเป็นค่าเริ่มต้น orphan แทบทุกตัวจึงติดมาด้วย
// ถ้า GC ไม่ปิดให้ก่อน มันจะลบไม่ได้สักตัวและเงียบไปเฉยๆ
func TestOrphanGC_ClearsDeleteProtectionBeforeDeleting(t *testing.T) {
	fake := clb.NewFake()
	id := ownedLB(t, fake, "gone/service")
	fake.LBs[id].DeleteProtect = true
	c, now := newCollector(t, fake, readerWith(t), true)

	if err := c.collectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	if err := c.collectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, ok := fake.LBs[id]; ok {
		t.Fatal("protected orphan was not deleted; protection was never cleared")
	}
}

// ค่าเริ่มต้นคือรายงานอย่างเดียว — ต้องไม่แตะคลาวด์เลย
func TestOrphanGC_ReportOnlyByDefault(t *testing.T) {
	fake := clb.NewFake()
	id := ownedLB(t, fake, "gone/service")
	c, now := newCollector(t, fake, readerWith(t), false)

	for i := 0; i < 5; i++ {
		if err := c.collectOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
	}

	if _, ok := fake.LBs[id]; !ok {
		t.Fatal("report-only mode deleted a load balancer")
	}
}

// เคสที่อันตรายที่สุด: อ่าน apiserver ไม่ได้ ถ้าตีความว่า Service หายหมด
// GC จะลบ CLB ทิ้งทั้งคลัสเตอร์ในรอบเดียว
func TestOrphanGC_AbortsRoundWhenAPIServerIsUnreadable(t *testing.T) {
	fake := clb.NewFake()
	id := ownedLB(t, fake, "kube-system/traefik")
	c, now := newCollector(t, fake, errReader{err: errors.New("etcd is having a bad day")}, true)

	for i := 0; i < 5; i++ {
		if err := c.collectOnce(context.Background()); err == nil {
			t.Fatal("round should have failed loudly instead of treating the error as 'not found'")
		}
		*now = now.Add(time.Hour)
	}

	if _, ok := fake.LBs[id]; !ok {
		t.Fatal("a load balancer was deleted while the apiserver was unreadable")
	}
}

// Service หายไปชั่วคราวแล้วกลับมา (apply ผิดจังหวะ) ต้องรีเซ็ตนาฬิกา
func TestOrphanGC_ResetsGraceWhenServiceComesBack(t *testing.T) {
	fake := clb.NewFake()
	id := ownedLB(t, fake, "kube-system/traefik")
	c, now := newCollector(t, fake, readerWith(t), true)

	if err := c.collectOnce(context.Background()); err != nil { // เห็นเป็น orphan
		t.Fatal(err)
	}
	c.Reader = existingService(t, "kube-system", "traefik") // Service กลับมา
	*now = now.Add(time.Hour)
	if err := c.collectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Reader = readerWith(t) // หายอีกรอบ
	*now = now.Add(time.Hour)
	if err := c.collectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, ok := fake.LBs[id]; !ok {
		t.Fatal("grace period was not restarted after the Service came back")
	}
}

// CLB ของคลัสเตอร์อื่นที่ใช้ Tencent account เดียวกันต้องไม่ถูกแตะ
func TestOrphanGC_IgnoresOtherClustersLoadBalancers(t *testing.T) {
	fake := clb.NewFake()
	other, err := fake.Create(context.Background(), clb.CreateSpec{
		Name: "k8s-other-kube-system-traefik",
		Type: clb.LBTypeOpen,
		Tags: map[string]string{
			config.TagManagedBy: config.ManagedByValue,
			config.TagClusterID: "some-other-cluster",
			config.TagService:   "kube-system/traefik",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, now := newCollector(t, fake, readerWith(t), true)

	for i := 0; i < 3; i++ {
		if err := c.collectOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
	}

	if _, ok := fake.LBs[other]; !ok {
		t.Fatal("deleted a load balancer belonging to another cluster")
	}
}

// สรุปท้ายรอบคือหลักฐานเดียวว่า GC เดินจริง — โหมดรายงานอย่างเดียวที่ทุกอย่างปกติ
// จะไม่ log อะไรอีกเลย ถ้าบรรทัดนี้หายไปก็แยกไม่ออกจาก ticker ที่ไม่เคยยิง
func TestOrphanGC_LogsSweepSummaryOnEveryRound(t *testing.T) {
	fake := clb.NewFake()
	ownedLB(t, fake, "kube-system/traefik") // มี Service อยู่
	ownedLB(t, fake, "gone/service")        // ไม่มี Service
	c, now := newCollector(t, fake, existingService(t, "kube-system", "traefik"), false)

	var sweeps []map[string]any
	ctx := log.IntoContext(context.Background(), logr.New(recordingSink{
		record: func(msg string, kv []any) {
			if msg != "sweep finished" {
				return
			}
			fields := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				fields[kv[i].(string)] = kv[i+1]
			}
			sweeps = append(sweeps, fields)
		},
	}))

	// รอบแรกยังอยู่ใน grace period รอบสองพ้นแล้ว
	for i := 0; i < 2; i++ {
		if err := c.collectOnce(ctx); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
	}

	if len(sweeps) != 2 {
		t.Fatalf("expected one summary per round, got %d", len(sweeps))
	}
	for i, s := range sweeps {
		if s["owned"] != 2 {
			t.Errorf("round %d: owned = %v, want 2", i, s["owned"])
		}
		if s["watching"] != 1 {
			t.Errorf("round %d: watching = %v, want 1", i, s["watching"])
		}
		if s["deleted"] != 0 {
			t.Errorf("round %d: deleted = %v in report-only mode", i, s["deleted"])
		}
	}
	// grace period ยังไม่ครบในรอบแรก ครบในรอบสอง
	if sweeps[0]["orphaned"] != 0 {
		t.Errorf("round 0: orphaned = %v, want 0 (still inside the grace period)", sweeps[0]["orphaned"])
	}
	if sweeps[1]["orphaned"] != 1 {
		t.Errorf("round 1: orphaned = %v, want 1", sweeps[1]["orphaned"])
	}
}

// recordingSink ดัก log record ไว้ตรวจตรงๆ ไม่ต้องพาร์สข้อความที่พิมพ์ออกมา
type recordingSink struct {
	record func(msg string, kv []any)
}

func (s recordingSink) Init(logr.RuntimeInfo)          {}
func (s recordingSink) Enabled(int) bool               { return true }
func (s recordingSink) WithValues(...any) logr.LogSink { return s }
func (s recordingSink) WithName(string) logr.LogSink   { return s }

func (s recordingSink) Info(_ int, msg string, kv ...any) { s.record(msg, kv) }
func (s recordingSink) Error(error, string, ...any)       {}

// tag ที่อ่านไม่ออกแปลว่ามีคนไปแก้เอง หรือไม่ใช่ของที่เราสร้าง — ห้ามลบ
func TestOrphanGC_SkipsLoadBalancerWithUnusableServiceTag(t *testing.T) {
	fake := clb.NewFake()
	id := ownedLB(t, fake, "no-slash-here")
	c, now := newCollector(t, fake, readerWith(t), true)

	for i := 0; i < 3; i++ {
		if err := c.collectOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Hour)
	}

	if _, ok := fake.LBs[id]; !ok {
		t.Fatal("deleted a load balancer whose service tag could not be parsed")
	}
}
