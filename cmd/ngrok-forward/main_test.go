package main

import "testing"

func TestLoadConfig(t *testing.T) {
	t.Run("missing authtoken", func(t *testing.T) {
		t.Setenv("NGROK_AUTHTOKEN", "")
		t.Setenv("NGROK_DOMAIN", "d.ngrok.dev")
		if _, err := loadConfig(); err == nil {
			t.Error("expected error when NGROK_AUTHTOKEN is unset")
		}
	})
	t.Run("missing domain", func(t *testing.T) {
		t.Setenv("NGROK_AUTHTOKEN", "tok")
		t.Setenv("NGROK_DOMAIN", "")
		if _, err := loadConfig(); err == nil {
			t.Error("expected error when NGROK_DOMAIN is unset")
		}
	})
	t.Run("defaults upstream", func(t *testing.T) {
		t.Setenv("NGROK_AUTHTOKEN", "tok")
		t.Setenv("NGROK_DOMAIN", "d.ngrok.dev")
		t.Setenv("NGROK_UPSTREAM", "")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.upstream != "http://localhost:8080" {
			t.Errorf("upstream = %q, want the Synapse WAF default", cfg.upstream)
		}
	})
	t.Run("honors explicit upstream", func(t *testing.T) {
		t.Setenv("NGROK_AUTHTOKEN", "tok")
		t.Setenv("NGROK_DOMAIN", "d.ngrok.dev")
		t.Setenv("NGROK_UPSTREAM", "http://localhost:9999")
		cfg, err := loadConfig()
		if err != nil || cfg.upstream != "http://localhost:9999" {
			t.Errorf("cfg = %+v, err = %v", cfg, err)
		}
		if cfg.token != "tok" || cfg.domain != "d.ngrok.dev" {
			t.Errorf("cfg = %+v, want token/domain populated", cfg)
		}
	})
}
