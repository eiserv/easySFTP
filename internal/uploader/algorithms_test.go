package uploader

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/eiserv/easySFTP/internal/config"
)

func TestApplySSHAlgorithmsIsAdditiveByCategory(t *testing.T) {
	client := &ssh.ClientConfig{}
	applySSHAlgorithms(client, config.SSHAlgorithms{
		KeyExchanges:      []string{ssh.InsecureKeyExchangeDH1SHA1},
		Ciphers:           []string{ssh.InsecureCipherAES128CBC},
		MACs:              []string{ssh.InsecureHMACSHA196},
		HostKeyAlgorithms: []string{ssh.KeyAlgoRSA},
	})

	supported := ssh.SupportedAlgorithms()
	checks := []struct {
		name string
		got  []string
		safe string
		old  string
	}{
		{"key exchange", client.KeyExchanges, supported.KeyExchanges[0], ssh.InsecureKeyExchangeDH1SHA1},
		{"cipher", client.Ciphers, supported.Ciphers[0], ssh.InsecureCipherAES128CBC},
		{"MAC", client.MACs, supported.MACs[0], ssh.InsecureHMACSHA196},
		{"host key", client.HostKeyAlgorithms, supported.HostKeys[0], ssh.KeyAlgoRSA},
	}
	for _, check := range checks {
		if len(check.got) == 0 || check.got[0] != check.safe || !slices.Contains(check.got, check.old) {
			t.Errorf("%s list is not safe-first and additive: %#v", check.name, check.got)
		}
	}

	defaults := &ssh.ClientConfig{}
	applySSHAlgorithms(defaults, config.SSHAlgorithms{})
	if defaults.KeyExchanges != nil || defaults.Ciphers != nil || defaults.MACs != nil || defaults.HostKeyAlgorithms != nil {
		t.Fatalf("empty additions changed library defaults: %#v", defaults)
	}
}

func TestLegacySSHAlgorithmsInteroperate(t *testing.T) {
	cases := []struct {
		name      string
		algorithm string
		configure func(*config.Config)
		server    serverOption
	}{
		{
			"key exchange",
			ssh.InsecureKeyExchangeDH1SHA1,
			func(cfg *config.Config) { cfg.Algorithms.KeyExchanges = []string{ssh.InsecureKeyExchangeDH1SHA1} },
			withOnlyKeyExchange(ssh.InsecureKeyExchangeDH1SHA1),
		},
		{
			"cipher",
			ssh.InsecureCipherAES128CBC,
			func(cfg *config.Config) { cfg.Algorithms.Ciphers = []string{ssh.InsecureCipherAES128CBC} },
			withOnlyCipher(ssh.InsecureCipherAES128CBC),
		},
		{
			"MAC",
			ssh.InsecureHMACSHA196,
			func(cfg *config.Config) { cfg.Algorithms.MACs = []string{ssh.InsecureHMACSHA196} },
			withOnlyMAC(ssh.InsecureHMACSHA196),
		},
		{
			"host key",
			ssh.KeyAlgoRSA,
			func(cfg *config.Config) { cfg.Algorithms.HostKeyAlgorithms = []string{ssh.KeyAlgoRSA} },
			withOnlyRSAHostKey(t),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startTestServer(t, tc.server)
			local := t.TempDir()
			writeTree(t, local, map[string]string{"file.txt": tc.name})
			cfg := baseConfig(srv)
			cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
			tc.configure(cfg)
			cfg.Algorithms.Insecure = []string{tc.algorithm}
			log := &recordingLogger{testLogger: testLogger{t}}
			if _, err := Run(context.Background(), cfg, log); err != nil {
				t.Fatalf("legacy %s handshake failed: %v", tc.name, err)
			}
			algorithmWarnings := 0
			for _, warning := range log.warnings {
				if strings.Contains(warning, "connection.algorithms") && strings.Contains(warning, tc.algorithm) {
					algorithmWarnings++
				}
			}
			if algorithmWarnings != 1 {
				t.Fatalf("warning for %s: %#v", tc.algorithm, log.warnings)
			}
		})
	}
}

func withOnlyKeyExchange(name string) serverOption {
	return func(s *testServer) { s.sshConfig.KeyExchanges = []string{name} }
}

func withOnlyCipher(name string) serverOption {
	return func(s *testServer) { s.sshConfig.Ciphers = []string{name} }
}

func withOnlyMAC(name string) serverOption {
	return func(s *testServer) { s.sshConfig.MACs = []string{name} }
}

func withOnlyRSAHostKey(t *testing.T) serverOption {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	algorithmSigner, ok := signer.(ssh.AlgorithmSigner)
	if !ok {
		t.Fatal("RSA signer does not implement ssh.AlgorithmSigner")
	}
	restricted, err := ssh.NewSignerWithAlgorithms(algorithmSigner, []string{ssh.KeyAlgoRSA})
	if err != nil {
		t.Fatal(err)
	}
	return func(s *testServer) {
		s.hostSigner = restricted
		s.HostPubKey = restricted.PublicKey()
		s.HostKeySHA256 = ssh.FingerprintSHA256(restricted.PublicKey())
	}
}
