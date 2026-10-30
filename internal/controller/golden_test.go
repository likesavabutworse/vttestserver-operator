package controller

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the files in testdata")

// A change of the generated StatefulSet or Job rolls every environment when the new operator version
// starts. These files make such a change visible in review. If a change is intended, run
//
//	go test ./internal/controller -run TestGeneratedObjectsAreStable -update-golden
//
// and say in the release notes that servers restart.
func TestGeneratedObjectsAreStable(t *testing.T) {
	minimal := &vtv1.VTTestServer{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns"},
		Spec: vtv1.VTTestServerSpec{
			Image:     "vitess/vttestserver:mysql84",
			Keyspaces: []vtv1.KeyspaceSpec{{Name: "commerce", Shards: 1}},
		},
	}
	full := &vtv1.VTTestServer{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns"},
		Spec: vtv1.VTTestServerSpec{
			Image:     "vitess/vttestserver:mysql84",
			Keyspaces: []vtv1.KeyspaceSpec{{Name: "commerce", Shards: 1}, {Name: "customer", Shards: 10}},
			Storage:   vtv1.StorageSpec{Type: vtv1.StoragePersistent},
			MySQLConfig: map[string]string{
				"max_connections":         "600",
				"innodb_buffer_pool_size": "128M",
			},
			ExtraArgs:          []string{"--no-scatter"},
			AllowVSchemaDDL:    true,
			ServiceAccountName: "snapshots",
			RestoreFrom: &vtv1.RestoreSpec{Tag: "latest", S3: vtv1.S3Spec{
				Bucket: "bkt", Prefix: "pre", Region: "us-east-1", Endpoint: "http://silo:9000", ForcePathStyle: true,
				CredentialsSecretRef: &corev1.LocalObjectReference{Name: "creds"},
			}},
		},
	}
	snap := &vtv1.VTTestServerSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "golden-1", Namespace: "ns"},
		Spec: vtv1.VTTestServerSnapshotSpec{
			ServerRef:          vtv1.ServerRef{Name: "shop"},
			ServiceAccountName: "snapshots",
			Tags:               []string{"latest"},
			S3:                 vtv1.S3Spec{Bucket: "bkt", Prefix: "pre", Endpoint: "http://silo:9000", ForcePathStyle: true},
		},
	}

	objects := map[string]any{
		"statefulset-minimal.yaml": buildStatefulSet(minimal, "datactl:test", 1),
		"statefulset-full.yaml":    buildStatefulSet(full, "datactl:test", 1),
		"job-upload.yaml":          uploadJob(snap, full, "datactl:test", 30*time.Minute),
		"job-delete.yaml":          deleteJob(snap, "datactl:test"),
	}
	for file, obj := range objects {
		t.Run(file, func(t *testing.T) {
			got, err := yaml.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", file)
			if *updateGolden {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (create it with -update-golden)", err)
			}
			if string(want) != string(got) {
				t.Fatalf("the generated object changed. Servers restart when the operator is upgraded.\n"+
					"If this is intended, run with -update-golden and write it in the release notes.\n--- want\n%s\n--- got\n%s", want, got)
			}
		})
	}
}
