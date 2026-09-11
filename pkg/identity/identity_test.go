package identity

import (
	"os"
	"testing"
)

func TestEnvironmentWinsOverConfig(t *testing.T) {
	t.Setenv("PODCD_HOST", "from-env")
	id, err := Resolve("from-config")
	if err != nil {
		t.Fatal(err)
	}
	if id.Host != "from-env" {
		t.Fatalf("host = %q, want from-env", id.Host)
	}
	if id.Source == "" {
		t.Error("the identity must say where it came from")
	}
}

func TestConfigWinsOverHostname(t *testing.T) {
	os.Unsetenv("PODCD_HOST")
	id, err := Resolve("prod-web-01")
	if err != nil {
		t.Fatal(err)
	}
	if id.Host != "prod-web-01" {
		t.Fatalf("host = %q", id.Host)
	}
}

func TestFallsBackToHostname(t *testing.T) {
	os.Unsetenv("PODCD_HOST")
	id, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	if id.Host == "" {
		t.Fatal("no identity was resolved")
	}
	if id.Hostname == "" || hostname == "" {
		t.Skip("this machine has no hostname")
	}
	// The domain is stripped: prod-web-01.example.com and prod-web-01 are the
	// same VM as far as Git is concerned.
	if len(id.Host) > len(hostname) {
		t.Fatalf("host %q is longer than the hostname %q", id.Host, hostname)
	}
}
