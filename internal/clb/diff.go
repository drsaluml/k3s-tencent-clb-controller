package clb

// ไฟล์นี้เป็น pure function ล้วน ไม่แตะ network ไม่แตะ k8s
// ตรรกะ "จะสร้าง/แก้/ลบอะไรบ้าง" อยู่ที่นี่ทั้งหมด เพื่อให้ทดสอบได้เต็มที่

// ListenerPlan คือแผนการเปลี่ยนแปลง listener บน CLB หนึ่งตัว
type ListenerPlan struct {
	Create []ListenerSpec
	// Update จับคู่ listener ที่มีอยู่กับ spec ที่ต้องการ
	Update []ListenerUpdate
	Delete []Listener
	// Unchanged ใช้ต่อในขั้น target sync — listener เหล่านี้ยังต้อง sync backend
	Unchanged []ListenerMatch
}

type ListenerUpdate struct {
	Existing Listener
	Desired  ListenerSpec
}

type ListenerMatch struct {
	Existing Listener
	Desired  ListenerSpec
}

// Empty บอกว่าไม่มีอะไรต้องทำกับ listener เลย
func (p ListenerPlan) Empty() bool {
	return len(p.Create) == 0 && len(p.Update) == 0 && len(p.Delete) == 0
}

// PlanListeners เทียบ desired กับ actual
//
// ownedPorts คือ port ที่ Service เคยเป็นเจ้าของ ใช้ตัดสินใจว่าจะลบ listener
// แปลกปลอมหรือไม่ — ถ้า nil แปลว่าเราเป็นเจ้าของ CLB ทั้งตัว (ลบได้หมด)
// ถ้าไม่ nil แปลว่าเป็น CLB ที่ user เอามาให้ใช้ (BYO) ห้ามแตะ listener ที่ไม่ใช่ของเรา
func PlanListeners(desired []ListenerSpec, actual []Listener, ownedPorts map[ListenerKey]bool) ListenerPlan {
	var plan ListenerPlan

	actualByKey := make(map[ListenerKey]Listener, len(actual))
	for _, l := range actual {
		actualByKey[l.Key()] = l
	}
	desiredByKey := make(map[ListenerKey]ListenerSpec, len(desired))
	for _, s := range desired {
		desiredByKey[s.Key()] = s
	}

	for _, s := range desired {
		existing, ok := actualByKey[s.Key()]
		switch {
		case !ok:
			plan.Create = append(plan.Create, s)
		case listenerNeedsUpdate(existing, s):
			plan.Update = append(plan.Update, ListenerUpdate{Existing: existing, Desired: s})
		default:
			plan.Unchanged = append(plan.Unchanged, ListenerMatch{Existing: existing, Desired: s})
		}
	}

	for _, l := range actual {
		if _, ok := desiredByKey[l.Key()]; ok {
			continue
		}
		// listener ที่ไม่อยู่ใน desired: ลบเฉพาะที่เรารู้ว่าเป็นของเรา
		if ownedPorts != nil && !ownedPorts[l.Key()] {
			continue
		}
		plan.Delete = append(plan.Delete, l)
	}

	return plan
}

// listenerNeedsUpdate เทียบเฉพาะ field ที่แก้ได้ผ่าน ModifyListener
// protocol กับ port แก้ไม่ได้ — ถ้าต่างก็เป็นคนละ listener ไปแล้ว (คนละ Key)
func listenerNeedsUpdate(actual Listener, desired ListenerSpec) bool {
	if desired.Scheduler != "" && actual.Scheduler != desired.Scheduler {
		return true
	}
	if desired.SessionTime != actual.SessionTime {
		return true
	}
	return healthCheckNeedsUpdate(actual.HealthCheck, desired.HealthCheck)
}

func healthCheckNeedsUpdate(actual, desired HealthCheck) bool {
	if actual.Enabled != desired.Enabled {
		return true
	}
	if !desired.Enabled {
		return false
	}
	if actual.Type != desired.Type || actual.Port != desired.Port {
		return true
	}
	if desired.Type == HealthCheckHTTP {
		if actual.HTTPPath != desired.HTTPPath {
			return true
		}
		// HTTPMethod ที่ CLB คืนมาอาจว่างถ้าไม่เคยตั้ง — เทียบเฉพาะเมื่อ desired ระบุไว้
		if desired.HTTPMethod != "" && actual.HTTPMethod != desired.HTTPMethod {
			return true
		}
	}
	// ค่าที่ desired ปล่อยเป็น 0 แปลว่า "ใช้ค่า default ของ CLB" ไม่ถือว่า drift
	if desired.Interval > 0 && actual.Interval != desired.Interval {
		return true
	}
	if desired.Timeout > 0 && actual.Timeout != desired.Timeout {
		return true
	}
	if desired.HealthNum > 0 && actual.HealthNum != desired.HealthNum {
		return true
	}
	if desired.UnhealthNum > 0 && actual.UnhealthNum != desired.UnhealthNum {
		return true
	}
	return false
}

// TargetPlan คือแผนการเปลี่ยน backend ของ listener หนึ่งตัว
type TargetPlan struct {
	Register   []Target
	Deregister []Target
}

func (p TargetPlan) Empty() bool { return len(p.Register) == 0 && len(p.Deregister) == 0 }

// PlanTargets เทียบ target ที่ต้องการกับที่ผูกอยู่จริง
//
// เทียบด้วย (InstanceId, Port) เท่านั้น ไม่รวม weight
// เพราะ re-register เพื่อแก้ weight จะตัด connection ที่วิ่งอยู่ทิ้ง
// ซึ่งแพงเกินไปสำหรับสิ่งที่ controller นี้ไม่ได้ใช้ (weight คงที่เสมอ)
func PlanTargets(desired, actual []Target) TargetPlan {
	var plan TargetPlan

	actualSet := make(map[TargetKey]bool, len(actual))
	for _, t := range actual {
		actualSet[t.Key()] = true
	}
	desiredSet := make(map[TargetKey]bool, len(desired))
	for _, t := range desired {
		desiredSet[t.Key()] = true
	}

	for _, t := range desired {
		if !actualSet[t.Key()] {
			plan.Register = append(plan.Register, t)
		}
	}
	for _, t := range actual {
		if !desiredSet[t.Key()] {
			plan.Deregister = append(plan.Deregister, t)
		}
	}
	return plan
}
