package clb

import "testing"

func tcpListener(port int64, checkPort int64) ListenerSpec {
	return ListenerSpec{
		Name:      "web",
		Protocol:  ProtocolTCP,
		Port:      port,
		Scheduler: SchedulerWRR,
		HealthCheck: HealthCheck{
			Enabled: true,
			Type:    HealthCheckTCP,
			Port:    checkPort,
		},
	}
}

func TestPlanListeners_CreatesMissing(t *testing.T) {
	plan := PlanListeners([]ListenerSpec{tcpListener(80, 31000)}, nil, nil)

	if len(plan.Create) != 1 || plan.Create[0].Port != 80 {
		t.Fatalf("expected one listener to be created, got %+v", plan.Create)
	}
	if len(plan.Delete) != 0 || len(plan.Update) != 0 {
		t.Fatalf("expected nothing else to change, got %+v", plan)
	}
}

func TestPlanListeners_NoChangeWhenInSync(t *testing.T) {
	desired := tcpListener(80, 31000)
	actual := Listener{
		ID:          "lbl-1",
		Protocol:    ProtocolTCP,
		Port:        80,
		Scheduler:   SchedulerWRR,
		HealthCheck: desired.HealthCheck,
	}

	plan := PlanListeners([]ListenerSpec{desired}, []Listener{actual}, nil)

	if !plan.Empty() {
		t.Fatalf("expected no changes, got %+v", plan)
	}
	if len(plan.Unchanged) != 1 || plan.Unchanged[0].Existing.ID != "lbl-1" {
		t.Fatalf("unchanged listener must still be reported for target sync, got %+v", plan.Unchanged)
	}
}

// nodePort ที่เปลี่ยนต้องทำให้ health check ถูกแก้ ไม่ใช่ปล่อยชี้ port เดิมที่ตายแล้ว
func TestPlanListeners_UpdatesWhenHealthCheckPortChanges(t *testing.T) {
	actual := Listener{
		ID:          "lbl-1",
		Protocol:    ProtocolTCP,
		Port:        80,
		Scheduler:   SchedulerWRR,
		HealthCheck: HealthCheck{Enabled: true, Type: HealthCheckTCP, Port: 31000},
	}

	plan := PlanListeners([]ListenerSpec{tcpListener(80, 32000)}, []Listener{actual}, nil)

	if len(plan.Update) != 1 {
		t.Fatalf("expected an update, got %+v", plan)
	}
	if plan.Update[0].Desired.HealthCheck.Port != 32000 {
		t.Fatalf("expected health check port 32000, got %d", plan.Update[0].Desired.HealthCheck.Port)
	}
}

// ค่าที่ปล่อยเป็น 0 แปลว่า "ใช้ default ของ CLB" ไม่ใช่ drift
// ถ้าไม่ระวังตรงนี้ controller จะ ModifyListener ทุกรอบ resync ตลอดไป
func TestPlanListeners_ZeroValuesAreNotDrift(t *testing.T) {
	actual := Listener{
		ID:        "lbl-1",
		Protocol:  ProtocolTCP,
		Port:      80,
		Scheduler: SchedulerWRR,
		HealthCheck: HealthCheck{
			Enabled: true, Type: HealthCheckTCP, Port: 31000,
			Interval: 5, Timeout: 2, HealthNum: 3, UnhealthNum: 3, // CLB เติมให้เอง
		},
	}

	plan := PlanListeners([]ListenerSpec{tcpListener(80, 31000)}, []Listener{actual}, nil)

	if !plan.Empty() {
		t.Fatalf("CLB-defaulted values must not count as drift, got %+v", plan)
	}
}

// CLB ที่ user เอามาให้ใช้ (BYO) ห้ามลบ listener ของคนอื่น
func TestPlanListeners_AdoptedLBKeepsForeignListeners(t *testing.T) {
	foreign := Listener{ID: "lbl-other", Protocol: ProtocolTCP, Port: 8080}
	ours := Listener{ID: "lbl-ours", Protocol: ProtocolTCP, Port: 80, Scheduler: SchedulerWRR,
		HealthCheck: HealthCheck{Enabled: true, Type: HealthCheckTCP, Port: 31000}}

	owned := map[ListenerKey]bool{{Protocol: ProtocolTCP, Port: 80}: true}

	// desired ว่าง = Service ไม่มี port นี้แล้ว
	plan := PlanListeners(nil, []Listener{foreign, ours}, owned)

	if len(plan.Delete) != 1 || plan.Delete[0].ID != "lbl-ours" {
		t.Fatalf("only our own listener may be deleted on an adopted LB, got %+v", plan.Delete)
	}
}

func TestPlanListeners_OwnedLBDeletesStrays(t *testing.T) {
	stray := Listener{ID: "lbl-stray", Protocol: ProtocolTCP, Port: 8080}

	// ownedPorts nil = CLB เป็นของเราทั้งตัว
	plan := PlanListeners(nil, []Listener{stray}, nil)

	if len(plan.Delete) != 1 {
		t.Fatalf("stray listener on an owned LB must be removed, got %+v", plan.Delete)
	}
}

func TestPlanTargets(t *testing.T) {
	desired := []Target{
		{InstanceID: "ins-1", Port: 31000, Weight: 10},
		{InstanceID: "ins-2", Port: 31000, Weight: 10},
	}
	actual := []Target{
		{InstanceID: "ins-2", Port: 31000, Weight: 10},
		{InstanceID: "ins-3", Port: 31000, Weight: 10},
	}

	plan := PlanTargets(desired, actual)

	if len(plan.Register) != 1 || plan.Register[0].InstanceID != "ins-1" {
		t.Fatalf("expected ins-1 to be registered, got %+v", plan.Register)
	}
	if len(plan.Deregister) != 1 || plan.Deregister[0].InstanceID != "ins-3" {
		t.Fatalf("expected ins-3 to be deregistered, got %+v", plan.Deregister)
	}
}

// weight ที่ต่างกันต้องไม่ทำให้ re-register — มันตัด connection ที่วิ่งอยู่ทิ้ง
func TestPlanTargets_WeightChangeIsNotChurn(t *testing.T) {
	desired := []Target{{InstanceID: "ins-1", Port: 31000, Weight: 10}}
	actual := []Target{{InstanceID: "ins-1", Port: 31000, Weight: 50}}

	if plan := PlanTargets(desired, actual); !plan.Empty() {
		t.Fatalf("weight-only difference must not cause churn, got %+v", plan)
	}
}
