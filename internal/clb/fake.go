package clb

import (
	"context"
	"fmt"
	"maps"
	"sync"
)

// Fake คือ CLB จำลองในหน่วยความจำ ใช้เทสต์ reconciler โดยไม่แตะ cloud จริง
//
// จำลองพฤติกรรมที่สำคัญไว้ด้วย: ClientToken ทำให้ Create ซ้ำได้ CLB ตัวเดิม
// ซึ่งเป็นกลไกที่ป้องกัน CLB ผีตอน controller crash
type Fake struct {
	mu sync.Mutex

	LBs       map[string]*LoadBalancer
	Listeners map[string][]Listener          // lbID → listeners
	Targets   map[string]map[string][]Target // lbID → listenerID → targets

	// tokens จำ ClientToken → lbID เพื่อจำลอง idempotency ฝั่ง Tencent
	tokens map[string]string

	// CreateCalls นับจำนวนครั้งที่ "สร้างจริง" ใช้ยืนยันว่าไม่สร้างซ้ำ
	CreateCalls int

	// FailNext ทำให้ call ถัดไปของ method ที่ระบุ error ออกมา
	FailNext map[string]error

	nextID int
}

func NewFake() *Fake {
	return &Fake{
		LBs:       map[string]*LoadBalancer{},
		Listeners: map[string][]Listener{},
		Targets:   map[string]map[string][]Target{},
		tokens:    map[string]string{},
		FailNext:  map[string]error{},
	}
}

func (f *Fake) fail(method string) error {
	if err, ok := f.FailNext[method]; ok {
		delete(f.FailNext, method)
		return err
	}
	return nil
}

func (f *Fake) FindByTags(_ context.Context, tags map[string]string) ([]LoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("FindByTags"); err != nil {
		return nil, err
	}

	var out []LoadBalancer
	for _, lb := range f.LBs {
		match := true
		for k, v := range tags {
			if lb.Tags[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, *lb)
		}
	}
	return out, nil
}

func (f *Fake) Get(_ context.Context, id string) (*LoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Get"); err != nil {
		return nil, err
	}
	lb, ok := f.LBs[id]
	if !ok {
		return nil, nil
	}
	cp := *lb
	cp.Tags = maps.Clone(lb.Tags)
	return &cp, nil
}

func (f *Fake) Create(_ context.Context, spec CreateSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Create"); err != nil {
		return "", err
	}

	// ClientToken เดิม → CLB ตัวเดิม ไม่สร้างใหม่ (เหมือน Tencent จริง)
	if spec.ClientToken != "" {
		if id, ok := f.tokens[spec.ClientToken]; ok {
			return id, nil
		}
	}

	f.nextID++
	f.CreateCalls++
	id := fmt.Sprintf("lb-fake%04d", f.nextID)
	f.LBs[id] = &LoadBalancer{
		ID:     id,
		Name:   spec.Name,
		Type:   spec.Type,
		VIPs:   []string{fmt.Sprintf("203.0.113.%d", f.nextID)},
		Status: 1,
		Tags:   maps.Clone(spec.Tags),
	}
	if spec.ClientToken != "" {
		f.tokens[spec.ClientToken] = id
	}
	return id, nil
}

func (f *Fake) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Delete"); err != nil {
		return err
	}
	delete(f.LBs, id)
	delete(f.Listeners, id)
	delete(f.Targets, id)
	return nil
}

func (f *Fake) ListListeners(_ context.Context, lbID string) ([]Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListListeners"); err != nil {
		return nil, err
	}
	return append([]Listener(nil), f.Listeners[lbID]...), nil
}

func (f *Fake) CreateListener(_ context.Context, lbID string, spec ListenerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateListener"); err != nil {
		return "", err
	}
	if _, ok := f.LBs[lbID]; !ok {
		return "", fmt.Errorf("load balancer %s not found", lbID)
	}
	f.nextID++
	id := fmt.Sprintf("lbl-fake%04d", f.nextID)
	f.Listeners[lbID] = append(f.Listeners[lbID], Listener{
		ID:          id,
		Name:        spec.Name,
		Protocol:    spec.Protocol,
		Port:        spec.Port,
		Scheduler:   spec.Scheduler,
		SessionTime: spec.SessionTime,
		HealthCheck: spec.HealthCheck,
	})
	return id, nil
}

func (f *Fake) UpdateListener(_ context.Context, lbID, listenerID string, spec ListenerSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("UpdateListener"); err != nil {
		return err
	}
	for i, l := range f.Listeners[lbID] {
		if l.ID != listenerID {
			continue
		}
		l.Name = spec.Name
		l.Scheduler = spec.Scheduler
		l.SessionTime = spec.SessionTime
		l.HealthCheck = spec.HealthCheck
		f.Listeners[lbID][i] = l
		return nil
	}
	return fmt.Errorf("listener %s not found", listenerID)
}

func (f *Fake) DeleteListener(_ context.Context, lbID, listenerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteListener"); err != nil {
		return err
	}
	out := f.Listeners[lbID][:0]
	for _, l := range f.Listeners[lbID] {
		if l.ID != listenerID {
			out = append(out, l)
		}
	}
	f.Listeners[lbID] = out
	if m := f.Targets[lbID]; m != nil {
		delete(m, listenerID)
	}
	return nil
}

func (f *Fake) ListTargets(_ context.Context, lbID string, listenerIDs []string) (map[string][]Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListTargets"); err != nil {
		return nil, err
	}
	out := map[string][]Target{}
	for _, id := range listenerIDs {
		out[id] = append([]Target(nil), f.Targets[lbID][id]...)
	}
	return out, nil
}

func (f *Fake) RegisterTargets(_ context.Context, lbID, listenerID string, targets []Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RegisterTargets"); err != nil {
		return err
	}
	if f.Targets[lbID] == nil {
		f.Targets[lbID] = map[string][]Target{}
	}
	f.Targets[lbID][listenerID] = append(f.Targets[lbID][listenerID], targets...)
	return nil
}

func (f *Fake) DeregisterTargets(_ context.Context, lbID, listenerID string, targets []Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeregisterTargets"); err != nil {
		return err
	}
	remove := map[TargetKey]bool{}
	for _, t := range targets {
		remove[t.Key()] = true
	}
	cur := f.Targets[lbID][listenerID]
	out := cur[:0]
	for _, t := range cur {
		if !remove[t.Key()] {
			out = append(out, t)
		}
	}
	if f.Targets[lbID] != nil {
		f.Targets[lbID][listenerID] = out
	}
	return nil
}

var _ Interface = (*Fake)(nil)
