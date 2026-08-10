package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/drsaluml/k3s-tencent-clb-controller/internal/clb"
	"github.com/drsaluml/k3s-tencent-clb-controller/internal/config"
)

// OrphanCollector ตามเก็บ CLB ที่ Service เจ้าของหายไปแล้ว
//
// เคสที่ทำให้เกิด orphan: ลบ Service ตอน controller ไม่ได้รัน, ลบทั้ง namespace
// พร้อมกันจน finalizer ถูก force ทิ้ง, หรือ DeleteLoadBalancer ล้มเหลวแล้วไม่มีใคร
// กลับมาดู — ทุกเคสจบเหมือนกันคือ CLB ที่ไม่มีใครใช้แต่ยังคิดเงินทุกชั่วโมง
//
// ตัวเก็บกวาดที่ลบ resource ได้เองเป็นของอันตราย จึงตั้งใจให้ **รายงานอย่างเดียว
// เป็นค่าเริ่มต้น** ต้องเปิด --orphan-gc-delete แยกถึงจะลบจริง
type OrphanCollector struct {
	CLB    clb.Interface
	Config *config.Config

	// Reader ต้องอ่านตรงจาก apiserver ไม่ใช่จาก cache ของ manager
	//
	// ถ้าอ่านจาก cache ที่ยัง sync ไม่เสร็จ Service ทุกตัวจะดูเหมือนหายไปหมด
	// แล้วรอบเดียวก็ลบ CLB ทิ้งทั้งคลัสเตอร์ ใช้ mgr.GetAPIReader()
	Reader client.Reader

	// Interval = 0 คือปิดการทำงาน
	Interval time.Duration
	// GracePeriod คือระยะที่ CLB ต้อง "ดูเหมือน orphan" ติดต่อกันก่อนถึงจะถูกลบ
	// กันเคสที่ Service หายไปชั่วคราวจากการ apply ผิดจังหวะแล้วกลับมาเอง
	GracePeriod time.Duration
	// Delete = false คือรายงานอย่างเดียว ไม่แตะอะไรบนคลาวด์
	Delete bool

	// firstSeen จำว่าเห็น CLB ตัวไหนเป็น orphan ครั้งแรกเมื่อไหร่
	// อยู่ในหน่วยความจำล้วน — controller restart แล้วเริ่มนับใหม่
	// ซึ่งเป็นฝั่งที่ปลอดภัยกว่า (ช้าลง ไม่ใช่ลบเร็วขึ้น)
	firstSeen map[string]time.Time

	now func() time.Time
}

// NeedLeaderElection ทำให้รันแค่ pod เดียว — สองตัวลบพร้อมกันจะชนกันที่ async task
func (c *OrphanCollector) NeedLeaderElection() bool { return true }

// Start ให้ manager เรียก หยุดเมื่อ ctx ถูกยกเลิก
func (c *OrphanCollector) Start(ctx context.Context) error {
	if c.Interval <= 0 {
		return nil
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.firstSeen == nil {
		c.firstSeen = map[string]time.Time{}
	}

	logger := log.FromContext(ctx).WithName("orphan-gc")
	logger.Info("starting orphan collector",
		"interval", c.Interval, "gracePeriod", c.GracePeriod, "deleteEnabled", c.Delete)

	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.collectOnce(ctx); err != nil {
				// รอบถัดไปค่อยว่ากัน อย่าให้ manager ตายเพราะ cloud มีปัญหาชั่วคราว
				logger.Error(err, "orphan collection round failed")
			}
		}
	}
}

// collectOnce เดินหนึ่งรอบ
//
// error ใดๆ = ยกเลิกทั้งรอบโดยไม่ลบอะไรเลย ตั้งใจให้ conservative:
// ข้อมูลไม่ครบแปลว่าเราไม่รู้ว่าอะไรเป็น orphan จริง
//
// รอบที่ไม่เจออะไรก็ยัง log สรุปหนึ่งบรรทัด เพราะโหมดรายงานอย่างเดียวที่เงียบสนิท
// แยกไม่ออกจาก "ticker ไม่เคยยิง" — ต้องไปไล่เทียบกับ API เองถึงจะรู้
func (c *OrphanCollector) collectOnce(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("orphan-gc")
	var orphaned, deleted int

	// กรองด้วย managed-by + cluster-id เท่านั้น ไม่ใส่ tag ของ service
	// เพราะเราอยากได้ "ทุกตัวของคลัสเตอร์นี้" มาเทียบกับ Service ที่มีอยู่จริง
	owned, err := c.CLB.FindByTags(ctx, map[string]string{
		config.TagManagedBy: config.ManagedByValue,
		config.TagClusterID: c.Config.ClusterID,
	})
	if err != nil {
		return fmt.Errorf("listing owned load balancers: %w", err)
	}

	alive := make(map[string]bool, len(owned))
	for _, lb := range owned {
		alive[lb.ID] = true

		key := lb.Tags[config.TagService]
		ns, name, ok := splitServiceKey(key)
		if !ok {
			// tag ผิดรูป = ไม่ใช่ของที่เราสร้าง หรือมีคนไปแก้ tag เอง
			// ไม่แตะเด็ดขาด
			logger.Info("skipping load balancer with unusable service tag",
				"id", lb.ID, "tag", key)
			delete(c.firstSeen, lb.ID)
			continue
		}

		var svc corev1.Service
		err := c.Reader.Get(ctx, serviceKey(ns, name), &svc)
		switch {
		case err == nil:
			// Service ยังอยู่ — ไม่ใช่ orphan ล้างประวัติทิ้ง
			delete(c.firstSeen, lb.ID)
			continue
		case !apierrors.IsNotFound(err):
			// อ่าน apiserver ไม่ได้ = เราไม่รู้ความจริง ห้ามลบอะไรทั้งรอบ
			return fmt.Errorf("reading service %s: %w", key, err)
		}

		first, seen := c.firstSeen[lb.ID]
		if !seen {
			c.firstSeen[lb.ID] = c.now()
			logger.Info("load balancer has no matching service, starting grace period",
				"id", lb.ID, "service", key, "gracePeriod", c.GracePeriod)
			continue
		}
		if age := c.now().Sub(first); age < c.GracePeriod {
			continue
		}

		orphaned++
		if !c.Delete {
			logger.Info("orphaned load balancer (reporting only, --orphan-gc-delete is off)",
				"id", lb.ID, "name", lb.Name, "service", key)
			continue
		}
		if err := c.deleteOrphan(ctx, lb); err != nil {
			// ตัวเดียวลบไม่ได้ไม่ควรหยุดตัวอื่น เก็บ firstSeen ไว้ให้ลองรอบหน้า
			logger.Error(err, "deleting orphaned load balancer", "id", lb.ID, "service", key)
			continue
		}
		logger.Info("deleted orphaned load balancer", "id", lb.ID, "name", lb.Name, "service", key)
		deleted++
		delete(c.firstSeen, lb.ID)
	}

	// CLB ที่หายไปจากรายการแล้ว (ถูกลบไปทางอื่น) ไม่ต้องจำต่อ
	for id := range c.firstSeen {
		if !alive[id] {
			delete(c.firstSeen, id)
		}
	}

	// watching = ตัวที่ไม่มี Service อยู่ตอนนี้ (นับรวมทั้งที่ยังไม่ครบ grace period
	// และที่ครบแล้วแต่ยังไม่ถูกลบเพราะ --orphan-gc-delete ปิดอยู่)
	// orphaned = ครบ grace period แล้วในรอบนี้
	logger.Info("sweep finished",
		"owned", len(owned), "watching", len(c.firstSeen),
		"orphaned", orphaned, "deleted", deleted)
	return nil
}

func (c *OrphanCollector) deleteOrphan(ctx context.Context, lb clb.LoadBalancer) error {
	// delete protection เปิดเป็นค่าเริ่มต้น orphan เกือบทุกตัวจึงติดมาด้วย
	if lb.DeleteProtect {
		if err := c.CLB.SetDeleteProtection(ctx, lb.ID, false); err != nil {
			return fmt.Errorf("clearing delete protection: %w", err)
		}
	}
	return c.CLB.Delete(ctx, lb.ID)
}

// splitServiceKey แยก "namespace/name" ออกจาก tag
func splitServiceKey(v string) (namespace, name string, ok bool) {
	ns, n, found := strings.Cut(v, "/")
	if !found || ns == "" || n == "" {
		return "", "", false
	}
	return ns, n, true
}
