package config

import (
	"strings"
	"testing"
)

// baseSyncConfig returns the smallest config that passes every other
// validation rule, so these tests exercise checkSyncTargets alone.
func baseSyncConfig() *Config {
	cfg := Defaults()
	cfg.Server = "sftp.example.com"
	cfg.Port = 22
	cfg.Username = "deploy"
	cfg.Password = "hunter2"
	return cfg
}

// Two sync deployments into one target share one manifest and delete each
// other's files in a green run (issue #278), so the configuration must be
// refused before the network is touched.
func TestValidateRefusesTwoSyncDeploymentsIntoOneTarget(t *testing.T) {
	cfg := baseSyncConfig()
	cfg.Uploads = []UploadPair{
		{Name: "site", Local: "dist", Remote: "/var/www/html", Strategy: StrategySync},
		{Name: "assets", Local: "assets-build", Remote: "/var/www/html/", Strategy: StrategySync},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected two sync deployments into one target to be refused")
	}
	if !strings.Contains(err.Error(), `deployments "site" and "assets"`) {
		t.Errorf("error should name both deployments, got %q", err)
	}
	if !strings.Contains(err.Error(), "merge the two sources into one directory") {
		t.Errorf("error should say what to do instead, got %q", err)
	}
}

// Every spelling of the same directory is one path: a trailing slash, an
// interior dot, a climbing ".." and a backslash spelling must not slip
// past the comparison the way they slip past a string equality check.
func TestValidateRefusesSyncTargetsThatDifferOnlyInSpelling(t *testing.T) {
	for _, spelling := range []string{
		"/var/www/html/",       // trailing slash
		"/var/www/html/./",     // interior dot
		"/var/www/html/sub/..", // climbing ..
		`\var\www\html`,        // backslashes
	} {
		cfg := baseSyncConfig()
		cfg.Uploads = []UploadPair{
			{Name: "site", Local: "dist", Remote: "/var/www/html", Strategy: StrategySync},
			{Name: "assets", Local: "assets-build", Remote: spelling, Strategy: StrategySync},
		}
		err := cfg.validate()
		if err == nil {
			t.Errorf("spelling %q must normalize to the same target and be refused", spelling)
			continue
		}
		if !strings.Contains(err.Error(), `deployments "site" and "assets"`) {
			t.Errorf("refusal for spelling %q should name both deployments, got %q", spelling, err)
		}
	}
}

// The two ways a Windows SFTP server addresses a drive, "C:/www" and
// "/C:/www", name the same directory, and path.Clean keeps them distinct.
// The drive-root guard already folds the two forms (isDriveRoot accepts
// both "X:" and "/X:"), so the shared-target refusal must too, or one sync
// spelled each way shares a manifest in silence.
func TestValidateRefusesSyncTargetsThatDifferOnlyInDriveSpelling(t *testing.T) {
	cfg := baseSyncConfig()
	cfg.Uploads = []UploadPair{
		{Name: "site", Local: "dist", Remote: "C:/www", Strategy: StrategySync},
		{Name: "assets", Local: "assets-build", Remote: "/C:/www", Strategy: StrategySync},
	}
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected the two drive spellings of one target to be refused")
	}
	if !strings.Contains(err.Error(), `deployments "site" and "assets"`) {
		t.Errorf("error should name both deployments, got %q", err)
	}
}

// The warnings fold drive spellings as well, so the clean-overlap detection
// cannot be dodged by spelling the two targets one way each.
func TestSyncTargetWarningsFoldDriveSpellings(t *testing.T) {
	cfg := baseSyncConfig()
	cfg.Uploads = []UploadPair{
		{Name: "site", Local: "dist", Remote: "C:/www", Strategy: StrategySync},
		{Name: "reset", Local: "empty", Remote: "/C:/www", Strategy: StrategyClean},
	}
	warnings := cfg.SyncTargetWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for the drive-spelled overlap, got %d: %v", len(warnings), warnings)
	}
}

// The refusal is about sharing a manifest, not about syncing: distinct
// targets keep working, including two sync deployments next to each other.
func TestValidateAllowsTwoSyncDeploymentsIntoDistinctTargets(t *testing.T) {
	cfg := baseSyncConfig()
	cfg.Uploads = []UploadPair{
		{Name: "site", Local: "dist", Remote: "/var/www/html", Strategy: StrategySync},
		{Name: "docs", Local: "docs-build", Remote: "/var/www/docs", Strategy: StrategySync},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("distinct sync targets must stay valid, got %v", err)
	}
}

// overlay and clean write no manifest, so they can share a target with a
// sync deployment without triggering this refusal; the clean overlap is
// warned about separately instead.
func TestValidateAllowsOtherModesToShareASyncTarget(t *testing.T) {
	cfg := baseSyncConfig()
	cfg.Uploads = []UploadPair{
		{Name: "site", Local: "dist", Remote: "/var/www/html", Strategy: StrategySync},
		{Name: "stamp", Local: "build-stamp", Remote: "/var/www/html/stamp.txt", Strategy: StrategyOverlay},
		{Name: "reset", Local: "empty", Remote: "/var/www/html", Strategy: StrategyClean},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("overlay/clean sharing a sync target must stay valid, got %v", err)
	}
	warnings := cfg.SyncTargetWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for the clean overlap, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], `"site"`) || !strings.Contains(warnings[0], `"reset"`) {
		t.Errorf("warning should name both deployments, got %q", warnings[0])
	}
}

// A sync target inside a clean target is warned about, as is the reverse:
// a clean inside a sync deletes manifest-listed files that a manifest-based
// sync never re-uploads.
func TestSyncTargetWarningsCoverBothNestingOrders(t *testing.T) {
	cfg := baseSyncConfig()
	cfg.Uploads = []UploadPair{
		{Name: "site", Local: "dist", Remote: "/var/www", Strategy: StrategySync},
		{Name: "cache", Local: "empty", Remote: "/var/www/cache", Strategy: StrategyClean},
	}
	warnings := cfg.SyncTargetWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for clean-inside-sync, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "never re-uploads an unchanged file") {
		t.Errorf("warning should explain the clean-inside-sync failure mode, got %q", warnings[0])
	}
}

// Disjoint targets produce no warnings, and neither does a config with a
// single deployment.
func TestSyncTargetWarningsQuietForDisjointTargets(t *testing.T) {
	cfg := baseSyncConfig()
	cfg.Uploads = []UploadPair{
		{Name: "site", Local: "dist", Remote: "/var/www", Strategy: StrategySync},
		{Name: "reset", Local: "empty", Remote: "/srv/fresh", Strategy: StrategyClean},
	}
	if warnings := cfg.SyncTargetWarnings(); len(warnings) != 0 {
		t.Errorf("disjoint targets should not warn, got %v", warnings)
	}
}

// Two clean deployments into one target delete everything in order and
// upload their own trees; neither reads the other's state, so there is
// nothing to warn about either.
func TestSyncTargetWarningsIgnoreCleanCleanOverlaps(t *testing.T) {
	cfg := baseSyncConfig()
	cfg.Uploads = []UploadPair{
		{Name: "a", Local: "a", Remote: "/var/www", Strategy: StrategyClean},
		{Name: "b", Local: "b", Remote: "/var/www", Strategy: StrategyClean},
	}
	if warnings := cfg.SyncTargetWarnings(); len(warnings) != 0 {
		t.Errorf("clean/clean overlaps are documented clean-vs-clean behaviour, should not warn, got %v", warnings)
	}
}

// The end-to-end path through the YAML file must reach the same refusal,
// including the parse of the deployments map.
func TestLoadConfigModeRefusesTwoSyncDeploymentsIntoOneTarget(t *testing.T) {
	setConfigModeEnv(t, `version: 3
connection:
  host: sftp.example.com
  username: deploy
deployments:
  site:
    source: dist
    target: /var/www/html
    mode: sync
  assets:
    source: assets-build
    target: /var/www/html/
    mode: sync
`)
	_, err := Load()
	if err == nil {
		t.Fatal("expected the config file to be refused")
	}
	if !strings.Contains(err.Error(), `deployments "site" and "assets"`) {
		t.Errorf("error should name both deployments from the file, got %q", err)
	}
}
