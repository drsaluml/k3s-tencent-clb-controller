package config

import (
	"fmt"
	"time"
)

// Config คือค่าระดับ controller ที่มาจาก flag/env ไม่ใช่จาก Service
type Config struct {
	// ClusterID แยกว่า CLB ตัวไหนเป็นของคลัสเตอร์ไหน
	// จำเป็นเมื่อมีหลายคลัสเตอร์ใช้ Tencent account เดียวกัน
	// ตั้งผิด = คลัสเตอร์หนึ่งไปลบ CLB ของอีกคลัสเตอร์ตอน orphan GC
	ClusterID string

	Region string
	VpcID  string

	// DefaultSubnetID ใช้เมื่อ Service ขอ internal LB แต่ไม่ระบุ subnet เอง
	DefaultSubnetID string

	// ClassExternal/ClassInternal คือค่า spec.loadBalancerClass ที่ controller นี้รับผิดชอบ
	// Service ที่ไม่ตรงสองค่านี้จะถูกข้ามทั้งหมด
	ClassExternal string
	ClassInternal string

	// ResyncPeriod คือรอบที่ reconcile ซ้ำเพื่อแก้ drift ที่เกิดนอก k8s
	// (คนไปแก้บน console, listener หาย ฯลฯ)
	ResyncPeriod time.Duration

	// ExcludedNodeLabels: node ที่มี label เหล่านี้จะไม่ถูก register เป็น target
	ExcludedNodeLabels map[string]string
}

const (
	DefaultClassExternal = "clb.tencentcloud.com/external"
	DefaultClassInternal = "clb.tencentcloud.com/internal"
)

func (c *Config) Defaults() {
	if c.ClassExternal == "" {
		c.ClassExternal = DefaultClassExternal
	}
	if c.ClassInternal == "" {
		c.ClassInternal = DefaultClassInternal
	}
	if c.ResyncPeriod == 0 {
		c.ResyncPeriod = 10 * time.Minute
	}
}

func (c *Config) Validate() error {
	if c.ClusterID == "" {
		return fmt.Errorf("--cluster-id is required: it scopes CLB ownership tags and orphan GC")
	}
	if c.Region == "" {
		return fmt.Errorf("--region is required")
	}
	if c.VpcID == "" {
		return fmt.Errorf("--vpc-id is required: CLB must be created in the cluster's VPC")
	}
	return nil
}
