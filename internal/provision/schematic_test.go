package provision

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFactoryResolve(t *testing.T) {
	var gotBody, gotCT, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"id":"abc123"}`))
	}))
	defer srv.Close()

	c := &FactoryClient{baseURL: srv.URL, http: srv.Client()}
	id, err := c.Resolve(context.Background(), []string{"siderolabs/gvisor", "siderolabs/tailscale"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "abc123" {
		t.Fatalf("id = %q, want abc123", id)
	}
	if gotPath != "POST /schematics" {
		t.Fatalf("request = %q", gotPath)
	}
	if gotCT != "application/yaml" {
		t.Fatalf("content-type = %q", gotCT)
	}
	if !strings.Contains(gotBody, "siderolabs/gvisor") || !strings.Contains(gotBody, "officialExtensions") {
		t.Fatalf("posted body:\n%s", gotBody)
	}
}

func TestFactoryResolveErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad extension", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := &FactoryClient{baseURL: srv.URL, http: srv.Client()}
	if _, err := c.Resolve(context.Background(), nil); err == nil {
		t.Fatal("expected error on non-2xx")
	}
}

func TestSchematicYAML(t *testing.T) {
	if got := schematicYAML(nil); got != "customization: {}\n" {
		t.Fatalf("empty schematic = %q", got)
	}
	got := schematicYAML([]string{"siderolabs/tailscale"})
	for _, want := range []string{"systemExtensions", "officialExtensions", "- siderolabs/tailscale"} {
		if !strings.Contains(got, want) {
			t.Fatalf("schematic %q missing %q", got, want)
		}
	}
}

func TestInstallImageAndBootAssets(t *testing.T) {
	if got := InstallImage("", "abc", "v1.13.5"); got != "factory.talos.dev/metal-installer/abc:v1.13.5" {
		t.Fatalf("install image = %q", got)
	}
	kernel, initrd := BootAssets("", "abc", "v1.13.5", "")
	if kernel != "https://factory.talos.dev/image/abc/v1.13.5/kernel-amd64" {
		t.Fatalf("kernel = %q", kernel)
	}
	if len(initrd) != 1 || initrd[0] != "https://factory.talos.dev/image/abc/v1.13.5/initramfs-amd64.xz" {
		t.Fatalf("initrd = %v", initrd)
	}
}
