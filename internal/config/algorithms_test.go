package config

import (
	"slices"
	"strings"
	"testing"
)

func TestConfigFileSSHAlgorithms(t *testing.T) {
	cfg, err := loadFile(t, `version: 3
connection:
  host: h
  username: u
  algorithms:
    key_exchanges: [diffie-hellman-group1-sha1, diffie-hellman-group1-sha1]
    ciphers: [aes128-cbc]
    macs: [hmac-sha1-96]
    host_key_algorithms: [ssh-rsa]
deployments:
  web:
    source: a
    target: /b
`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Algorithms.KeyExchanges, []string{"diffie-hellman-group1-sha1"}) {
		t.Fatalf("key exchanges: %#v", cfg.Algorithms.KeyExchanges)
	}
	for _, name := range []string{"diffie-hellman-group1-sha1", "aes128-cbc", "hmac-sha1-96", "ssh-rsa"} {
		if !slices.Contains(cfg.Algorithms.Insecure, name) {
			t.Errorf("insecure algorithms do not contain %q: %#v", name, cfg.Algorithms.Insecure)
		}
	}
}

func TestConfigFileRejectsUnknownSSHAlgorithm(t *testing.T) {
	_, err := loadFile(t, `version: 3
connection:
  host: h
  username: u
  algorithms:
    ciphers: [rot13]
deployments:
  web:
    source: a
    target: /b
`)
	if err == nil || !strings.Contains(err.Error(), `connection.algorithms.ciphers' contains unsupported SSH algorithm "rot13"`) {
		t.Fatalf("unknown algorithm error: %v", err)
	}
}
