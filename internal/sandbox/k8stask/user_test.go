package k8stask

import (
	"reflect"
	"testing"
)

// TestPlatformUserOfTheRunner checks that platform.linux.user of the runner
// (task.Platform.User) sets the user of the Pod, or keeps the user of the image.
func TestPlatformUserOfTheRunner(t *testing.T) {
	cases := []struct {
		name string
		user string
		mod  func(*Options)
		want map[string]any // nil means no securityContext of the backend
	}{
		{"default", "", nil, map[string]any{"runAsNonRoot": true, "runAsUser": float64(1000), "runAsGroup": float64(1000), "fsGroup": float64(1000)}},
		{"number", "2500", nil, map[string]any{"runAsNonRoot": true, "runAsUser": float64(2500), "runAsGroup": float64(2500), "fsGroup": float64(2500)}},
		{"image", "image", nil, nil},
		{"image wins over the setting", "image", func(o *Options) { o.RunAsUser = 3000 }, nil},
		{"number wins over the image setting", "2500", func(o *Options) { o.RunAsImageUser = true }, map[string]any{"runAsNonRoot": true, "runAsUser": float64(2500), "runAsGroup": float64(2500), "fsGroup": float64(2500)}},
		{"bad value is ignored", "root", nil, map[string]any{"runAsNonRoot": true, "runAsUser": float64(1000), "runAsGroup": float64(1000), "fsGroup": float64(1000)}},
		{"zero is ignored", "0", nil, map[string]any{"runAsNonRoot": true, "runAsUser": float64(1000), "runAsGroup": float64(1000), "fsGroup": float64(1000)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeAPI(t)
			b := newBackend(t, f, c.mod)
			task := testTask("run_user", 1)
			task.Platform.User = c.user
			if _, err := b.Start(ctx5(t), task); err != nil {
				t.Fatal(err)
			}
			sc, _ := podSpecOf(t, f, task.TaskID)["securityContext"].(map[string]any)
			if c.want == nil {
				if sc != nil {
					t.Errorf("securityContext = %v, want none", sc)
				}
				return
			}
			if !reflect.DeepEqual(sc, c.want) {
				t.Errorf("securityContext = %v, want %v", sc, c.want)
			}
		})
	}
}
