package config

import (
	"fmt"
	"path/filepath"
	"testing"

	"persea-terminal/internal/proto"
)

func imageFrontConfig(extra string) string {
	return `{"realms":[{"name":"r","socket":"/tmp/b.sock","broker_uid":0}],"alias_store_path":"/tmp/aliases.json"` + extra + `}`
}
func imageBrokerConfig(realm, extra string) string {
	return `{"realm":"` + realm + `","front_uid":0,"servers":[{"label":"s","socket_path":"/tmp/tmux.sock"}]` + extra + `}`
}

func TestImageConfigDisabledRepresentations(t *testing.T) {
	for name, body := range map[string]string{
		"absent": imageFrontConfig(""),
		"zero":   imageFrontConfig(`,"image_upload_max_bytes":0`),
		"null":   imageFrontConfig(`,"image_upload_max_bytes":null`),
	} {
		t.Run("front_"+name, func(t *testing.T) {
			got, err := LoadFront(write(t, body))
			if err != nil {
				t.Fatalf("disabled front config rejected: %v", err)
			}
			if got.ImageUploadMaxBytes != 0 {
				t.Fatalf("ImageUploadMaxBytes = %d, want disabled zero", got.ImageUploadMaxBytes)
			}
		})
	}
	for name, body := range map[string]string{
		"absent": imageBrokerConfig("r", ""),
		"empty":  imageBrokerConfig("r", `,"image_staging_dir":""`),
		"null":   imageBrokerConfig("r", `,"image_staging_dir":null`),
	} {
		t.Run("broker_"+name, func(t *testing.T) {
			got, err := LoadBroker(write(t, body))
			if err != nil {
				t.Fatalf("disabled broker config rejected: %v", err)
			}
			if got.ImageStagingDir != "" {
				t.Fatalf("ImageStagingDir = %q, want disabled empty string", got.ImageStagingDir)
			}
		})
	}
}

func TestImageUploadMaxBytesUsesProtoMaxImage(t *testing.T) {
	for name, value := range map[string]int{
		"minimum": 1 << 20,
		"maximum": proto.MaxImage,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := LoadFront(write(t, imageFrontConfig(fmt.Sprintf(`,"image_upload_max_bytes":%d`, value))))
			if err != nil {
				t.Fatalf("valid image_upload_max_bytes rejected: %v", err)
			}
			if got.ImageUploadMaxBytes != value {
				t.Fatalf("ImageUploadMaxBytes = %d, want %d", got.ImageUploadMaxBytes, value)
			}
		})
	}
	for name, value := range map[string]int{
		"negative":       -1,
		"below_minimum":  (1 << 20) - 1,
		"above_wire_cap": proto.MaxImage + 1,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadFront(write(t, imageFrontConfig(fmt.Sprintf(`,"image_upload_max_bytes":%d`, value)))); err == nil {
				t.Fatalf("accepted image_upload_max_bytes=%d", value)
			}
		})
	}
}

func TestImageStagingRootDefaultIsFrozen(t *testing.T) {
	if DefaultImageStagingRoot != "/var/lib/persea-terminal-staging" {
		t.Fatalf("DefaultImageStagingRoot = %q", DefaultImageStagingRoot)
	}
}

func TestImageStagingPathIsRealmConfined(t *testing.T) {
	realmRoot := filepath.Join(DefaultImageStagingRoot, "r")
	for name, candidate := range map[string]string{
		"realm_root": realmRoot,
		"child":      filepath.Join(realmRoot, "uploads"),
		"nested":     filepath.Join(realmRoot, "uploads", "day-1"),
	} {
		t.Run("accept_"+name, func(t *testing.T) {
			if !imageStagingPath("r", DefaultImageStagingRoot, candidate) {
				t.Fatalf("rejected confined path %q", candidate)
			}
		})
	}
	for name, candidate := range map[string]string{
		"empty":          "",
		"global_root":    DefaultImageStagingRoot,
		"other_realm":    filepath.Join(DefaultImageStagingRoot, "other"),
		"sibling_prefix": filepath.Join(DefaultImageStagingRoot, "r-other"),
		"relative":       filepath.Join("r", "uploads"),
		"unclean_parent": filepath.Join(realmRoot, "uploads") + "/../uploads",
		"unclean_dot":    realmRoot + "/./uploads",
		"trailing_slash": realmRoot + "/",
		"space":          filepath.Join(realmRoot, "has space"),
		"single_quote":   filepath.Join(realmRoot, "has'quote"),
		"double_quote":   filepath.Join(realmRoot, `has"quote`),
		"backslash":      filepath.Join(realmRoot, `has\\slash`),
	} {
		t.Run("reject_"+name, func(t *testing.T) {
			if imageStagingPath("r", DefaultImageStagingRoot, candidate) {
				t.Fatalf("accepted unconfined or unsafe path %q", candidate)
			}
		})
	}
	for _, realm := range []string{"", ".", "../r", "r/child", "r space"} {
		if imageStagingPath(realm, DefaultImageStagingRoot, realmRoot) {
			t.Fatalf("accepted invalid realm %q", realm)
		}
	}
}

func TestImageStagingDirValidationFailsBrokerLoad(t *testing.T) {
	realmRoot := filepath.Join(DefaultImageStagingRoot, "r")
	for name, candidate := range map[string]string{
		"realm_root": realmRoot,
		"child":      filepath.Join(realmRoot, "uploads"),
	} {
		t.Run("accept_"+name, func(t *testing.T) {
			got, err := LoadBroker(write(t, imageBrokerConfig("r", fmt.Sprintf(`,"image_staging_dir":%q`, candidate))))
			if err != nil {
				t.Fatalf("valid image_staging_dir rejected: %v", err)
			}
			if got.ImageStagingDir != candidate {
				t.Fatalf("ImageStagingDir = %q, want %q", got.ImageStagingDir, candidate)
			}
		})
	}
	for name, candidate := range map[string]string{
		"global_root":    DefaultImageStagingRoot,
		"other_realm":    filepath.Join(DefaultImageStagingRoot, "other"),
		"sibling_prefix": filepath.Join(DefaultImageStagingRoot, "r-other"),
		"relative":       filepath.Join("r", "uploads"),
		"unclean":        realmRoot + "/../r",
		"space":          filepath.Join(realmRoot, "has space"),
		"quote":          filepath.Join(realmRoot, `has"quote`),
		"backslash":      filepath.Join(realmRoot, `has\\slash`),
	} {
		t.Run("reject_"+name, func(t *testing.T) {
			if _, err := LoadBroker(write(t, imageBrokerConfig("r", fmt.Sprintf(`,"image_staging_dir":%q`, candidate)))); err == nil {
				t.Fatalf("broker startup accepted image_staging_dir %q", candidate)
			}
		})
	}
}

// TestImageStagingRootOverride pins the host-level staging_root knob: absent
// means the frozen default, a clean absolute path elsewhere is accepted and
// re-scopes the realm confinement, and the forbidden subtrees — the front's
// exact-0700 state directory, /tmp, and the install root — are rejected at
// load for both the broker and the front.
func TestImageStagingRootOverride(t *testing.T) {
	broker, err := LoadBroker(write(t, imageBrokerConfig("r", "")))
	if err != nil || broker.ImageStagingRoot != DefaultImageStagingRoot {
		t.Fatalf("absent broker root normalized to %q err=%v", broker.ImageStagingRoot, err)
	}
	front, err := LoadFront(write(t, imageFrontConfig("")))
	if err != nil || front.ImageStagingRoot != DefaultImageStagingRoot {
		t.Fatalf("absent front root normalized to %q err=%v", front.ImageStagingRoot, err)
	}

	override := "/srv/persea-staging"
	broker, err = LoadBroker(write(t, imageBrokerConfig("r", fmt.Sprintf(`,"image_staging_root":%q,"image_staging_dir":%q`, override, override+"/r"))))
	if err != nil || broker.ImageStagingRoot != override || broker.ImageStagingDir != override+"/r" {
		t.Fatalf("valid override rejected: root=%q dir=%q err=%v", broker.ImageStagingRoot, broker.ImageStagingDir, err)
	}
	front, err = LoadFront(write(t, imageFrontConfig(fmt.Sprintf(`,"image_staging_root":%q`, override))))
	if err != nil || front.ImageStagingRoot != override {
		t.Fatalf("valid front override rejected: root=%q err=%v", front.ImageStagingRoot, err)
	}

	// With an override in force, a staging dir under the DEFAULT root is no
	// longer within the realm staging root and must fail load.
	if _, err := LoadBroker(write(t, imageBrokerConfig("r", fmt.Sprintf(`,"image_staging_root":%q,"image_staging_dir":%q`, override, DefaultImageStagingRoot+"/r")))); err == nil {
		t.Fatal("an overridden root accepted a default-root staging dir")
	}

	for name, root := range map[string]string{
		"front_state_root":  "/var/lib/persea-terminal",
		"front_state_child": "/var/lib/persea-terminal/staging",
		"tmp_root":          "/tmp",
		"tmp_child":         "/tmp/persea-staging",
		"install_root":      "/opt/persea-terminal",
		"install_child":     "/opt/persea-terminal/staging",
		"relative":          "srv/persea-staging",
		"unclean":           "/srv/../srv/persea-staging",
		"trailing_slash":    "/srv/persea-staging/",
		"filesystem_root":   "/",
		"space":             "/srv/has space",
		"quote":             `/srv/has"quote`,
	} {
		t.Run("reject_"+name, func(t *testing.T) {
			if _, err := LoadBroker(write(t, imageBrokerConfig("r", fmt.Sprintf(`,"image_staging_root":%q`, root)))); err == nil {
				t.Fatalf("broker accepted image_staging_root %q", root)
			}
			if _, err := LoadFront(write(t, imageFrontConfig(fmt.Sprintf(`,"image_staging_root":%q`, root)))); err == nil {
				t.Fatalf("front accepted image_staging_root %q", root)
			}
		})
	}

	// A sibling of the forbidden parents is legitimate — the default itself
	// is a sibling of the front state directory.
	for _, root := range []string{DefaultImageStagingRoot, "/var/lib/persea-terminal-extra", "/opt/persea-terminal-data"} {
		if _, err := LoadBroker(write(t, imageBrokerConfig("r", fmt.Sprintf(`,"image_staging_root":%q`, root)))); err != nil {
			t.Fatalf("broker rejected sibling root %q: %v", root, err)
		}
	}
}

func TestImageConfigSchemaRemainsClosed(t *testing.T) {
	if _, err := LoadFront(write(t, imageFrontConfig(`,"image_upload_max_byte":1048576`))); err == nil {
		t.Fatal("front config accepted unknown image key")
	}
	if _, err := LoadBroker(write(t, imageBrokerConfig("r", `,"image_staging_roots":"/srv/x"`))); err == nil {
		t.Fatal("broker config accepted unknown image key")
	}
}
