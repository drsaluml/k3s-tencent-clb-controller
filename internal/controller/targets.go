package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/drsaluml/k3s-tencent-clb-controller/internal/clb"
)

// eligibleNodes คืน node ที่ควรถูก register เป็น backend ของ Service นี้
//
// externalTrafficPolicy: Cluster → ทุก node ที่ใช้งานได้ (kube-proxy forward ต่อเอง)
// externalTrafficPolicy: Local   → เฉพาะ node ที่มี ready endpoint อยู่จริง
//
//	ทำแบบนี้ทำให้ failover เร็วกว่ารอ health check timeout
//	และ health check บน healthCheckNodePort ยังทำงานเป็นชั้นสำรองอยู่
func (r *ServiceReconciler) eligibleNodes(ctx context.Context, svc *corev1.Service) ([]corev1.Node, error) {
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	usable := make([]corev1.Node, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		n := nodeList.Items[i]
		if r.nodeUsable(&n) {
			usable = append(usable, n)
		}
	}

	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		return usable, nil
	}

	withEndpoints, err := r.nodesWithReadyEndpoints(ctx, svc)
	if err != nil {
		return nil, err
	}
	local := make([]corev1.Node, 0, len(usable))
	for _, n := range usable {
		if withEndpoints[n.Name] {
			local = append(local, n)
		}
	}
	return local, nil
}

// nodeUsable กรอง node ที่ไม่ควรรับ traffic ออก
func (r *ServiceReconciler) nodeUsable(n *corev1.Node) bool {
	if n.DeletionTimestamp != nil {
		return false
	}
	if n.Spec.Unschedulable {
		return false
	}
	for k, v := range r.Config.ExcludedNodeLabels {
		if got, ok := n.Labels[k]; ok && got == v {
			return false
		}
	}
	// node ที่กำลังจะถูกลบหรือ shutdown อยู่ ไม่ควรอยู่ใน target pool
	for _, t := range n.Spec.Taints {
		switch t.Key {
		case corev1.TaintNodeNotReady, // ยังไม่พร้อม
			"node.kubernetes.io/unschedulable",
			"node.cloudprovider.kubernetes.io/shutdown",
			"ToBeDeletedByClusterAutoscaler":
			return false
		}
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// nodesWithReadyEndpoints อ่านจาก EndpointSlice ว่า pod ของ Service นี้อยู่บน node ไหนบ้าง
func (r *ServiceReconciler) nodesWithReadyEndpoints(ctx context.Context, svc *corev1.Service) (map[string]bool, error) {
	var slices discoveryv1.EndpointSliceList
	err := r.List(ctx, &slices,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name},
	)
	if err != nil {
		return nil, fmt.Errorf("listing endpointslices for %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	out := map[string]bool{}
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			if ep.NodeName == nil || *ep.NodeName == "" {
				continue
			}
			// Ready เป็น nil แปลว่า "ถือว่า ready" ตาม spec ของ EndpointSlice
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			// terminating endpoint ไม่ควรรับ traffic ใหม่
			if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
				continue
			}
			out[*ep.NodeName] = true
		}
	}
	return out, nil
}

// buildTargets แปลง node → CLB target สำหรับ port หนึ่ง
//
// node ที่ resolve เป็น CVM ไม่ได้จะถูกข้ามพร้อม log ไม่ล้มทั้ง reconcile —
// node ตัวเดียวที่มีปัญหาไม่ควรทำให้ LB ทั้งตัวหยุด sync
func (r *ServiceReconciler) buildTargets(ctx context.Context, nodes []corev1.Node, nodePort int64) ([]clb.Target, int) {
	logger := log.FromContext(ctx)

	targets := make([]clb.Target, 0, len(nodes))
	unresolved := 0
	for i := range nodes {
		n := &nodes[i]
		id, err := r.Nodes.InstanceID(ctx, n)
		if err != nil {
			unresolved++
			logger.V(1).Info("skipping node: cannot resolve CVM instance", "node", n.Name, "err", err)
			continue
		}
		targets = append(targets, clb.Target{
			InstanceID: id,
			Port:       nodePort,
			Weight:     clb.DefaultWeight,
		})
	}
	return targets, unresolved
}
