package db

import (
	"strings"
	"testing"
	"time"
)

// Exact-text comparison depends on every connection printing values the same
// way, whatever the connection string asks for.
func TestConfigPinsOutputSettings(t *testing.T) {
	cfg, err := Config("postgres://alice@db.example.com/shop?datestyle=SQL&TimeZone=Asia/Tokyo&extra_float_digits=0")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"DateStyle": "ISO, MDY", "TimeZone": "UTC", "extra_float_digits": "3", "search_path": ""}
	for key, value := range want {
		if got, ok := cfg.RuntimeParams[key]; !ok || got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if _, ok := cfg.RuntimeParams["datestyle"]; ok {
		t.Error("the user's lowercase datestyle survived next to the pinned DateStyle")
	}
	if cfg.RuntimeParams["application_name"] != "agent-undo" || cfg.ConnectTimeout != 10*time.Second {
		t.Errorf("defaults: application_name %q, connect timeout %v", cfg.RuntimeParams["application_name"], cfg.ConnectTimeout)
	}

	named, err := Config("postgres://alice@localhost/shop?application_name=recorder-1&connect_timeout=3")
	if err != nil {
		t.Fatal(err)
	}
	if named.RuntimeParams["application_name"] != "recorder-1" || named.ConnectTimeout != 3*time.Second {
		t.Errorf("the user's application_name and connect_timeout were not kept")
	}
}

func TestConfigErrorsHideThePassword(t *testing.T) {
	_, err := Config("postgres://alice:s3cret@db.example.com:notaport/shop")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error = %v; want an error without the password", err)
	}
}

func TestTLSWarning(t *testing.T) {
	tests := map[string]bool{
		"postgres://alice@localhost/shop":                                    false, // this machine
		"postgres://alice@127.0.0.1/shop?sslmode=disable":                    false,
		"host=/var/run/postgresql user=alice dbname=shop":                    false, // a Unix socket
		"postgres://alice@db.example.com/shop":                               true,  // prefer: no verification, plain-text fallback
		"postgres://alice@db.example.com/shop?sslmode=require":               true,
		"postgres://alice@db.example.com/shop?sslmode=verify-full":           false,
		"postgres://alice@localhost,db.example.com/shop?sslmode=disable":     true, // a remote fallback host
		"postgres://alice@localhost,db.example.com/shop?sslmode=verify-full": false,
	}
	for conn, want := range tests {
		cfg, err := Config(conn)
		if err != nil {
			t.Fatalf("%s: %v", conn, err)
		}
		if got := TLSWarning(cfg) != ""; got != want {
			t.Errorf("%s: warning = %v, want %v", conn, got, want)
		}
	}
}
