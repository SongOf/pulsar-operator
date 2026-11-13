package v1alpha1_test

import (
	"encoding/json"
	"reflect"
	"testing"

	pulsarv1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// Both modules have a v1alpha1 package, but their public group/kind identities
// must remain separate in the same scheme. Existing storage JSON must retain
// required zero values and exact device capacities through typed conversion.
func TestModuleSchemesAndExistingStorageJSON(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{pulsarv1.AddToScheme, storagev1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"PulsarCluster", "PulsarClusterList", "BookKeeperCluster", "BookKeeperClusterList"} {
		if _, err := scheme.New(pulsarv1.GroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
		if _, err := scheme.New(storagev1.GroupVersion.WithKind(kind)); err == nil {
			t.Fatalf("Pulsar kind registered in storage group: %s", kind)
		}
	}
	for _, kind := range []string{"DiskPool", "DiskPoolList", "PhysicalDisk", "PhysicalDiskList"} {
		if _, err := scheme.New(storagev1.GroupVersion.WithKind(kind)); err != nil {
			t.Fatal(err)
		}
	}
	decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	for _, raw := range []string{
		`{"apiVersion":"storage.diskfleet.io/v1alpha1","kind":"DiskPool","metadata":{"name":"approved-later"},"spec":{"nodeName":"worker-1","vgName":"df_test","approved":false,"devices":[{"path":"/dev/disk/by-id/ata-test","serial":"SERIAL","sizeBytes":9007199254740993}]}}`,
		`{"apiVersion":"storage.diskfleet.io/v1alpha1","kind":"PhysicalDisk","metadata":{"name":"unidentified-disk"},"spec":{"nodeName":"worker-1","path":"/dev/sdb","serial":"","sizeBytes":0,"model":"unknown","media":"unknown"},"status":{"phase":"Protected","reasons":["Missing stable identity"]}}`,
	} {
		obj, gvk, err := decoder.Decode([]byte(raw), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		original := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(raw), &original); err != nil {
			t.Fatal(err)
		}
		data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(data["spec"])
		if err != nil {
			t.Fatal(err)
		}
		var expected, actual map[string]json.RawMessage
		if err := json.Unmarshal(original["spec"], &expected); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got, &actual); err != nil {
			t.Fatal(err)
		}
		// Compare each property's JSON, allowing object key ordering to differ.
		if len(expected) != len(actual) {
			t.Fatalf("%s dropped required properties: %s", gvk.Kind, got)
		}
		for key, want := range expected {
			var left, right any
			if key == "devices" {
				var devices []storagev1.Device
				if err := json.Unmarshal(actual[key], &devices); err != nil {
					t.Fatal(err)
				}
				if devices[0].SizeBytes != 9007199254740993 {
					t.Fatalf("device capacity lost precision: %s", got)
				}
			}
			if err := json.Unmarshal(want, &left); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(actual[key], &right); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(left, right) {
				t.Fatalf("%s.%s changed: %s", gvk.Kind, key, got)
			}
		}
	}
}
