package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
)

func q(s string) resource.Quantity { return resource.MustParse(s) }

func serverWith(res corev1.ResourceRequirements, storage vtv1.StorageType) *vtv1.VTTestServer {
	return &vtv1.VTTestServer{Spec: vtv1.VTTestServerSpec{Resources: res, Storage: vtv1.StorageSpec{Type: storage}}}
}

func TestServerResourcesDefaults(t *testing.T) {
	cases := []struct {
		name            string
		res             corev1.ResourceRequirements
		storage         vtv1.StorageType
		wantReq, wantLm string // memory, empty means unset
		wantEphemeral   string
	}{
		{"nothing set, ephemeral", corev1.ResourceRequirements{}, vtv1.StorageEphemeral, "768Mi", "1536Mi", "2Gi"},
		{"nothing set, persistent has no ephemeral request", corev1.ResourceRequirements{}, vtv1.StoragePersistent, "768Mi", "1536Mi", ""},
		{"empty storage type counts as ephemeral", corev1.ResourceRequirements{}, "", "768Mi", "1536Mi", "2Gi"},
		{"user values win", corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"memory": q("1Gi"), "ephemeral-storage": q("5Gi")},
			Limits:   corev1.ResourceList{"memory": q("4Gi")}}, vtv1.StorageEphemeral, "1Gi", "4Gi", "5Gi"},
		{"request above the default limit gets no default limit", corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"memory": q("2Gi")}}, vtv1.StoragePersistent, "2Gi", "", ""},
		{"request below the default limit gets the default limit", corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"memory": q("1Gi")}}, vtv1.StoragePersistent, "1Gi", "1536Mi", ""},
		{"limit below the default request lowers the request", corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"memory": q("512Mi")}}, vtv1.StoragePersistent, "512Mi", "512Mi", ""},
		{"limit above the default request keeps the default request", corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"memory": q("4Gi")}}, vtv1.StoragePersistent, "768Mi", "4Gi", ""},
		{"cpu stays as the user set it", corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"cpu": q("500m")}}, vtv1.StoragePersistent, "768Mi", "1536Mi", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := serverResources(serverWith(c.res, c.storage))
			check := func(list corev1.ResourceList, name corev1.ResourceName, want string) {
				v, ok := list[name]
				if want == "" {
					if ok {
						t.Errorf("%s must be unset, got %s", name, v.String())
					}
					return
				}
				if !ok || v.Cmp(q(want)) != 0 {
					t.Errorf("%s: got %v, want %s", name, v.String(), want)
				}
			}
			check(got.Requests, "memory", c.wantReq)
			check(got.Limits, "memory", c.wantLm)
			check(got.Requests, "ephemeral-storage", c.wantEphemeral)
			// A request must never be above its limit.
			if r, ok := got.Requests["memory"]; ok {
				if l, ok := got.Limits["memory"]; ok && r.Cmp(l) > 0 {
					t.Errorf("request %s is above limit %s", r.String(), l.String())
				}
			}
		})
	}
}

func TestServerResourcesDoNotChangeTheSpec(t *testing.T) {
	s := serverWith(corev1.ResourceRequirements{Requests: corev1.ResourceList{"cpu": q("1")}}, vtv1.StorageEphemeral)
	_ = serverResources(s)
	if _, ok := s.Spec.Resources.Requests["memory"]; ok || len(s.Spec.Resources.Requests) != 1 {
		t.Fatalf("the spec was changed: %v", s.Spec.Resources.Requests)
	}
}
