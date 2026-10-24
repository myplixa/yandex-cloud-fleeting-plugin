package fleeting_plugin_yc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	ycsdk "github.com/yandex-cloud/go-sdk"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func minimalTemplate(preemptible bool) InstanceTemplate {
	var tpl InstanceTemplate
	tpl.Platform = "standard-v3"
	tpl.Zone = "ru-central1-a"
	tpl.Preemptible = preemptible
	tpl.Resources.CPU = 2
	tpl.Resources.Memory = 2147483648
	tpl.Disk.Type = "network-ssd"
	tpl.Disk.Size = 17179869184
	tpl.Disk.Image = "fd8image"
	tpl.Network.Network = "net1"
	tpl.Network.Subnet = "subnet1"
	return tpl
}

func newTestGroup(preemptible bool) *InstanceGroup {
	g := &InstanceGroup{Name: "test-runners"}
	g.template = minimalTemplate(preemptible)
	g.settings = provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{Username: "ci"},
	}
	g.SshPublicKey = "ssh-rsa AAAAtest"
	return g
}

// TestPreemptible_HonorsTemplateField guards against the upstream bug where
// SchedulingPolicy.Preemptible was hardcoded to true regardless of what the
// operator set in template.yml's preemptible field.
func TestPreemptible_HonorsTemplateField(t *testing.T) {
	for _, want := range []bool{true, false} {
		g := newTestGroup(want)
		tpl := g.GenerateInstanceGroup()
		if got := tpl.SchedulingPolicy.Preemptible; got != want {
			t.Fatalf("preemptible=%v in template.yml, but SchedulingPolicy.Preemptible=%v", want, got)
		}
	}
}

// TestGenerateMeta_NoExtras verifies the plain case (no monitoring, no
// cloud_init_extra_file) still sets ssh-keys and nothing else weird.
func TestGenerateMeta_NoExtras(t *testing.T) {
	g := newTestGroup(false)
	meta := g.GenerateMeta()

	if meta["ssh-keys"] != "ci:ssh-rsa AAAAtest" {
		t.Fatalf("unexpected ssh-keys value: %q", meta["ssh-keys"])
	}
	if _, ok := meta["user-data"]; ok {
		t.Fatalf("expected no user-data when monitoring is disabled and no cloud_init_extra_file is set, got: %q", meta["user-data"])
	}
}

// TestGenerateMeta_MonitoringOnly verifies the pre-existing monitoring-agent
// behavior is unchanged by the GenerateMeta rewrite.
func TestGenerateMeta_MonitoringOnly(t *testing.T) {
	g := newTestGroup(false)
	g.template.Monitoring.Enabled = true

	meta := g.GenerateMeta()

	if meta["install-unified-agent"] != "1" {
		t.Fatalf("expected install-unified-agent=1, got %q", meta["install-unified-agent"])
	}
	if meta["user-data"] == "" || !strings.Contains(meta["user-data"], "unifiedAgent") {
		t.Fatalf("expected user-data to contain the unified agent installer, got: %q", meta["user-data"])
	}
}

// TestGenerateMeta_MonitoringPlusExtra_MergesAsMultipart verifies that when
// both monitoring and a cloud_init_extra_file are set, both end up present
// (via multipart) instead of one clobbering the other - this is the actual
// gap this fork closes relative to upstream, which had no extensibility
// mechanism at all.
func TestGenerateMeta_MonitoringPlusExtra_MergesAsMultipart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/extra.yaml"
	if err := os.WriteFile(path, []byte("#cloud-config\npackages:\n  - docker.io\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	tmpl, err := loadCloudInitExtraTemplate(path)
	if err != nil {
		t.Fatalf("loading template: %v", err)
	}

	g := newTestGroup(false)
	g.template.Monitoring.Enabled = true
	g.cloudInitExtraTemplate = tmpl

	meta := g.GenerateMeta()

	userData := meta["user-data"]
	if !strings.Contains(userData, "unifiedAgent") {
		t.Fatalf("expected monitoring part to still be present, got: %q", userData)
	}
	if !strings.Contains(userData, "docker.io") {
		t.Fatalf("expected cloud_init_extra_file content to be present, got: %q", userData)
	}
	if !strings.Contains(userData, "boundary") && !strings.Contains(userData, "--") {
		t.Fatalf("expected multipart MIME framing when merging 2 parts, got: %q", userData)
	}
}

// TestCredentials_DefaultsToInstanceServiceAccount guards backward
// compatibility: without key_file the plugin keeps authenticating via the
// metadata service of the VM it runs on.
func TestCredentials_DefaultsToInstanceServiceAccount(t *testing.T) {
	g := &InstanceGroup{}

	creds, err := g.credentials()
	if err != nil {
		t.Fatalf("credentials() without key_file: %v", err)
	}

	want := reflect.TypeOf(ycsdk.InstanceServiceAccount())
	if got := reflect.TypeOf(creds); got != want {
		t.Fatalf("credentials() without key_file returned %v, want %v", got, want)
	}
}

// TestCredentials_KeyFileSignsJWTForServiceAccount checks that key_file
// switches the plugin to the service account key: the IAM token request is a
// JWT issued by that service account and signed with that key. Building the
// request only signs the JWT locally - nothing is sent to Yandex Cloud.
func TestCredentials_KeyFileSignsJWTForServiceAccount(t *testing.T) {
	path := writeServiceAccountKey(t, map[string]string{
		"id":                 "ajekeyid",
		"service_account_id": "ajesaid",
		"key_algorithm":      "RSA_2048",
		"private_key":        newPrivateKeyPEM(t),
	})
	g := &InstanceGroup{KeyFile: path}

	creds, err := g.credentials()
	if err != nil {
		t.Fatalf("credentials() with key_file: %v", err)
	}

	exchangeable, ok := creds.(ycsdk.ExchangeableCredentials)
	if !ok {
		t.Fatalf("credentials() with key_file returned %T, want service account key credentials", creds)
	}
	req, err := exchangeable.IAMTokenRequest()
	if err != nil {
		t.Fatalf("IAMTokenRequest(): %v", err)
	}

	segments := strings.Split(req.GetJwt(), ".")
	if len(segments) != 3 {
		t.Fatalf("expected a signed JWT in the IAM token request, got %q", req.GetJwt())
	}
	if kid := decodeJWTSegment(t, segments[0])["kid"]; kid != "ajekeyid" {
		t.Fatalf("JWT kid = %v, want the key id from key_file", kid)
	}
	if iss := decodeJWTSegment(t, segments[1])["iss"]; iss != "ajesaid" {
		t.Fatalf("JWT iss = %v, want the service account id from key_file", iss)
	}
}

func TestCredentials_KeyFileErrors(t *testing.T) {
	validPEM := newPrivateKeyPEM(t)

	cases := []struct {
		name    string
		content string // empty means the file does not exist
	}{
		{name: "missing file"},
		{name: "not JSON", content: "not a key"},
		{name: "user account key", content: keyJSON(t, map[string]string{
			"id":              "ajekeyid",
			"user_account_id": "ajeuserid",
			"private_key":     validPEM,
		})},
		{name: "no key id", content: keyJSON(t, map[string]string{
			"service_account_id": "ajesaid",
			"private_key":        validPEM,
		})},
		{name: "broken private key", content: keyJSON(t, map[string]string{
			"id":                 "ajekeyid",
			"service_account_id": "ajesaid",
			"private_key":        "-----BEGIN PRIVATE KEY-----\nbroken\n-----END PRIVATE KEY-----\n",
		})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatalf("writing fixture: %v", err)
				}
			}
			g := &InstanceGroup{KeyFile: path}

			creds, err := g.credentials()
			if err == nil {
				t.Fatalf("expected an error, got credentials %T", creds)
			}
			if !strings.Contains(err.Error(), "key_file") {
				t.Fatalf("error should name the key_file setting, got: %v", err)
			}
		})
	}
}

// TestInit_ReturnsKeyFileError checks that a bad key_file fails Init with an
// error, before the plugin tries to reach Yandex Cloud.
func TestInit_ReturnsKeyFileError(t *testing.T) {
	g := &InstanceGroup{KeyFile: filepath.Join(t.TempDir(), "missing.json")}

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err == nil || !strings.Contains(err.Error(), "key_file") {
		t.Fatalf("expected a key_file error from Init, got: %v", err)
	}
}

// newPrivateKeyPEM returns a fresh RSA key in the shape `yc iam key create`
// writes: a PKCS#8 PEM block preceded by the "PLEASE DO NOT REMOVE THIS LINE!"
// line current yc versions put in front of it.
func newPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshalling RSA key: %v", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return "PLEASE DO NOT REMOVE THIS LINE! Yandex.Cloud SA Key ID <ajekeyid>\n" + string(block)
}

func keyJSON(t *testing.T, fields map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshalling key fixture: %v", err)
	}
	return string(raw)
}

func writeServiceAccountKey(t *testing.T, fields map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(path, []byte(keyJSON(t, fields)), 0o600); err != nil {
		t.Fatalf("writing key fixture: %v", err)
	}
	return path
}

func decodeJWTSegment(t *testing.T, segment string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decoding JWT segment %q: %v", segment, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parsing JWT segment %q: %v", raw, err)
	}
	return out
}

// TestCredentials_WarnsWhenKeyFileIsTooOpen checks that a key_file readable by
// group/other produces a warning but still works, and that a 0600 file stays
// silent.
func TestCredentials_WarnsWhenKeyFileIsTooOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on Windows")
	}

	path := writeServiceAccountKey(t, map[string]string{
		"id":                 "ajekeyid",
		"service_account_id": "ajesaid",
		"key_algorithm":      "RSA_2048",
		"private_key":        newPrivateKeyPEM(t),
	})

	cases := []struct {
		name     string
		mode     os.FileMode
		wantWarn bool
	}{
		{name: "0600 is silent", mode: 0o600},
		{name: "0640 warns", mode: 0o640, wantWarn: true},
		{name: "0644 warns", mode: 0o644, wantWarn: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}

			var out bytes.Buffer
			g := &InstanceGroup{
				KeyFile: path,
				logger:  hclog.New(&hclog.LoggerOptions{Output: &out, Level: hclog.Warn}),
			}

			if _, err := g.credentials(); err != nil {
				t.Fatalf("credentials() must not fail on a permissive mode: %v", err)
			}

			warned := strings.Contains(out.String(), "key_file is readable by group or other")
			if warned != tc.wantWarn {
				t.Fatalf("warned = %v, want %v; log: %q", warned, tc.wantWarn, out.String())
			}
		})
	}
}
