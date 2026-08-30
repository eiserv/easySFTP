package uploader

import (
	"context"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

func TestKeyboardInteractivePasswordUpload(t *testing.T) {
	srv := startTestServer(t, withKeyboardInteractiveOnly())
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "keyboard interactive"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatalf("upload through keyboard-interactive authentication failed: %v", err)
	}
	if stats.FilesUploaded != 1 {
		t.Fatalf("uploaded files: got %d, want 1", stats.FilesUploaded)
	}
}

func TestPasswordChallengeDoesNotLeakOrReusePassword(t *testing.T) {
	challenge := passwordChallenge("correct horse battery staple")

	answers, err := challenge("server", "", []string{"Password: ", "Account: "}, []bool{false, true})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || answers[0] != "correct horse battery staple" || answers[1] != "" {
		t.Fatalf("unexpected answers: %#v", answers)
	}

	_, err = challenge("server", "", []string{"Verification code: "}, []bool{false})
	if err == nil || !strings.Contains(err.Error(), "one password prompt only") {
		t.Fatalf("additional secret prompt error: got %v", err)
	}
}

func TestPasswordChallengeRejectsMultipleSecretQuestions(t *testing.T) {
	challenge := passwordChallenge("secret")
	_, err := challenge("server", "", []string{"Password: ", "Token: "}, []bool{false, false})
	if err == nil || !strings.Contains(err.Error(), "one password prompt only") {
		t.Fatalf("multiple secret prompt error: got %v", err)
	}
}
