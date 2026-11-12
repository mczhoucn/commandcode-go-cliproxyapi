package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Update only the selected file's email, preserving its identity, disabled
// state and all host metadata. Do not call host.auth.save for legacy records:
// their JSON id can differ from the host's path-derived id, creating duplicates.
// Best effort only: the host does not expose an atomic conditional update.
func (m *Manager) syncAccountEmail(_ context.Context, entry pluginapi.HostAuthFileEntry, key, email string) error {
	email = strings.TrimSpace(email)
	path := strings.TrimSpace(entry.Path)
	if email == "" || path == "" || !strings.HasSuffix(path, ".json") {
		return fmt.Errorf("account email sync unavailable")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("account email sync failed")
	}
	if !gjson.ValidBytes(raw) || strings.TrimSpace(gjson.GetBytes(raw, "api_key").String()) != key {
		return fmt.Errorf("account email sync skipped: credential changed")
	}
	if gjson.GetBytes(raw, "email").String() == email {
		return nil
	}
	merged, err := sjson.SetBytes(raw, "email", email)
	if err != nil {
		return fmt.Errorf("account email sync failed")
	}
	// The non-JSON temp file is ignored by the directory watcher. Only the
	// complete replacement is observed, so the watcher cannot parse a partial file.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".commandcode-email-*.tmp")
	if err != nil {
		return fmt.Errorf("account email sync failed")
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(merged); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("account email sync failed")
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("account email sync failed")
	}
	// Re-check after preparing the replacement. This detects prior deletions or
	// edits, but a host change between this read and Rename can still be lost.
	// Keeping email visible on the host page retains this known limitation.
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(raw) {
		return fmt.Errorf("account email sync skipped: credential changed")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("account email sync failed")
	}
	return nil
}
